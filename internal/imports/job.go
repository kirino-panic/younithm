// Package imports is the import job system (docs/imports.md): persisted
// jobs, upload -> parse -> dry-run preview -> confirm -> apply, offline
// matching, and dedupe. It sits on top of the pure parsers in
// internal/importers and writes the same scrobble/recording model that live
// ingest uses. See docs/imports.md.
package imports

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"younithm/internal/importers"
)

// Job statuses (import_job.status).
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusAwaiting  = "awaiting_confirmation"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"

	// StatusAwaitingFiles is what the API reports for a multi-file import that
	// does not have all its files yet. In the database it is 'queued' with
	// awaiting_files=1, and the worker never claims it: nothing is scanned,
	// joined, previewed or applied until every required file is present.
	StatusAwaitingFiles = "awaiting_files"
)

// roleDef is one file an import format needs.
type roleDef struct{ Role, Label string }

// requiredFiles lists the files a format needs before anything runs on it; nil
// for the formats that are a single file. Apple Music's privacy export splits an
// import over two files: Play Activity has the plays with their exact times but
// no artist and no catalog id, Daily Tracks has the catalog id and the artist but
// only the day. Neither is enough alone.
func requiredFiles(format string) []roleDef {
	if format == importers.SourceAppleMusic {
		return []roleDef{
			{importers.RolePlayActivity, "Apple Music Play Activity.csv"},
			{importers.RoleDailyTracks, "Apple Music - Play History Daily Tracks.csv"},
		}
	}
	return nil
}

// RequiredFiles is requiredFiles for the API: format -> files, multi-file formats only.
func RequiredFiles() map[string][]map[string]string {
	out := map[string][]map[string]string{}
	for _, f := range FormatNames() {
		for _, d := range requiredFiles(f) {
			out[f] = append(out[f], map[string]string{"role": d.Role, "label": d.Label})
		}
	}
	return out
}

// JobFile is one required file of a multi-file import and whether it is here.
type JobFile struct {
	Role     string `json:"role"`
	Label    string `json:"label"`
	Present  bool   `json:"present"`
	Filename string `json:"filename,omitempty"`
	Size     int64  `json:"size,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

// fileRef is a stored file (or zip entry) of a multi-file job.
type fileRef struct {
	id                    int64
	role, filename, entry string
	path                  string // "" once deleted
	size                  int64
	sha                   string
}

// Phases (import_job.phase).
const (
	PhasePreview = "preview"
	PhaseApply   = "apply"
)

// JobOptions are the per-job settings, frozen at upload so the apply pass
// evaluates exactly what the preview did.
type JobOptions struct {
	// DedupeWindowSec: a listen is a duplicate of a stored scrobble with the
	// same normalized artist+title whose played_at differs by at most this.
	DedupeWindowSec float64 `json:"dedupe_window_sec"`
	// SpotifyMinMs: Spotify plays shorter than this are skipped (0 = parser
	// default of 30s, negative disables).
	SpotifyMinMs int64 `json:"spotify_min_ms,omitempty"`
	// FuzzyConsent: the user accepted that listens with no ids may be matched
	// to provider records by fuzzy name search and linked automatically. Off:
	// those matches are only offered as candidates for review. Frozen at
	// confirm like the other options.
	FuzzyConsent bool `json:"fuzzy_consent,omitempty"`
	// DebugLimit (debug only): process just the N most recent listens of the
	// file by played-at time. 0 = off (then YOUNITHM_DEBUG_IMPORT_LIMIT applies).
	DebugLimit int `json:"debug_limit,omitempty"`
	// From and To (YYYY-MM-DD, both inclusive) keep only the listens played
	// on or between those days in time zone TZ (an IANA name, the uploader's
	// browser zone; UTC when empty). The rest are left out and counted.
	// Either may be empty for an open end.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	TZ   string `json:"tz,omitempty"`
}

const dayLayout = "2006-01-02"

// parseDay reads a YYYY-MM-DD option as the start of that day in loc ("" = unset).
func parseDay(v string, loc *time.Location) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	return time.ParseInLocation(dayLayout, v, loc)
}

// location is the zone From and To are read in.
func (o JobOptions) location() *time.Location {
	if loc, err := time.LoadLocation(o.TZ); o.TZ != "" && err == nil {
		return loc
	}
	return time.UTC
}

// dateFilter returns a test for whether a listen played at t is inside
// From..To. Options are validated at upload; an unreadable one is ignored.
func (o JobOptions) dateFilter() func(time.Time) bool {
	loc := o.location()
	from, _ := parseDay(o.From, loc)
	to, _ := parseDay(o.To, loc)
	if !to.IsZero() {
		to = to.AddDate(0, 0, 1) // inclusive: up to the end of that day
	}
	return func(t time.Time) bool {
		return (from.IsZero() || !t.Before(from)) && (to.IsZero() || t.Before(to))
	}
}

// Window returns the dedupe tolerance.
func (o JobOptions) Window() time.Duration {
	return time.Duration(o.DedupeWindowSec * float64(time.Second))
}

// Job is the API/DB view of one import.
type Job struct {
	ID             int64           `json:"id"`
	Status         string          `json:"status"`
	Phase          string          `json:"phase"`
	Filename       string          `json:"filename"`
	Format         string          `json:"format"`
	FormatDetected bool            `json:"format_detected"`
	FileSize       int64           `json:"file_size"`
	FileSHA256     string          `json:"file_sha256"`
	Options        JobOptions      `json:"options"`
	Progress       Progress        `json:"progress"`
	Preview        json.RawMessage `json:"preview,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          string          `json:"error,omitempty"`
	// Files and Missing describe a multi-file import (empty for single-file
	// formats): which required files are present, and the roles still missing.
	Files      []JobFile `json:"files,omitempty"`
	Missing    []string  `json:"missing,omitempty"`
	CreatedAt  string    `json:"created_at"`
	UpdatedAt  string    `json:"updated_at"`
	StartedAt  string    `json:"started_at,omitempty"`
	FinishedAt string    `json:"finished_at,omitempty"`

	filePath string
	fileRefs []fileRef // multi-file jobs
}

// filesOnDisk reports whether everything the job needs is still stored.
func (j Job) filesOnDisk() bool {
	if def := requiredFiles(j.Format); def != nil {
		for _, d := range def {
			ok := false
			for _, f := range j.fileRefs {
				if f.role == d.Role && f.path != "" {
					ok = true
				}
			}
			if !ok {
				return false
			}
		}
		return true
	}
	return j.filePath != ""
}

// requireFiles returns an error naming the first required file that is not
// present: the check every stage of a multi-file import makes before it starts.
func (j Job) requireFiles() error {
	for _, d := range requiredFiles(j.Format) {
		ok := false
		for _, f := range j.fileRefs {
			if f.role == d.Role && f.path != "" {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("%s is missing: an %s import needs all its files before anything starts", d.Label, j.Format)
		}
	}
	return nil
}

func joinNames(refs []fileRef, role string) string {
	var names []string
	for _, f := range refs {
		if f.role == role {
			names = append(names, f.filename)
		}
	}
	return strings.Join(names, ", ")
}

// Progress holds the counters of the current phase.
type Progress struct {
	RowsSeen  int64 `json:"rows_seen"`
	Parsed    int64 `json:"parsed"`
	Skipped   int64 `json:"skipped"`
	Errors    int64 `json:"errors"`
	New       int64 `json:"new"`
	Duplicate int64 `json:"duplicates"`
	Unmatched int64 `json:"unmatched"`
	Inserted  int64 `json:"inserted"`
}

func (j Job) terminal() bool {
	return j.Status == StatusDone || j.Status == StatusFailed || j.Status == StatusCancelled
}
