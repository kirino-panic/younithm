package enrich

import (
	"context"
	"database/sql"
	"sort"

	"younithm/internal/identity"
	"younithm/internal/providers"
)

// albumSource is a track whose albums may fill a gap, and whether the listen
// named its id (so its album is exactly the one that was played).
type albumSource struct {
	t       *providers.Track
	trusted bool
}

// fillReleaseGaps fills what a recording's listens left empty about its album,
// from what the catalogues say, whenever they say something:
//
//   - A recording that is on no release (an Apple Play Activity row has no album
//     name for most plays) gets the album a catalogue names: the release is found
//     by the album's barcode or id, then by title, as when resolving a listen,
//     else created, and the recording is put on it as its primary release. An id
//     the listen named comes first (it is the album that was played); an album we
//     only found by ISRC still fills the gap, because an album is better than
//     none, and Spotify's, then Apple's, then MusicBrainz's is preferred.
//   - A play with no release and no album of its own gets the recording's primary
//     release, the default the design gives plays. A play that names an album is
//     never reassigned.
//
// A recording a person has edited is left alone: they may have removed a release
// on purpose. The albums' own metadata (ids, barcode, date, album artists) is
// attached by the normal path, applyReleases, which runs again once the recording
// is on a release.
func fillReleaseGaps(ctx context.Context, tx *sql.Tx, recID int64, srcs []albumSource, albums map[string]providers.Album) error {
	if edited, err := handEdited(ctx, tx, recID); err != nil || edited {
		return err
	}
	var onRelease int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recording_release WHERE recording_id=?`, recID).Scan(&onRelease); err != nil {
		return err
	}
	if onRelease == 0 {
		made, err := putOnCatalogueAlbum(ctx, tx, recID, srcs, albums)
		if err != nil || !made {
			return err
		}
		for _, s := range srcs { // attach the albums' ids, barcode, date and album artists now it has a release
			if err := applyReleases(ctx, tx, recID, s.t, s.trusted, albums); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE scrobble SET release_id=(
			SELECT release_id FROM recording_release WHERE recording_id=?1 ORDER BY is_primary DESC, release_id LIMIT 1)
		WHERE recording_id=?1 AND release_id IS NULL AND (raw_album IS NULL OR trim(raw_album)='')
		  AND EXISTS (SELECT 1 FROM recording_release WHERE recording_id=?1)`, recID)
	return err
}

func providerRank(p string) int {
	switch p {
	case providers.Spotify:
		return 0
	case providers.AppleMusic:
		return 1
	case providers.ITunes:
		return 2
	case providers.MusicBrainz:
		return 3
	}
	return 4
}

// putOnCatalogueAlbum puts the recording on the best album a catalogue names and
// reports whether it did.
func putOnCatalogueAlbum(ctx context.Context, tx *sql.Tx, recID int64, srcs []albumSource, albums map[string]providers.Album) (bool, error) {
	ordered := append([]albumSource(nil), srcs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].trusted != ordered[j].trusted {
			return ordered[i].trusted
		}
		return providerRank(ordered[i].t.Provider) < providerRank(ordered[j].t.Provider)
	})
	for _, s := range ordered {
		for _, ref := range s.t.Releases {
			if ref.ProviderID == "" && identity.Norm(ref.Title) == "" {
				continue
			}
			// The release that already holds this album's barcode is it.
			if a, ok := albums[albumKey(s.t.Provider, ref.ProviderID)]; ok && ref.ProviderID != "" {
				if upc := providers.NormalizeBarcode(a.UPC); upc != "" {
					owner, err := identity.FindByLink(ctx, tx, identity.KindRelease, identity.UPCProv, upc)
					if err != nil {
						return false, err
					}
					if owner != 0 {
						_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO recording_release (recording_id, release_id, is_primary)
							VALUES (?,?, NOT EXISTS (SELECT 1 FROM recording_release WHERE recording_id=?))`, recID, owner, recID)
						return err == nil, err
					}
				}
			}
			in := identity.Input{Album: ref.Title}
			switch s.t.Provider {
			case providers.Spotify:
				in.SpotifyAlbumID = ref.ProviderID
			case providers.AppleMusic, providers.ITunes:
				in.AppleAlbumID = ref.ProviderID
			case providers.MusicBrainz:
				in.ReleaseMBID, in.ReleaseGroupMBID = ref.ProviderID, ref.GroupID
			}
			for _, a := range ref.Artists {
				c := identity.CreditFor(s.t.Provider, a.ProviderID, a.Name)
				c.JoinPhrase = a.JoinPhrase
				in.AlbumArtists = append(in.AlbumArtists, c)
			}
			rel, err := identity.ReleaseFor(ctx, tx, in, recID)
			if err != nil {
				return false, err
			}
			if rel != 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// ReasonBarcodeVariant marks a merge candidate the matcher merges by itself:
// the catalogue's album carries the barcode one release holds, and the other
// release is that album under its own title with no ids or barcode of its own
// (Apple's "Nocturnal - Single" next to Spotify's "Nocturnal").
const ReasonBarcodeVariant = "same album and barcode, named differently"

// barcodeVariant reports whether other is the barcode-holding target's album
// under another name: target holds upc, other holds no release links at all.
func barcodeVariant(ctx context.Context, tx *sql.Tx, target, other int64, upc string) (bool, error) {
	if upc == "" {
		return false, nil
	}
	var holds, links int
	if err := tx.QueryRowContext(ctx, `SELECT
  (SELECT COUNT(*) FROM external_link WHERE entity_type='release' AND entity_id=? AND provider=? AND provider_id=?),
  (SELECT COUNT(*) FROM external_link WHERE entity_type='release' AND entity_id=?)`,
		target, identity.UPCProv, upc, other).Scan(&holds, &links); err != nil {
		return false, err
	}
	return holds > 0 && links == 0, nil
}

// markBarcodeVariant records the pair for the automatic merge, upgrading a
// weaker flag an earlier run left.
func markBarcodeVariant(ctx context.Context, tx *sql.Tx, a, b int64) error {
	if a > b {
		a, b = b, a
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO merge_candidate (kind, entity_id, other_id, reason) VALUES ('release', ?, ?, ?)
ON CONFLICT(kind, entity_id, other_id) DO UPDATE SET reason=excluded.reason`, a, b, ReasonBarcodeVariant)
	return err
}

// flagSameAlbumVariants raises a merge candidate between the release an album
// was attached to and any other release of the same recording that has the
// album's own title: the same album under another spelling of its name (Apple
// adds " - Single" or " - EP" to a single's or EP's title, so a listen from
// Apple and one from Spotify make two releases). When the barcode ties them
// (see ReasonBarcodeVariant) the pair is merged automatically; otherwise a
// duplicate is better than a wrong merge, so it is flagged for a person. A release that holds
// another barcode is another edition, not a variant, and is not flagged.
func flagSameAlbumVariants(ctx context.Context, tx *sql.Tx, target int64, ours []ourRelease, title, upc string, alb providers.Album, albums map[string]providers.Album) error {
	if identity.Norm(title) == "" {
		return nil
	}
	for _, o := range ours {
		if o.id == target || identity.Norm(o.title) != identity.Norm(title) {
			continue
		}
		held, err := releaseBarcodes(ctx, tx, o.id)
		if err != nil {
			return err
		}
		if upc != "" && len(held) > 0 && !contains(held, upc) && !sameReleaseOtherBarcode(albums, held, alb) {
			continue // another edition under the same name
		}
		same, err := barcodeVariant(ctx, tx, target, o.id, upc)
		if err != nil {
			return err
		}
		if same {
			if err := markBarcodeVariant(ctx, tx, target, o.id); err != nil {
				return err
			}
			// Album artists too, in case the merge is left to a person.
			if err := copyAlbumArtists(ctx, tx, target, o.id); err != nil {
				return err
			}
			continue
		}
		if err := identity.Candidate(ctx, tx, identity.KindRelease, target, o.id, "same title as the catalogue's album"); err != nil {
			return err
		}
		if err := copyAlbumArtists(ctx, tx, target, o.id); err != nil {
			return err
		}
	}
	return nil
}

// copyAlbumArtists gives a release that has no album artist the ones the release
// it is a variant of has: every album has at least one, and a variant that no
// catalogue album is attached to would otherwise stay without. A release that
// already has album artists is never changed.
func copyAlbumArtists(ctx context.Context, tx *sql.Tx, from, to int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO release_artist (release_id, artist_id, position, join_phrase)
		SELECT ?2, artist_id, position, join_phrase FROM release_artist WHERE release_id=?1
		AND NOT EXISTS (SELECT 1 FROM release_artist WHERE release_id=?2)`, from, to)
	return err
}
