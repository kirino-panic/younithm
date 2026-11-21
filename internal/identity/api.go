package identity

import (
	"context"
	"strings"
)

// Entity kinds, equal to external_link.entity_type and alias.kind.
const (
	KindArtist    = kArtist
	KindRecording = kRecording
	KindRelease   = kRelease
)

// Ref is one provider id for an entity.
type Ref struct{ Provider, ID, Method string }

// FindByLink returns the entity that holds a non-candidate link to the given
// provider id (0 = none).
func FindByLink(ctx context.Context, q DB, kind, provider, id string) (int64, error) {
	return entityByLink(ctx, q, kind, provider, id)
}

// AttachIDs adds provider ids to an entity as auto links, with the guards
// resolving uses: an id another entity already owns is left there (the caller
// should flag a merge candidate), a different id of a strong provider
// (MusicBrainz, Spotify) on the entity is a conflict and nothing is added, and
// ids the admin rejected are skipped. It returns how many links the entity now
// holds among refs (added or already present).
func AttachIDs(ctx context.Context, q DB, kind string, entity int64, refs []Ref) (held int, conflict bool, err error) {
	var ids []idRef
	for _, x := range refs {
		if strings.TrimSpace(x.ID) != "" {
			ids = append(ids, idRef{x.Provider, strings.TrimSpace(x.ID), x.Method})
		}
	}
	if len(ids) == 0 {
		return 0, false, nil
	}
	if conflict, err = hasConflict(ctx, q, kind, entity, ids); err != nil || conflict {
		return 0, conflict, err
	}
	held, err = (&Resolver{}).attach(ctx, q, kind, entity, ids)
	return held, false, err
}

// HasConflict reports whether the entity already holds a different id of a
// strong provider (MusicBrainz, Spotify) than one of refs. Two different ids of
// the same provider are two entities whatever their names, so an entity with a
// conflict must not be taken for the one the refs name, nor receive their ids,
// aliases or verification.
func HasConflict(ctx context.Context, q DB, kind string, entity int64, refs []Ref) (bool, error) {
	var ids []idRef
	for _, x := range refs {
		if id := strings.TrimSpace(x.ID); id != "" {
			ids = append(ids, idRef{x.Provider, id, x.Method})
		}
	}
	return hasConflict(ctx, q, kind, entity, ids)
}

// ReleaseFor finds or creates the release an album names and puts the recording
// on it (as its primary release when it has none), by the same rules as
// resolving a listen: an id we hold, then the title (a single match), else a new
// release; it never merges. Only the album fields of in are used.
func ReleaseFor(ctx context.Context, q DB, in Input, recID int64) (int64, error) {
	return (&Resolver{}).resolveRelease(ctx, q, in, recID, nil)
}

// MarkVerified flags an artist as corroborated by a provider id.
func MarkVerified(ctx context.Context, q DB, artist int64) error {
	_, err := q.ExecContext(ctx, `UPDATE artist SET verified=1 WHERE id=? AND verified=0`, artist)
	return err
}

// CreditFor builds a Credit that carries id under the right field for provider.
func CreditFor(provider, id, name string) Credit {
	c := Credit{Name: name}
	switch provider {
	case MusicBrainz:
		c.MBID = id
	case Spotify:
		c.SpotifyID = id
	case Apple:
		c.AppleID = id
	}
	return c
}

// ArtistFor finds or creates the artist a credit names, by the same rules as
// resolving a listen.
func ArtistFor(ctx context.Context, q DB, c Credit) (int64, error) {
	return (&Resolver{}).resolveArtist(ctx, q, c, "")
}

// FillReleaseArtists sets a release's album artists when it has none yet (an
// album first seen without album-artist data). A release that already has
// album artists is left alone.
func FillReleaseArtists(ctx context.Context, q DB, release int64, credits []Credit) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_artist WHERE release_id=?`, release).Scan(&n); err != nil || n > 0 {
		return err
	}
	var aids []int64
	var used []Credit
	for _, c := range credits {
		if Norm(c.Name) == "" && c.MBID == "" && c.SpotifyID == "" && c.AppleID == "" {
			continue
		}
		id, err := ArtistFor(ctx, q, c)
		if err != nil {
			return err
		}
		aids = append(aids, id)
		used = append(used, c)
	}
	return (&Resolver{}).credits(ctx, q, "release_artist", "release_id", release, aids, used)
}

// Candidate records a pair of entities for the manual merge queue.
func Candidate(ctx context.Context, q DB, kind string, a, b int64, reason string) error {
	return (&Resolver{}).candidate(ctx, q, kind, a, b, reason)
}
