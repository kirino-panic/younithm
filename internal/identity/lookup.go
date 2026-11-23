package identity

import (
	"context"
	"database/sql"
	"errors"
)

// Lookup finds the library's recording and release for a listen without
// writing anything: it is for a track that is only playing now and may never
// be scrobbled. It uses the same keys as Resolve, in the same order (ids, then
// a learned spelling, then the track's own title under its first artist), and
// gives up rather than guess: a name match counts only when it is the only one.
// Zero means not found.
func Lookup(ctx context.Context, q DB, in Input) (recID, relID int64, err error) {
	if recID, err = lookupRecording(ctx, q, in); err != nil {
		return 0, 0, err
	}
	relID, err = lookupRelease(ctx, q, in, recID)
	return recID, relID, err
}

func lookupRecording(ctx context.Context, q DB, in Input) (int64, error) {
	ids := nonEmpty([]idRef{
		{MusicBrainz, in.RecordingMBID, ""},
		{Spotify, in.SpotifyTrackID, ""},
		{Apple, in.AppleSongID, ""},
		{ISRCProv, in.ISRC, ""},
	})
	for _, x := range ids {
		id, err := entityByLink(ctx, q, kRecording, x.prov, x.id)
		if err != nil || id == 0 {
			if err != nil {
				return 0, err
			}
			continue
		}
		if x.prov == ISRCProv { // shared across versions: the title must agree
			if ok, err := titleIs(ctx, q, id, in.Title); err != nil || !ok {
				if err != nil {
					return 0, err
				}
				continue
			}
		}
		return id, nil
	}
	rec, err := recordingByAlias(ctx, q, in)
	if err != nil || rec == 0 {
		if err != nil {
			return 0, err
		}
		artist := in.Artist
		if len(in.Credits) > 0 {
			artist = in.Credits[0].Name
		}
		if Norm(artist) == "" || Norm(in.Title) == "" {
			return 0, nil
		}
		if rec, err = only(ctx, q, `SELECT DISTINCT r.id FROM recording r
			JOIN recording_artist ra ON ra.recording_id = r.id JOIN artist a ON a.id = ra.artist_id
			WHERE lower(trim(r.title)) = ? AND lower(trim(a.name)) = ? LIMIT 2`, Norm(in.Title), Norm(artist)); err != nil || rec == 0 {
			return 0, err
		}
	}
	// A track that holds another id of the same catalogue is a different track.
	if conflict, err := hasConflict(ctx, q, kRecording, rec, ids); err != nil || conflict {
		return 0, err
	}
	return rec, nil
}

func lookupRelease(ctx context.Context, q DB, in Input, rec int64) (int64, error) {
	ids := nonEmpty([]idRef{
		{MusicBrainz, in.ReleaseMBID, ""},
		{Spotify, in.SpotifyAlbumID, ""},
		{Apple, in.AppleAlbumID, ""},
	})
	for _, x := range ids {
		id, err := entityByLink(ctx, q, kRelease, x.prov, x.id)
		if err != nil || id == 0 {
			if err != nil {
				return 0, err
			}
			continue
		}
		if x.prov == Apple && Norm(in.Album) != "" { // weak: the title must agree
			if ok, err := releaseTitleIs(ctx, q, id, in.Album); err != nil || !ok {
				if err != nil {
					return 0, err
				}
				continue
			}
		}
		return id, nil
	}
	if rec == 0 {
		return 0, nil
	}
	if Norm(in.Album) != "" {
		// The track's album of that title, when it has exactly one.
		return only(ctx, q, `SELECT rel.id FROM recording_release rr JOIN release rel ON rel.id = rr.release_id
			WHERE rr.recording_id = ? AND lower(trim(rel.title)) = ? LIMIT 2`, rec, Norm(in.Album))
	}
	// No album named: the track's primary album.
	var rel int64
	err := q.QueryRowContext(ctx, `SELECT release_id FROM recording_release WHERE recording_id = ?
		ORDER BY is_primary DESC, release_id LIMIT 1`, rec).Scan(&rel)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rel, err
}

// only returns the single id a query yields, or 0 when it yields none or
// several (the query should LIMIT 2).
func only(ctx context.Context, q DB, query string, args ...any) (int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 {
		return 0, rows.Err()
	}
	return ids[0], rows.Err()
}
