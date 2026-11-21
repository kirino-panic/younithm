package identity

import "context"

// Integrity counts how well the data follows the identity rules
// (docs/identity.md). The first three should
// always be zero; the rest are work waiting for review.
type Integrity struct {
	// Must be zero.
	ScrobblesWithoutRecording  int64 `json:"scrobbles_without_recording"` // a titled listen that maps to no recording
	RecordingsWithoutArtist    int64 `json:"recordings_without_artist"`
	ScrobblesOffTheirRecording int64 `json:"scrobbles_on_unrelated_release"` // release_id not among the recording's releases
	// Awaiting review or enrichment.
	ReleasesWithoutAlbumArtist int64 `json:"releases_without_album_artist"`
	UnverifiedArtists          int64 `json:"unverified_artists"`
	MergeCandidates            int64 `json:"merge_candidates"`
	LinkCandidates             int64 `json:"link_candidates"`
}

// CheckIntegrity runs the counts. Fix the first three with a backfill run.
func CheckIntegrity(ctx context.Context, q DB) (Integrity, error) {
	var in Integrity
	for _, c := range []struct {
		dst *int64
		sql string
	}{
		{&in.ScrobblesWithoutRecording, `SELECT COUNT(*) FROM scrobble WHERE recording_id IS NULL AND trim(raw_title) <> ''`},
		{&in.RecordingsWithoutArtist, `SELECT COUNT(*) FROM recording r WHERE NOT EXISTS (SELECT 1 FROM recording_artist ra WHERE ra.recording_id=r.id)`},
		{&in.ScrobblesOffTheirRecording, `SELECT COUNT(*) FROM scrobble s WHERE s.release_id IS NOT NULL AND s.recording_id IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM recording_release rr WHERE rr.recording_id=s.recording_id AND rr.release_id=s.release_id)`},
		{&in.ReleasesWithoutAlbumArtist, `SELECT COUNT(*) FROM release r WHERE NOT EXISTS (SELECT 1 FROM release_artist ra WHERE ra.release_id=r.id)`},
		{&in.UnverifiedArtists, `SELECT COUNT(*) FROM artist WHERE verified=0`},
		{&in.MergeCandidates, `SELECT COUNT(*) FROM merge_candidate`},
		{&in.LinkCandidates, `SELECT COUNT(DISTINCT entity_type || ':' || entity_id) FROM external_link WHERE status='candidate'`},
	} {
		if err := q.QueryRowContext(ctx, c.sql).Scan(c.dst); err != nil {
			return in, err
		}
	}
	return in, nil
}
