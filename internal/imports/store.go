package imports

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned for an unknown job id.
var ErrNotFound = errors.New("imports: job not found")

// ErrState is returned when a transition is not allowed from the job's state.
var ErrState = errors.New("imports: invalid state for this action")

func nowStr() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

const jobCols = `id, status, phase, filename, format, format_detected, file_path, file_size, file_sha256,
 options_json, rows_seen, parsed_count, skipped_count, error_count, new_count, duplicate_count,
 unmatched_count, inserted_count, preview_json, result_json, error, created_at, updated_at, started_at, finished_at,
 awaiting_files`

type rowScanner interface{ Scan(...any) error }

func scanJob(s rowScanner) (Job, error) {
	var j Job
	var fp, prev, res, errS, started, finished sql.NullString
	var opts string
	var det, awaiting int
	if err := s.Scan(&j.ID, &j.Status, &j.Phase, &j.Filename, &j.Format, &det, &fp, &j.FileSize, &j.FileSHA256,
		&opts, &j.Progress.RowsSeen, &j.Progress.Parsed, &j.Progress.Skipped, &j.Progress.Errors, &j.Progress.New,
		&j.Progress.Duplicate, &j.Progress.Unmatched, &j.Progress.Inserted, &prev, &res, &errS,
		&j.CreatedAt, &j.UpdatedAt, &started, &finished, &awaiting); err != nil {
		return Job{}, err
	}
	if awaiting == 1 && j.Status == StatusQueued {
		j.Status = StatusAwaitingFiles
	}
	j.FormatDetected = det == 1
	j.filePath = fp.String
	_ = json.Unmarshal([]byte(opts), &j.Options)
	if prev.Valid {
		j.Preview = json.RawMessage(prev.String)
	}
	if res.Valid {
		j.Result = json.RawMessage(res.String)
	}
	j.Error, j.StartedAt, j.FinishedAt = errS.String, started.String, finished.String
	return j, nil
}

// Store persists jobs. Reads use Read, writes use Write (single connection).
type Store struct{ Read, Write *sql.DB }

func (s *Store) Get(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.Read.QueryRowContext(ctx, `SELECT `+jobCols+` FROM import_job WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	err = fillFiles(ctx, s.Read, &j)
	return j, err
}

// getW reads through the write connection (read-your-writes).
func (s *Store) getW(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.Write.QueryRowContext(ctx, `SELECT `+jobCols+` FROM import_job WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	err = fillFiles(ctx, s.Write, &j)
	return j, err
}

// fillFiles loads a multi-file job's files and says which required ones are
// present. Single-file jobs have none.
func fillFiles(ctx context.Context, db *sql.DB, j *Job) error {
	defs := requiredFiles(j.Format)
	if defs == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id, role, filename, entry, COALESCE(file_path,''), file_size, file_sha256
		FROM import_job_file WHERE job_id=? ORDER BY id`, j.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f fileRef
		if err := rows.Scan(&f.id, &f.role, &f.filename, &f.entry, &f.path, &f.size, &f.sha); err != nil {
			return err
		}
		j.fileRefs = append(j.fileRefs, f)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range defs {
		jf := JobFile{Role: d.Role, Label: d.Label, Filename: joinNames(j.fileRefs, d.Role)}
		for _, f := range j.fileRefs {
			if f.role == d.Role {
				jf.Present, jf.SHA256 = true, f.sha
				jf.Size += f.size
			}
		}
		j.Files = append(j.Files, jf)
		if !jf.Present {
			j.Missing = append(j.Missing, d.Role)
		}
	}
	return nil
}

func (s *Store) List(ctx context.Context, limit int) ([]Job, error) {
	rows, err := s.Read.QueryContext(ctx, `SELECT `+jobCols+` FROM import_job ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err := fillFiles(ctx, s.Read, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) Create(ctx context.Context, j Job, path string) (int64, error) {
	opts, _ := json.Marshal(j.Options)
	det := 0
	if j.FormatDetected {
		det = 1
	}
	res, err := s.Write.ExecContext(ctx,
		`INSERT INTO import_job (status, phase, filename, format, format_detected, file_path, file_size, file_sha256, options_json)
		 VALUES ('queued','preview',?,?,?,?,?,?,?)`,
		j.Filename, j.Format, det, path, j.FileSize, j.FileSHA256, string(opts))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// transition moves a job between states atomically: it only applies when the
// current status is one of from.
func (s *Store) transition(ctx context.Context, id int64, to string, extra string, args []any, from ...string) error {
	q := `UPDATE import_job SET status=?, updated_at=?` + extra + ` WHERE id=? AND status IN (`
	a := append([]any{to, nowStr()}, args...)
	a = append(a, id)
	for i, f := range from {
		if i > 0 {
			q += ","
		}
		q += "?"
		a = append(a, f)
	}
	q += ")"
	res, err := s.Write.ExecContext(ctx, q, a...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.getW(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("%w (job %d is not in %v)", ErrState, id, from)
	}
	return nil
}

// Confirm queues the apply phase of a previewed job.
func (s *Store) Confirm(ctx context.Context, id int64) error {
	return s.transition(ctx, id, StatusQueued,
		`, phase='apply', error=NULL, rows_seen=0, parsed_count=0, skipped_count=0, error_count=0, new_count=0, duplicate_count=0, unmatched_count=0, inserted_count=0`,
		nil, StatusAwaiting)
}

// ConfirmWhenReady confirms a previewed job now, or, while its files are still
// awaited or being read, marks it so the apply phase is queued as soon as the
// preview finishes. It reports whether the job was confirmed straight away.
func (s *Store) ConfirmWhenReady(ctx context.Context, id int64) (bool, error) {
	err := s.Confirm(ctx, id)
	if err == nil || !errors.Is(err, ErrState) {
		return err == nil, err
	}
	res, err := s.Write.ExecContext(ctx,
		`UPDATE import_job SET auto_confirm=1, updated_at=? WHERE id=? AND phase='preview' AND status IN ('queued','running')`,
		nowStr(), id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The preview may have finished between the two statements.
		if err := s.Confirm(ctx, id); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// autoConfirmed reports whether the admin asked for job id to apply as soon as
// its preview is done.
func (s *Store) autoConfirmed(ctx context.Context, id int64) bool {
	var v int
	err := s.Write.QueryRowContext(ctx, `SELECT auto_confirm FROM import_job WHERE id=?`, id).Scan(&v)
	return err == nil && v == 1
}

// Retry re-queues a failed job at its current phase.
func (s *Store) Retry(ctx context.Context, id int64) error {
	j, err := s.getW(ctx, id)
	if err != nil {
		return err
	}
	if !j.filesOnDisk() {
		return fmt.Errorf("%w (upload no longer on disk)", ErrState)
	}
	return s.transition(ctx, id, StatusQueued,
		`, error=NULL, cancel_requested=0, finished_at=NULL, rows_seen=0, parsed_count=0, skipped_count=0, error_count=0, new_count=0, duplicate_count=0, unmatched_count=0, inserted_count=0`,
		nil, StatusFailed)
}

// Cancel cancels a job that has not finished. A running job is flagged and
// stops at its next checkpoint (the manager also cancels its context).
func (s *Store) Cancel(ctx context.Context, id int64) error {
	return s.transition(ctx, id, StatusCancelled, `, cancel_requested=1, finished_at=?`, []any{nowStr()},
		StatusQueued, StatusRunning, StatusAwaiting)
}

func (s *Store) setFilePath(ctx context.Context, id int64, p any) error {
	_, err := s.Write.ExecContext(ctx, `UPDATE import_job SET file_path=? WHERE id=?`, p, id)
	return err
}

// clearFiles forgets every stored file of a job (they have been deleted).
func (s *Store) clearFiles(ctx context.Context, id int64) error {
	if err := s.setFilePath(ctx, id, nil); err != nil {
		return err
	}
	_, err := s.Write.ExecContext(ctx, `UPDATE import_job_file SET file_path=NULL WHERE job_id=?`, id)
	return err
}

// ErrAlreadyPresent is returned when a file's role is already filled.
var ErrAlreadyPresent = errors.New("imports: that file is already present")

// CreateMulti creates a multi-file job from the first upload. The job waits
// (awaiting=true) until every required file is present.
func (s *Store) CreateMulti(ctx context.Context, j Job, files []fileRef, awaiting bool) (int64, error) {
	opts, _ := json.Marshal(j.Options)
	det, aw := 0, 0
	if j.FormatDetected {
		det = 1
	}
	if awaiting {
		aw = 1
	}
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO import_job (status, phase, filename, format, format_detected, file_path, file_size, file_sha256, options_json, awaiting_files)
		 VALUES ('queued','preview',?,?,?,NULL,?,?,?,?)`,
		j.Filename, j.Format, det, j.FileSize, j.FileSHA256, string(opts), aw)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_job_file (job_id, role, filename, entry, file_path, file_size, file_sha256)
			VALUES (?,?,?,?,?,?,?)`, id, f.role, f.filename, f.entry, f.path, f.size, f.sha); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// AddFiles stores more files on a job that is waiting for them. A role that is
// already filled is refused unless replace is set (then the old file is
// dropped). When the last required file arrives the job is released to the
// worker. It returns the paths of files that are no longer referenced, for the
// caller to delete.
func (s *Store) AddFiles(ctx context.Context, id int64, files []fileRef, replace bool) (orphans []string, complete bool, err error) {
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var format string
	var awaiting int
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status, awaiting_files, format FROM import_job WHERE id=?`, id).Scan(&status, &awaiting, &format); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, err
	}
	if status != StatusQueued || awaiting != 1 {
		return nil, false, fmt.Errorf("%w (job %d is not waiting for files)", ErrState, id)
	}
	have := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT role FROM import_job_file WHERE job_id=?`, id)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, false, err
		}
		have[r] = true
	}
	rows.Close()
	var oldPaths []string
	for _, f := range files {
		if !have[f.role] {
			continue
		}
		if !replace {
			return nil, false, fmt.Errorf("%w (%s)", ErrAlreadyPresent, f.role)
		}
		pr, err := tx.QueryContext(ctx, `SELECT COALESCE(file_path,'') FROM import_job_file WHERE job_id=? AND role=?`, id, f.role)
		if err != nil {
			return nil, false, err
		}
		for pr.Next() {
			var p string
			if err := pr.Scan(&p); err != nil {
				pr.Close()
				return nil, false, err
			}
			oldPaths = append(oldPaths, p)
		}
		pr.Close()
		if _, err := tx.ExecContext(ctx, `DELETE FROM import_job_file WHERE job_id=? AND role=?`, id, f.role); err != nil {
			return nil, false, err
		}
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_job_file (job_id, role, filename, entry, file_path, file_size, file_sha256)
			VALUES (?,?,?,?,?,?,?)`, id, f.role, f.filename, f.entry, f.path, f.size, f.sha); err != nil {
			return nil, false, err
		}
		have[f.role] = true
	}
	complete = true
	for _, d := range requiredFiles(format) {
		if !have[d.Role] {
			complete = false
		}
	}
	if complete {
		// The job is released: describe it by all its files and fix its bundle hash.
		var names []string
		var sums string
		nr, err := tx.QueryContext(ctx, `SELECT DISTINCT filename FROM import_job_file WHERE job_id=? ORDER BY CASE role WHEN 'play_activity' THEN 0 ELSE 1 END, id`, id)
		if err != nil {
			return nil, false, err
		}
		for nr.Next() {
			var n string
			if err := nr.Scan(&n); err != nil {
				nr.Close()
				return nil, false, err
			}
			names = append(names, n)
		}
		nr.Close()
		sr, err := tx.QueryContext(ctx, `SELECT DISTINCT file_sha256 FROM import_job_file WHERE job_id=? ORDER BY CASE role WHEN 'play_activity' THEN 0 ELSE 1 END, id`, id)
		if err != nil {
			return nil, false, err
		}
		for sr.Next() {
			var h string
			if err := sr.Scan(&h); err != nil {
				sr.Close()
				return nil, false, err
			}
			sums += h
		}
		sr.Close()
		bundle := sha256.Sum256([]byte(sums))
		if _, err := tx.ExecContext(ctx, `UPDATE import_job SET awaiting_files=0, filename=?, file_sha256=?,
			file_size=(SELECT COALESCE(SUM(sz),0) FROM (SELECT MAX(file_size) sz FROM import_job_file WHERE job_id=? GROUP BY file_path)), updated_at=? WHERE id=?`,
			strings.Join(names, " + "), hex.EncodeToString(bundle[:]), id, nowStr(), id); err != nil {
			return nil, false, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE import_job SET updated_at=? WHERE id=?`, nowStr(), id); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	for _, p := range oldPaths { // not shared with a file that stays
		if p == "" {
			continue
		}
		var n int
		if s.Write.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_job_file WHERE file_path=?`, p).Scan(&n) == nil && n == 0 {
			orphans = append(orphans, p)
		}
	}
	return orphans, complete, nil
}

// undo deletes the job's scrobbles and the job in one transaction.
func (s *Store) undo(ctx context.Context, id int64) (int64, error) {
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM scrobble WHERE import_job_id=?`, id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM import_job WHERE id=?`, id); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func (s *Store) delete(ctx context.Context, id int64) error {
	_, err := s.Write.ExecContext(ctx, `DELETE FROM import_job WHERE id=?`, id)
	return err
}

// claimNext flips the oldest queued job to running.
func (s *Store) claimNext(ctx context.Context) (Job, bool, error) {
	var id int64
	// A multi-file job that is still waiting for a file is never claimed.
	err := s.Write.QueryRowContext(ctx, `SELECT id FROM import_job WHERE status='queued' AND awaiting_files=0 ORDER BY id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	now := nowStr()
	if _, err := s.Write.ExecContext(ctx,
		`UPDATE import_job SET status='running', started_at=COALESCE(started_at, ?), updated_at=? WHERE id=? AND status='queued' AND awaiting_files=0`,
		now, now, id); err != nil {
		return Job{}, false, err
	}
	j, err := s.getW(ctx, id)
	return j, err == nil, err
}

func (s *Store) setProgress(ctx context.Context, id int64, p Progress) error {
	_, err := s.Write.ExecContext(ctx,
		`UPDATE import_job SET rows_seen=?, parsed_count=?, skipped_count=?, error_count=?, new_count=?,
		 duplicate_count=?, unmatched_count=?, inserted_count=?, updated_at=? WHERE id=? AND status='running'`,
		p.RowsSeen, p.Parsed, p.Skipped, p.Errors, p.New, p.Duplicate, p.Unmatched, p.Inserted, nowStr(), id)
	return err
}

func (s *Store) finishPreview(ctx context.Context, id int64, p Progress, preview []byte) error {
	res, err := s.Write.ExecContext(ctx,
		`UPDATE import_job SET status='awaiting_confirmation', rows_seen=?, parsed_count=?, skipped_count=?, error_count=?,
		 new_count=?, duplicate_count=?, unmatched_count=?, preview_json=?, updated_at=? WHERE id=? AND status='running'`,
		p.RowsSeen, p.Parsed, p.Skipped, p.Errors, p.New, p.Duplicate, p.Unmatched, string(preview), nowStr(), id)
	return noRows(res, err)
}

func (s *Store) finishApply(ctx context.Context, id int64, p Progress, result []byte) error {
	now := nowStr()
	res, err := s.Write.ExecContext(ctx,
		`UPDATE import_job SET status='done', rows_seen=?, parsed_count=?, skipped_count=?, error_count=?,
		 new_count=?, duplicate_count=?, unmatched_count=?, inserted_count=?, result_json=?, updated_at=?, finished_at=?
		 WHERE id=? AND status='running'`,
		p.RowsSeen, p.Parsed, p.Skipped, p.Errors, p.New, p.Duplicate, p.Unmatched, p.Inserted, string(result), now, now, id)
	return noRows(res, err)
}

func noRows(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrState // cancelled while running
	}
	return nil
}

func (s *Store) fail(ctx context.Context, id int64, msg string) {
	now := nowStr()
	_, _ = s.Write.ExecContext(ctx,
		`UPDATE import_job SET status='failed', error=?, updated_at=?, finished_at=? WHERE id=? AND status='running'`,
		msg, now, now, id)
}

// RecoverInterrupted marks jobs left 'running' by a crash or restart as
// failed. Both phases are safe to Retry: preview has no side effects and
// apply is idempotent (its own earlier rows dedupe as exact matches).
func (s *Store) RecoverInterrupted(ctx context.Context) (int, error) {
	now := nowStr()
	res, err := s.Write.ExecContext(ctx,
		`UPDATE import_job SET status='failed', error='interrupted by restart; retry to resume', updated_at=?, finished_at=?
		 WHERE status='running'`, now, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) isCancelled(ctx context.Context, id int64) bool {
	var st string
	err := s.Write.QueryRowContext(ctx, `SELECT status FROM import_job WHERE id=?`, id).Scan(&st)
	return err != nil || st != StatusRunning
}
