package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // same pure-Go driver as internal/db
)

// BackfillReport is the job's running totals.
type BackfillReport struct {
	Processed   int `json:"processed"`
	NewlyLinked int `json:"newly_linked"` // scrobbles that gained a recording
	Skipped     int `json:"skipped"`      // no usable title
	Report
}

// BackfillStatus is one backfill job.
type BackfillStatus struct {
	ID           int64          `json:"id"`
	Status       string         `json:"status"` // running, done, failed, cancelled
	DryRun       bool           `json:"dry_run"`
	LastScrobble int64          `json:"last_scrobble_id"`
	Total        int64          `json:"total"`
	Report       BackfillReport `json:"report"`
	Error        string         `json:"error,omitempty"`
	StartedAt    string         `json:"started_at"`
	FinishedAt   string         `json:"finished_at,omitempty"`
}

// ErrBackfillRunning is returned when a job is already running.
var ErrBackfillRunning = errors.New("identity: a backfill is already running")

const backfillBatch = 500

// Backfill re-resolves existing scrobbles from raw_payload: it keeps every
// existing row and attaches credits, ids and join phrases to it, creating
// entities only for scrobbles that have none. A dry run does the same on a
// private copy of the database and discards it, so the counts are exact.
type Backfill struct {
	db   *sql.DB         // write pool
	path string          // database file, for the dry-run copy
	bg   context.Context // lifetime of jobs started over HTTP

	mu     sync.Mutex
	cancel context.CancelFunc
}

// NewBackfill returns a manager. bg bounds jobs started over HTTP (the
// server's lifetime context).
func NewBackfill(bg context.Context, write *sql.DB, dbPath string) *Backfill {
	return &Backfill{db: write, path: dbPath, bg: bg}
}

// Recover marks jobs left running by a restart as failed (resume continues them).
func (b *Backfill) Recover(ctx context.Context) error {
	_, err := b.db.ExecContext(ctx, `UPDATE identity_backfill SET status='failed', error='interrupted by restart',
		finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE status='running'`)
	return err
}

// Start begins a job in the background. resume continues the latest failed or
// cancelled real job from its cursor. bg bounds the job's lifetime.
func (b *Backfill) Start(bg context.Context, dryRun, resume bool) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		return 0, ErrBackfillRunning
	}
	var id, last int64
	var rep BackfillReport
	if resume && !dryRun {
		var counters string
		err := b.db.QueryRowContext(bg, `SELECT id, last_scrobble_id, counters FROM identity_backfill
			WHERE dry_run=0 AND status IN ('failed','cancelled') ORDER BY id DESC LIMIT 1`).Scan(&id, &last, &counters)
		if err == nil {
			_ = json.Unmarshal([]byte(counters), &rep)
			if _, err := b.db.ExecContext(bg, `UPDATE identity_backfill SET status='running', error=NULL, finished_at=NULL WHERE id=?`, id); err != nil {
				return 0, err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		} else {
			id = 0
		}
	}
	if id == 0 {
		res, err := b.db.ExecContext(bg, `INSERT INTO identity_backfill (status, dry_run) VALUES ('running', ?)`, dryRun)
		if err != nil {
			return 0, err
		}
		id, _ = res.LastInsertId()
	}
	ctx, cancel := context.WithCancel(bg)
	b.cancel = cancel
	go func() {
		err := b.run(ctx, id, dryRun, last, rep)
		b.mu.Lock()
		b.cancel = nil
		b.mu.Unlock()
		cancel()
		status, msg := "done", ""
		switch {
		case errors.Is(err, context.Canceled):
			status = "cancelled"
		case err != nil:
			status, msg = "failed", err.Error()
			log.Printf("identity: backfill %d: %v", id, err)
		}
		if _, e := b.db.ExecContext(context.Background(), `UPDATE identity_backfill SET status=?, error=NULLIF(?,''),
			finished_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, status, msg, id); e != nil {
			log.Printf("identity: backfill %d: recording result: %v", id, e)
		}
	}()
	return id, nil
}

// Cancel stops the running job; its cursor is kept so it can be resumed.
func (b *Backfill) Cancel() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel == nil {
		return false
	}
	b.cancel()
	return true
}

// Latest returns the most recent job of the kind (nil when none).
func (b *Backfill) Latest(ctx context.Context, dryRun bool) (*BackfillStatus, error) {
	var s BackfillStatus
	var counters string
	var errS, fin sql.NullString
	err := b.db.QueryRowContext(ctx, `SELECT id, status, dry_run, last_scrobble_id, total, counters, error, started_at, finished_at
		FROM identity_backfill WHERE dry_run=? ORDER BY id DESC LIMIT 1`, dryRun).
		Scan(&s.ID, &s.Status, &s.DryRun, &s.LastScrobble, &s.Total, &counters, &errS, &s.StartedAt, &fin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(counters), &s.Report)
	s.Error, s.FinishedAt = errS.String, fin.String
	return &s, nil
}

func (b *Backfill) run(ctx context.Context, id int64, dryRun bool, last int64, rep BackfillReport) error {
	work := b.db
	if dryRun {
		cp, cleanup, err := b.copyDB(ctx)
		if err != nil {
			return err
		}
		defer cleanup()
		work = cp
	}
	var total int64
	if err := work.QueryRowContext(ctx, `SELECT COUNT(*) FROM scrobble WHERE id>?`, last).Scan(&total); err != nil {
		return err
	}
	if _, err := b.db.ExecContext(ctx, `UPDATE identity_backfill SET total=? WHERE id=?`, total+int64(rep.Processed), id); err != nil {
		return err
	}
	type row struct {
		id                 int64
		artist, title      string
		album, payload     sql.NullString
		recording, release sql.NullInt64
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := work.QueryContext(ctx, `SELECT id, raw_artist, raw_title, raw_album, raw_payload, recording_id, release_id
			FROM scrobble WHERE id>? ORDER BY id LIMIT ?`, last, backfillBatch)
		if err != nil {
			return err
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.artist, &r.title, &r.album, &r.payload, &r.recording, &r.release); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		tx, err := work.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		res := &Resolver{}
		for _, r := range batch {
			rep.Processed++
			in := FromBasics(json.RawMessage(r.payload.String), Basics{
				Artist: r.artist, Title: r.title, Album: r.album.String,
				HintRecordingID: r.recording.Int64, HintReleaseID: r.release.Int64,
			})
			rec, rel, err := res.Resolve(ctx, tx, in)
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("scrobble %d: %w", r.id, err)
			}
			switch {
			case Norm(r.title) == "":
				rep.Skipped++
			case rec != 0 && !r.recording.Valid:
				rep.NewlyLinked++
				if _, err := tx.ExecContext(ctx, `UPDATE scrobble SET recording_id=?, release_id=COALESCE(release_id, ?) WHERE id=?`,
					rec, nullID(rel), r.id); err != nil {
					tx.Rollback()
					return err
				}
			case rel != 0 && !r.release.Valid:
				if _, err := tx.ExecContext(ctx, `UPDATE scrobble SET release_id=? WHERE id=?`, rel, r.id); err != nil {
					tx.Rollback()
					return err
				}
			}
			last = r.id
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		rep.Report.Add(res.Rep)
		counters, _ := json.Marshal(rep)
		if _, err := b.db.ExecContext(ctx, `UPDATE identity_backfill SET last_scrobble_id=?, counters=? WHERE id=?`, last, string(counters), id); err != nil {
			return err
		}
	}
}

func nullID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// copyDB snapshots the database (VACUUM INTO) to a temp file next to it.
func (b *Backfill) copyDB(ctx context.Context) (*sql.DB, func(), error) {
	if b.path == "" {
		return nil, nil, errors.New("identity: database path unknown, cannot preview")
	}
	dir, err := os.MkdirTemp(filepath.Dir(b.path), "backfill-preview-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	cp := filepath.Join(dir, "copy.db")
	if _, err := b.db.ExecContext(ctx, `VACUUM INTO ?`, cp); err != nil {
		cleanup()
		return nil, nil, err
	}
	d, err := sql.Open("sqlite", "file:"+cp+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	d.SetMaxOpenConns(1)
	return d, func() { d.Close(); cleanup() }, nil
}
