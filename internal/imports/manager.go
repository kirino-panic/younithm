package imports

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// Options configures the Manager.
type Options struct {
	Dir          string        // uploads live in Dir (created on demand)
	LiveWindow   time.Duration // ingest dedupe window; imports never go below it
	ImportWindow time.Duration // default import-time dedupe window
	MaxBytes     int64         // upload size cap
	DebugLimit   func() int    // debug: when it returns N > 0, imports use only the newest N listens
	// OnDone runs after a job finishes importing (matching starts from here).
	OnDone func()
	// Backup snapshots the database before an import is undone; nil skips it.
	Backup func(tag string) (string, error)
}

// Manager owns the job store, the single background worker and cancellation.
// One import runs at a time: SQLite has a single writer anyway.
type Manager struct {
	store *Store
	opt   Options

	wake chan struct{}
	mu   sync.Mutex
	run  map[int64]context.CancelFunc
}

// NewManager builds a Manager. Call Start to launch the worker.
func NewManager(read, write *sql.DB, o Options) *Manager {
	if o.LiveWindow <= 0 {
		o.LiveWindow = 2 * time.Second
	}
	if o.ImportWindow <= 0 {
		o.ImportWindow = 30 * time.Second
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 1 << 30
	}
	return &Manager{store: &Store{Read: read, Write: write}, opt: o,
		wake: make(chan struct{}, 1), run: map[int64]context.CancelFunc{}}
}

// Store exposes the job store (tests, admin tooling).
func (m *Manager) Store() *Store { return m.store }

// EffectiveWindow resolves a requested dedupe window (0 = default) against
// the rules: never below the live window, at most one hour.
func (m *Manager) EffectiveWindow(req time.Duration) time.Duration {
	w := req
	if w <= 0 {
		w = m.opt.ImportWindow
	}
	if w < m.opt.LiveWindow {
		w = m.opt.LiveWindow
	}
	if w > time.Hour {
		w = time.Hour
	}
	return w
}

// Start recovers jobs interrupted by a restart (marked failed, retryable)
// and runs the worker until ctx ends.
func (m *Manager) Start(ctx context.Context) error {
	n, err := m.store.RecoverInterrupted(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("imports: %d job(s) were interrupted by a restart; marked failed (retry resumes them)", n)
	}
	go m.loop(ctx)
	m.Wake()
	return nil
}

// Wake tells the worker to look for queued jobs.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) loop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second) // safety net if a wake is missed
	defer tick.Stop()
	for {
		for {
			if ctx.Err() != nil {
				return
			}
			j, ok, err := m.store.claimNext(ctx)
			if err != nil {
				log.Printf("imports: claiming job: %v", err)
				break
			}
			if !ok {
				break
			}
			m.runJob(ctx, j)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-tick.C:
		}
	}
}

func (m *Manager) runJob(root context.Context, j Job) {
	ctx, cancel := context.WithCancel(root)
	m.mu.Lock()
	m.run[j.ID] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.run, j.ID)
		m.mu.Unlock()
	}()

	err := m.execute(ctx, j)
	if root.Err() != nil {
		return // shutting down: stays 'running', recovered as failed on next start
	}
	switch {
	case err == nil:
	case errors.Is(err, errCancelled), errors.Is(err, ErrState), errors.Is(err, context.Canceled):
		// Cancel() already set the status.
	default:
		log.Printf("imports: job %d failed: %v", j.ID, err)
		m.store.fail(context.Background(), j.ID, err.Error())
	}
	// Uploads are kept only while they can still be used.
	if cur, gerr := m.store.getW(context.Background(), j.ID); gerr == nil && (cur.Status == StatusDone || cur.Status == StatusCancelled) {
		m.dropFile(cur)
		if cur.Status == StatusDone && m.opt.OnDone != nil {
			m.opt.OnDone()
		}
	}
}

func (m *Manager) dropFile(j Job) {
	if len(j.fileRefs) > 0 { // a multi-file job: every file, once each (zip entries share one)
		seen := map[string]bool{}
		for _, f := range j.fileRefs {
			if f.path != "" && !seen[f.path] {
				seen[f.path] = true
				_ = os.Remove(f.path)
			}
		}
		_ = m.store.clearFiles(context.Background(), j.ID)
		return
	}
	if j.filePath == "" {
		return
	}
	_ = os.Remove(j.filePath)
	_ = m.store.setFilePath(context.Background(), j.ID, nil)
}

func (m *Manager) execute(ctx context.Context, j Job) error {
	if requiredFiles(j.Format) != nil {
		// Scan, join, preview and apply all start from here: none of them runs
		// unless every required file is stored.
		if err := j.requireFiles(); err != nil {
			return err
		}
		if j.Phase == PhaseApply {
			// Confirm applies exactly the bytes that were previewed.
			seen := map[string]bool{}
			for _, f := range j.fileRefs {
				if seen[f.path] {
					continue
				}
				seen[f.path] = true
				sum, err := hashFile(f.path)
				if err != nil {
					return err
				}
				if sum != f.sha {
					return fmt.Errorf("%s changed since the preview; refusing to apply", f.filename)
				}
			}
		}
	} else {
		if j.filePath == "" {
			return errors.New("upload file is gone")
		}
		if j.Phase == PhaseApply {
			// Confirm applies exactly the bytes that were previewed.
			sum, err := hashFile(j.filePath)
			if err != nil {
				return err
			}
			if sum != j.FileSHA256 {
				return errors.New("upload file changed since the preview; refusing to apply")
			}
		}
	}
	e := newPass(m, j, j.Phase == PhaseApply)
	sum, p, err := e.run(ctx)
	if err != nil {
		return err
	}
	if j.Phase == PhasePreview {
		if p.Parsed == 0 {
			msg := fmt.Sprintf("no valid listens found (%d rows, %d errors, %d skipped)", p.RowsSeen, p.Errors, p.Skipped)
			if len(sum.ErrorsSample) > 0 {
				msg += "; first error: " + sum.ErrorsSample[0].Error
			}
			return errors.New(msg)
		}
		b, _ := json.Marshal(sum)
		if err := m.store.finishPreview(ctx, j.ID, p, b); err != nil {
			return err
		}
		if m.store.autoConfirmed(ctx, j.ID) {
			if err := m.store.Confirm(ctx, j.ID); err != nil {
				return err
			}
			m.Wake()
		}
		return nil
	}
	if len(j.Preview) > 0 {
		var prev Summary
		if json.Unmarshal(j.Preview, &prev) == nil {
			sum.Drift = &Drift{PreviewNew: prev.Counts.New, PreviewDuplicate: prev.Counts.Duplicate,
				AppliedNew: p.New, AppliedDuplicate: p.Duplicate}
		}
	}
	b, _ := json.Marshal(sum)
	return m.store.finishApply(ctx, j.ID, p, b)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Confirm queues the apply phase of a previewed job.
func (m *Manager) Confirm(ctx context.Context, id int64) error {
	if err := m.store.Confirm(ctx, id); err != nil {
		return err
	}
	m.Wake()
	return nil
}

// ConfirmWhenReady confirms a job, or has it apply as soon as its preview is
// done if that is still to come.
func (m *Manager) ConfirmWhenReady(ctx context.Context, id int64) error {
	ok, err := m.store.ConfirmWhenReady(ctx, id)
	if ok {
		m.Wake()
	}
	return err
}

// Retry re-queues a failed job.
func (m *Manager) Retry(ctx context.Context, id int64) error {
	if err := m.store.Retry(ctx, id); err != nil {
		return err
	}
	m.Wake()
	return nil
}

// Cancel stops a job. Scrobbles already committed by a running apply stay
// (batches of up to 500; re-running the same file later is idempotent).
func (m *Manager) Cancel(ctx context.Context, id int64) error {
	if err := m.store.Cancel(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	cancel := m.run[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel() // runJob drops the file once the worker has stopped
		return nil
	}
	if j, err := m.store.getW(ctx, id); err == nil {
		m.dropFile(j)
	}
	return nil
}

// Undo removes every listen a finished import added, then the job itself, so
// the file can be imported again. The database is backed up first. Recordings
// and releases the import created stay: a re-import finds them again, with
// any links and fixes made since.
func (m *Manager) Undo(ctx context.Context, id int64) (int64, error) {
	j, err := m.store.getW(ctx, id)
	if err != nil {
		return 0, err
	}
	if j.Status != StatusDone && j.Status != StatusFailed {
		return 0, fmt.Errorf("%w (only a finished import can be undone)", ErrState)
	}
	if m.opt.Backup != nil {
		if _, err := m.opt.Backup("pre-undo-import"); err != nil {
			return 0, fmt.Errorf("backup before undo: %w", err)
		}
	}
	n, err := m.store.undo(ctx, id)
	if err != nil {
		return 0, err
	}
	m.dropFile(j)
	return n, nil
}

// Delete removes a finished job record and its upload. Scrobbles it created
// are kept (they carry import_job_id until the job row is deleted, then NULL).
func (m *Manager) Delete(ctx context.Context, id int64) error {
	j, err := m.store.getW(ctx, id)
	if err != nil {
		return err
	}
	if !j.terminal() {
		return fmt.Errorf("%w (cancel the job first)", ErrState)
	}
	m.dropFile(j)
	return m.store.delete(ctx, id)
}
