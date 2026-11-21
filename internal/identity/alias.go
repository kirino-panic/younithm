package identity

import (
	"context"
	"database/sql"
	"errors"
)

// Alias keys. They must equal the keys internal/edit writes (a test asserts
// it), so aliases from manual edits and from enrichment are interchangeable.
const keySep = "\x1f"

// ArtistAliasKey is the alias raw_string for an artist name.
func ArtistAliasKey(name string) string { return Norm(name) }

// RecordingAliasKey is the alias raw_string for a recording: the normalised
// artist string and title joined by a unit separator.
func RecordingAliasKey(artist, title string) string { return Norm(artist) + keySep + Norm(title) }

// ReleaseAliasKey is the alias raw_string for an album title.
func ReleaseAliasKey(title string) string { return Norm(title) }

// AddAlias records key as another name of entity id, so later raw matching
// finds it. It returns whether a row was added. It never repoints an alias
// that already exists, and it skips a name that another entity already holds
// as its own canonical name (an alias would hijack that entity's listens).
func AddAlias(ctx context.Context, q DB, kind string, id int64, key string) (bool, error) {
	if key == "" || key == keySep {
		return false, nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM alias WHERE kind=? AND raw_string=?`, kind, key).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	switch kind {
	case kArtist:
		err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist WHERE id<>? AND lower(trim(name))=?`, id, key).Scan(&n)
		if err != nil {
			return false, err
		}
	case kRelease:
		err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM release WHERE id<>? AND lower(trim(title))=?`, id, key).Scan(&n)
		if err != nil {
			return false, err
		}
	case kRecording:
		err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM recording r
			JOIN recording_artist ra ON ra.recording_id=r.id JOIN artist a ON a.id=ra.artist_id
			WHERE r.id<>? AND lower(trim(a.name)) || char(31) || lower(trim(r.title)) = ?`, id, key).Scan(&n)
		if err != nil {
			return false, err
		}
	default:
		return false, errors.New("identity: unknown alias kind " + kind)
	}
	if n > 0 {
		return false, nil
	}
	res, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO alias (kind, raw_string, entity_id) VALUES (?,?,?)`, kind, key, id)
	if err != nil {
		return false, err
	}
	a, _ := res.RowsAffected()
	return a > 0, nil
}

// entityByAlias returns the entity an alias key points at (0 = none), only
// when that entity still exists.
func entityByAlias(ctx context.Context, q DB, kind, key string) (int64, error) {
	if key == "" || key == keySep {
		return 0, nil
	}
	table := map[string]string{kArtist: "artist", kRecording: "recording", kRelease: "release"}[kind]
	var e int64
	err := q.QueryRowContext(ctx, `SELECT entity_id FROM alias WHERE kind=? AND raw_string=? AND
		EXISTS (SELECT 1 FROM `+table+` WHERE id=entity_id)`, kind, key).Scan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return e, err
}
