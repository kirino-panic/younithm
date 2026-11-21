package imports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"younithm/internal/config"
	"younithm/internal/identity"
	"younithm/internal/importers"
	"younithm/internal/ingest"
)

const (
	batchSize     = 500
	flushInterval = time.Second
	sampleEach    = 10 // sample rows kept per outcome
	errSampleMax  = 20
)

var errCancelled = errors.New("import cancelled")

// Outcomes of one listen.
const (
	outNew       = "new"
	outUnmatched = "new_unmatched"
	outDuplicate = "duplicate"
)

// SampleRow is one example listen with what would happen to it.
type SampleRow struct {
	PlayedAt string `json:"played_at"`
	Artist   string `json:"artist"`
	Title    string `json:"title"`
	Album    string `json:"album,omitempty"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
}

// ErrSample is one unparseable row.
type ErrSample struct {
	Row   int    `json:"row"`
	Error string `json:"error"`
	Raw   string `json:"raw,omitempty"`
}

// DupBreakdown explains the duplicate count.
type DupBreakdown struct {
	InFile               int64 `json:"in_file"`                // identical second+artist+title earlier in the same file
	ExistingExact        int64 `json:"existing_exact"`         // same second+artist+title already stored
	ExistingWithinWindow int64 `json:"existing_within_window"` // same artist+title stored within the window but not the same second
}

// Summary is the frozen preview (and, after apply, the result).
type Summary struct {
	Format     string `json:"format"`
	Filename   string `json:"filename"`
	FileSHA256 string `json:"file_sha256"`
	DateRange  struct {
		Earliest string `json:"earliest,omitempty"`
		Latest   string `json:"latest,omitempty"`
	} `json:"date_range"`
	Counts   Progress         `json:"counts"`
	Dups     DupBreakdown     `json:"duplicate_breakdown"`
	Matching map[string]int64 `json:"matching"`
	Dedupe   struct {
		WindowSec   float64 `json:"window_sec"`
		Explanation string  `json:"explanation"`
	} `json:"dedupe"`
	// DebugLimit is set when only the newest N listens of the file were used.
	DebugLimit int `json:"debug_limit,omitempty"`
	// Apple is what the Play Activity / Daily Tracks join did (Apple Music only).
	Apple        *importers.AppleJoinStats `json:"apple,omitempty"`
	Sample       []SampleRow               `json:"sample"`
	ErrorsSample []ErrSample               `json:"errors_sample,omitempty"`
	// Drift is set on the apply result: how it differs from the preview
	// (the database can change between preview and confirm).
	Drift *Drift `json:"drift,omitempty"`
}

// Drift compares the applied counts with the previewed ones.
type Drift struct {
	PreviewNew       int64 `json:"preview_new"`
	PreviewDuplicate int64 `json:"preview_duplicates"`
	AppliedNew       int64 `json:"applied_new"`
	AppliedDuplicate int64 `json:"applied_duplicates"`
}

func dedupeExplanation(window time.Duration) string {
	return fmt.Sprintf("A listen counts as a duplicate when a stored scrobble has the same artist and title "+
		"(ignoring case and spacing) and a played_at within %s of it; rows imported by this job itself only "+
		"match on the identical second. Identical rows within the file are collapsed first. "+
		"Repeats of a track more than %s apart, or repeated plays inside one file that differ by a second or "+
		"more, are kept.", window, window)
}

// pass is one run over the file: a dry run (apply=false) or the real thing.
type pass struct {
	m     *Manager
	job   Job
	apply bool
	res   *identity.Resolver

	seen map[uint64]struct{}
	p    Progress
	sum  Summary
	errs int64

	nsample map[string]int
	tx      *sql.Tx
	inBatch int
	last    time.Time
}

func newPass(m *Manager, j Job, apply bool) *pass {
	e := &pass{m: m, job: j, apply: apply, res: identity.NewMemoResolver(), seen: map[uint64]struct{}{},
		nsample: map[string]int{}, last: time.Now()}
	e.sum.Format, e.sum.Filename, e.sum.FileSHA256 = j.Format, j.Filename, j.FileSHA256
	e.sum.Matching = map[string]int64{}
	e.sum.Dedupe.WindowSec = j.Options.DedupeWindowSec
	e.sum.Dedupe.Explanation = dedupeExplanation(j.Options.Window())
	return e
}

func (e *pass) sample(c Candidate, outcome, reason string) {
	if e.nsample[outcome] >= sampleEach {
		return
	}
	e.nsample[outcome]++
	e.sum.Sample = append(e.sum.Sample, SampleRow{
		PlayedAt: c.PlayedAt.Format(ingest.TimeFormat), Artist: c.L.Artist, Title: c.L.Title, Album: c.L.Album,
		Outcome: outcome, Reason: reason})
}

func (e *pass) q(ctx context.Context) (dbq, error) {
	if !e.apply {
		return e.m.store.Read, nil
	}
	if e.tx == nil {
		tx, err := e.m.store.Write.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		e.tx = tx
	}
	return e.tx, nil
}

// dupInfo describes why a listen is a duplicate of a stored scrobble.
type dupInfo struct {
	id    int64
	delta time.Duration
	exact bool
}

// findDup looks for a stored scrobble the candidate duplicates.
func findDup(ctx context.Context, q dbq, jobID int64, window time.Duration, c Candidate) (*dupInfo, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, played_at, raw_artist, raw_title, COALESCE(import_job_id,0) FROM scrobble
		  WHERE user_id=1 AND played_at BETWEEN ? AND ?`,
		c.PlayedAt.Add(-window).Format(ingest.TimeFormat), c.PlayedAt.Add(window).Format(ingest.TimeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var best *dupInfo
	for rows.Next() {
		var id, jid int64
		var at, a, t string
		if err := rows.Scan(&id, &at, &a, &t, &jid); err != nil {
			return nil, err
		}
		if ingest.Normalize(a) != c.NArtist || ingest.Normalize(t) != c.NTitle {
			continue
		}
		pt, err := time.Parse(ingest.TimeFormat, at)
		if err != nil {
			continue
		}
		d := pt.Sub(c.PlayedAt)
		exact := d == 0
		if jid == jobID && !exact {
			continue // own earlier rows only dedupe exactly (resumed apply)
		}
		if best == nil || abs(d) < abs(best.delta) {
			best = &dupInfo{id: id, delta: d, exact: exact}
		}
	}
	return best, rows.Err()
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (e *pass) handle(ctx context.Context, l importers.Listen) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c := MapListen(l)
	e.p.Parsed++
	q, err := e.q(ctx)
	if err != nil {
		return err
	}
	_, inFile := e.seen[c.Key]
	e.seen[c.Key] = struct{}{}
	if inFile {
		e.p.Duplicate++
		e.sum.Dups.InFile++
		e.sample(c, outDuplicate, "identical to an earlier row in this file")
		return e.tick(ctx)
	}
	d, err := findDup(ctx, q, e.job.ID, e.job.Options.Window(), c)
	if err != nil {
		return err
	}
	if d != nil {
		e.p.Duplicate++
		reason := fmt.Sprintf("same artist and title as scrobble #%d, %s", d.id, deltaText(d.delta))
		if d.exact {
			e.sum.Dups.ExistingExact++
		} else {
			e.sum.Dups.ExistingWithinWindow++
		}
		e.sample(c, outDuplicate, reason)
		return e.tick(ctx)
	}
	e.p.New++
	in := identityInput(l)
	method := matchPath(in)
	if e.apply {
		created := e.res.Rep.RecordingsCreated
		recID, relID, err := e.res.Resolve(ctx, e.tx, in)
		if err != nil {
			return err
		}
		if recID != 0 {
			// Fuzzy consent only governs listens with no id. A recording made
			// from such a listen without consent is never auto-linked by fuzzy
			// search; any consenting import sets it back.
			switch {
			case method == matchByName && e.job.Options.FuzzyConsent:
				_, err = e.tx.ExecContext(ctx, `UPDATE recording SET fuzzy_consent=1 WHERE id=? AND fuzzy_consent=0`, recID)
			case method == matchByName && e.res.Rep.RecordingsCreated > created:
				_, err = e.tx.ExecContext(ctx, `UPDATE recording SET fuzzy_consent=0 WHERE id=?`, recID)
			}
			if err != nil {
				return err
			}
		}
		if err := e.insert(ctx, c, recID, relID); err != nil {
			return err
		}
		e.p.Inserted++
	}
	e.sum.Matching[method]++
	if method == matchByName {
		e.p.Unmatched++
		e.sample(c, outUnmatched, "no ids: matched by name; fuzzy matching decides the rest")
	} else {
		e.sample(c, outNew, "matched by id")
	}
	return e.tick(ctx)
}

func deltaText(d time.Duration) string {
	if d == 0 {
		return "at the identical second"
	}
	s := d.Round(time.Second).Seconds()
	if s < 0 {
		return fmt.Sprintf("%gs earlier", -s)
	}
	return fmt.Sprintf("%gs later", s)
}

func (e *pass) insert(ctx context.Context, c Candidate, recID, relID int64) error {
	n := c.ToScrobble()
	var payload, album, dur, rec, rel any
	if len(n.RawPayload) > 0 {
		payload = string(n.RawPayload)
	}
	if n.Album != "" {
		album = n.Album
	}
	if n.DurationPlayedMs > 0 {
		dur = n.DurationPlayedMs
	}
	if recID != 0 {
		rec = recID
	}
	if relID != 0 {
		rel = relID
	}
	_, err := e.tx.ExecContext(ctx,
		`INSERT INTO scrobble (user_id, played_at, duration_played_ms, source, recording_id, release_id,
		   raw_artist, raw_title, raw_album, raw_payload, import_job_id)
		 VALUES (1,?,?,?,?,?,?,?,?,?,?)`,
		n.PlayedAt.Format(ingest.TimeFormat), dur, n.Source, rec, rel, n.Artist, n.Title, album, payload, e.job.ID)
	return err
}

func (e *pass) progress() Progress {
	p := e.p
	p.Errors = e.errs
	p.RowsSeen = p.Parsed + p.Errors
	return p
}

// tick flushes progress (and, when applying, commits the batch) on a size or
// time boundary.
func (e *pass) tick(ctx context.Context) error {
	e.inBatch++
	if e.inBatch < batchSize && time.Since(e.last) < flushInterval {
		return nil
	}
	return e.flush(ctx)
}

func (e *pass) flush(ctx context.Context) error {
	e.inBatch, e.last = 0, time.Now()
	p := e.progress()
	if !e.apply {
		if e.m.store.isCancelled(ctx, e.job.ID) {
			return errCancelled
		}
		return e.m.store.setProgress(ctx, e.job.ID, p)
	}
	if e.tx == nil {
		return nil
	}
	var st string
	if err := e.tx.QueryRowContext(ctx, `SELECT status FROM import_job WHERE id=?`, e.job.ID).Scan(&st); err != nil {
		return err
	}
	if st != StatusRunning {
		return errCancelled
	}
	if _, err := e.tx.ExecContext(ctx,
		`UPDATE import_job SET rows_seen=?, parsed_count=?, error_count=?, new_count=?, duplicate_count=?,
		 unmatched_count=?, inserted_count=?, updated_at=? WHERE id=?`,
		p.RowsSeen, p.Parsed, p.Errors, p.New, p.Duplicate, p.Unmatched, p.Inserted, nowStr(), e.job.ID); err != nil {
		return err
	}
	err := e.tx.Commit()
	e.tx = nil
	return err
}

func (e *pass) rollback() {
	if e.tx != nil {
		_ = e.tx.Rollback()
		e.tx = nil
	}
}

// run streams the stored upload through the pass.
func (e *pass) run(ctx context.Context) (Summary, Progress, error) {
	defer e.rollback()
	emit := func(l importers.Listen) error { return e.handle(ctx, l) }
	var keep *newestN
	n := e.job.Options.DebugLimit
	if n <= 0 {
		n = config.DebugImportLimit()
	}
	if n > 0 { // debug only: newest n listens
		e.sum.DebugLimit = n
		keep = &newestN{n: n}
		emit = func(l importers.Listen) error { keep.add(l); return nil }
	}
	onRowErr := func(re importers.RowError) {
		e.errs++
		if len(e.sum.ErrorsSample) < errSampleMax {
			raw := re.Raw
			if len(raw) > 200 {
				raw = raw[:200] + "..."
			}
			e.sum.ErrorsSample = append(e.sum.ErrorsSample, ErrSample{Row: re.Index, Error: re.Err.Error(), Raw: strings.TrimSpace(raw)})
		}
	}
	var res importers.Result
	var err error
	if requiredFiles(e.job.Format) != nil {
		// Several files are joined into one stream; this refuses to start unless
		// every one of them is stored.
		var stats importers.AppleJoinStats
		res, stats, err = ParseApple(ctx, e.job, emit, onRowErr)
		e.sum.Apple = &stats
	} else {
		res, err = ParseFile(ctx, e.job.filePath, e.job.Format, e.job.Options, emit, onRowErr)
	}
	if err != nil {
		return e.sum, e.progress(), err
	}
	if keep != nil {
		for _, l := range keep.sorted() {
			if err := e.handle(ctx, l); err != nil {
				return e.sum, e.progress(), err
			}
		}
	}
	if e.apply {
		if err := e.flush(ctx); err != nil { // commits the last batch
			return e.sum, e.progress(), err
		}
	}
	p := e.progress()
	p.RowsSeen, p.Skipped, p.Errors = int64(res.Rows), int64(res.Skipped), int64(res.Errors)
	e.sum.Counts = p
	if !res.Earliest.IsZero() {
		e.sum.DateRange.Earliest = res.Earliest.UTC().Format(time.RFC3339)
		e.sum.DateRange.Latest = res.Latest.UTC().Format(time.RFC3339)
	}
	return e.sum, p, nil
}
