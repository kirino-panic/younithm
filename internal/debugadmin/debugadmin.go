// Package debugadmin holds the owner's debug tools: counts of what is stored
// and a full wipe of the library.
package debugadmin

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"younithm/internal/artwork"
)

// ErrBusy is returned when an import is still queued or running.
var ErrBusy = errors.New("an import is in progress")

// Counts is a quick look at what the library holds.
type Counts struct {
	Scrobbles  int64 `json:"scrobbles"`
	Recordings int64 `json:"recordings"`
	Releases   int64 `json:"releases"`
	Artists    int64 `json:"artists"`
	Imports    int64 `json:"imports"`
}

// Count reads the row counts.
func Count(ctx context.Context, db *sql.DB) (Counts, error) {
	var c Counts
	for _, q := range []struct {
		sql string
		to  *int64
	}{
		{`SELECT COUNT(*) FROM scrobble`, &c.Scrobbles},
		{`SELECT COUNT(*) FROM recording`, &c.Recordings},
		{`SELECT COUNT(*) FROM release`, &c.Releases},
		{`SELECT COUNT(*) FROM artist`, &c.Artists},
		{`SELECT COUNT(*) FROM import_job`, &c.Imports},
	} {
		if err := db.QueryRowContext(ctx, q.sql).Scan(q.to); err != nil {
			return Counts{}, err
		}
	}
	return c, nil
}

// Track is a library track as the debug now-playing shows it.
type Track struct {
	Artist, Title, Album, Image string
	DurationMs                  int64
}

// RandomTrack picks a track at random from the library, with its primary
// artists and best album (one with artwork if any). ok is false when the
// library has no tracks.
func RandomTrack(ctx context.Context, db *sql.DB) (t Track, ok bool) {
	var id int64
	err := db.QueryRowContext(ctx, `
SELECT r.id, r.title,
  COALESCE((SELECT group_concat(name, ', ') FROM (
    SELECT a.name FROM recording_artist ra JOIN artist a ON a.id = ra.artist_id
    WHERE ra.recording_id = r.id AND ra.role = 'primary' ORDER BY ra.position)), ''),
  COALESCE(r.duration_ms, 0)
FROM recording r ORDER BY RANDOM() LIMIT 1`).Scan(&id, &t.Title, &t.Artist, &t.DurationMs)
	if err != nil {
		return Track{}, false
	}
	var relID int64
	var art sql.NullString
	err = db.QueryRowContext(ctx, `
SELECT rel.id, rel.title, `+artwork.ReleaseImage("rel")+` FROM recording_release rr JOIN release rel ON rel.id = rr.release_id
WHERE rr.recording_id = ?
ORDER BY (COALESCE(`+artwork.ReleaseImage("rel")+`, '') != '') DESC, rr.is_primary DESC, rel.release_date LIMIT 1`, id).Scan(&relID, &t.Album, &art)
	if err == nil {
		t.Image = artwork.URL(artwork.KindRelease, relID, art.String)
	}
	return t, true
}

// libraryTables are emptied by Clear, children first. Accounts, provider
// secrets, site settings (page, profile, visibility, naming) and the
// provider response cache are left alone.
var libraryTables = []string{
	"scrobble", "recording_artist", "release_artist", "recording_release", "alias",
	"external_link", "merge_candidate", "link_rejection", "enrichment_job", "artwork_try",
	"identity_backfill", "edit_log", "import_job_file", "import_job",
	"recording", "release", "artist",
}

// Clear deletes every scrobble, entity, link, edit and import record, then
// the stored import uploads and downloaded artwork in dirs. Import history goes
// too, so the same file can be imported again. It refuses while an import is
// queued or running.
func Clear(ctx context.Context, db *sql.DB, dirs ...string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var busy int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_job WHERE status IN ('queued','running')`).Scan(&busy); err != nil {
		return err
	}
	if busy > 0 {
		return ErrBusy
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	for _, t := range libraryTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, d := range dirs {
		emptyDir(d)
	}
	return nil
}

// emptyDir removes what is inside dir but keeps dir itself.
func emptyDir(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
