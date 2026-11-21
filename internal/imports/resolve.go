package imports

import (
	"context"
	"database/sql"

	"younithm/internal/identity"
	"younithm/internal/importers"
)

// dbq is the read surface shared by *sql.DB and *sql.Tx.
type dbq interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// How a listen can be matched, reported in the preview's "matching" counts.
const (
	matchByID   = "by_id"   // carries an id (MBID, ISRC, Spotify or Apple catalog id): exact lookup
	matchByName = "by_name" // names only: matched by name now, fuzzy matching later (needs consent)
)

// identityInput maps an imported listen onto what the identity resolver reads:
// the credit lists, ids, durations and album artists the source carries (a
// ListenBrainz-shaped payload adds its credit arrays, join phrases and
// mbid_mapping), with the same rules as a live scrobble.
func identityInput(l importers.Listen) identity.Input {
	in := identity.FromBasics(l.RawPayload, identity.Basics{
		Artist: l.Artist, Title: l.Title, Album: l.Album, DurationMs: l.TrackDurationMs,
		RecordingMBID: l.RecordingMBID, ReleaseMBID: l.ReleaseMBID, ISRC: l.ISRC,
		ReleaseGroupMBID: l.ReleaseGroupMBID, SpotifyTrackID: l.SpotifyTrackID, ArtistMBIDs: l.ArtistMBIDs,
	})
	if in.AppleSongID == "" {
		in.AppleSongID = l.AppleSongID
	}
	if len(in.AlbumArtists) == 0 && identity.Norm(l.AlbumArtist) != "" {
		in.AlbumArtists = []identity.Credit{{Name: l.AlbumArtist}}
	}
	return in
}

// matchPath says whether the listen can be resolved by an id or only by name.
func matchPath(in identity.Input) string {
	if in.RecordingMBID != "" || in.ISRC != "" || in.SpotifyTrackID != "" || in.AppleSongID != "" {
		return matchByID
	}
	return matchByName
}
