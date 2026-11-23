package enrich

import (
	"context"
	"database/sql"
	"strings"

	"younithm/internal/identity"
	"younithm/internal/providers"
	"younithm/internal/providers/match"
)

// applyTrack writes what a provider says about a recording we hold onto the
// recording, its artists and its releases (docs/identity.md):
//
//   - ISRCs become recording links, so the same recording can be found on the
//     other platforms and on MusicBrainz.
//   - Credited artists: the provider's artist id is attached to the artist it
//     is paired with (by an id we already hold, by name, or by position when
//     the track was fetched by id), and its spelling becomes an alias. When
//     credits is set, the provider's credit list also becomes the recording's
//     artists (see replaceCredits).
//   - Releases: the provider's album id is attached to our release of that
//     title (or the only one), with the album artists, date, type, release
//     group and title variant.
//   - Title and artist spellings become recording aliases, so raw matching
//     finds the same recording under translated or localised names.
//
// exact is true when the track was fetched by an id the listen itself named (a
// link we found ourselves is not exact: it may be wrong). credits is true for
// the one such id that sets the recording's artists. It runs in
// the caller's transaction and never merges: an id that another entity
// already owns becomes a merge candidate instead.
func applyTrack(ctx context.Context, tx *sql.Tx, recID int64, t *providers.Track, exact, credits bool, albums map[string]providers.Album) error {
	for _, isrc := range t.ISRCs {
		ref := identity.Ref{Provider: identity.ISRCProv, ID: isrc, Method: match.MethodLookup}
		if err := attachOrFlag(ctx, tx, identity.KindRecording, recID, ref, "same ISRC"); err != nil {
			return err
		}
	}
	if t.DurationMs > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE recording SET duration_ms=? WHERE id=? AND duration_ms IS NULL`, t.DurationMs, recID); err != nil {
			return err
		}
	}

	if credits {
		if err := replaceCredits(ctx, tx, recID, t); err != nil {
			return err
		}
	} else if err := addMissingCredits(ctx, tx, recID, t); err != nil {
		return err
	}
	ours, err := creditedArtists(ctx, tx, recID)
	if err != nil {
		return err
	}
	pairs, err := pairArtists(ctx, tx, ours, t, exact)
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if p.pa.ProviderID != "" {
			ref := identity.Ref{Provider: t.Provider, ID: p.pa.ProviderID, Method: match.MethodLookup}
			// Our artist already holds a different id of this provider: it is not
			// the artist the provider names, so nothing of the provider's (the id,
			// the spelling as an alias, the verification) goes onto it. An alias
			// added here would make later lookups by that name resolve to the wrong
			// artist for good.
			if conflict, err := identity.HasConflict(ctx, tx, identity.KindArtist, p.ours, []identity.Ref{ref}); err != nil {
				return err
			} else if conflict {
				continue
			}
			if err := attachOrFlag(ctx, tx, identity.KindArtist, p.ours, ref, "same provider artist id"); err != nil {
				return err
			}
			if held, err := holds(ctx, tx, identity.KindArtist, p.ours, ref); err != nil {
				return err
			} else if held {
				if err := identity.MarkVerified(ctx, tx, p.ours); err != nil {
					return err
				}
			}
		}
		if _, err := identity.AddAlias(ctx, tx, identity.KindArtist, p.ours, identity.ArtistAliasKey(p.pa.Name)); err != nil {
			return err
		}
	}

	var names []string
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	for _, n := range names {
		if _, err := identity.AddAlias(ctx, tx, identity.KindRecording, recID, identity.RecordingAliasKey(n, t.Title)); err != nil {
			return err
		}
	}
	if len(names) > 1 {
		if _, err := identity.AddAlias(ctx, tx, identity.KindRecording, recID, identity.RecordingAliasKey(strings.Join(names, ", "), t.Title)); err != nil {
			return err
		}
	}
	for _, a := range ours { // the names we use, with the provider's title
		if _, err := identity.AddAlias(ctx, tx, identity.KindRecording, recID, identity.RecordingAliasKey(a.name, t.Title)); err != nil {
			return err
		}
	}
	if err := fillCreditedNames(ctx, tx, identity.KindRecording, recID, t.Provider, t.Artists); err != nil {
		return err
	}
	return applyReleases(ctx, tx, recID, t, exact, albums)
}

// fillCreditedNames stores, on our credits of a recording or release, the
// names a MusicBrainz credit list says the artists are credited as ("The
// Square" for T-SQUARE). A credit is paired only through the MusicBrainz
// artist id our artist holds, never by name or position, and an id the list
// credits twice is skipped. It fills gaps only:
// a credited name already set is kept, and an entity edited by hand is left
// alone.
func fillCreditedNames(ctx context.Context, tx *sql.Tx, kind string, id int64, provider string, artists []providers.Artist) error {
	if provider != providers.MusicBrainz {
		return nil
	}
	table, col := "recording_artist", "recording_id"
	if kind == identity.KindRelease {
		table, col = "release_artist", "release_id"
	}
	var edited int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM edit_log WHERE entity_type=? AND entity_id=? AND scope<>'link'`, kind, id).Scan(&edited); err != nil || edited > 0 {
		return err
	}
	// An artist credited more than once (ATLUS Sound Team, then the same
	// artist again as "ATLUS GAME MUSIC") cannot be told apart by id: which of
	// our credits a name belongs to is unknown, so none gets it.
	seen := map[string]int{}
	for _, a := range artists {
		seen[a.ProviderID]++
	}
	for _, a := range artists {
		if a.CreditedName == "" || a.ProviderID == "" || seen[a.ProviderID] > 1 {
			continue
		}
		// A credit spelled exactly like our artist's own name adds nothing (our
		// name just differs from MusicBrainz's), and would pin that spelling
		// over the naming preference.
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET credited_name=? WHERE `+col+`=? AND credited_name IS NULL
			AND artist_id IN (SELECT entity_id FROM external_link WHERE entity_type='artist' AND provider=? AND provider_id=? AND status<>'candidate')
			AND (SELECT name FROM artist WHERE id=artist_id) <> ?1`,
			a.CreditedName, id, identity.MusicBrainz, a.ProviderID); err != nil {
			return err
		}
	}
	return nil
}

type ourArtist struct {
	id   int64
	name string
}

func creditedArtists(ctx context.Context, tx *sql.Tx, recID int64) ([]ourArtist, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.id, a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
		WHERE ra.recording_id=? ORDER BY ra.position, a.id`, recID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ourArtist
	for rows.Next() {
		var a ourArtist
		if err := rows.Scan(&a.id, &a.name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type artistPair struct {
	ours int64
	pa   providers.Artist
}

// pairArtists matches the provider's credited artists to the recording's.
// Only certain pairings are made: by an id we already hold, equal names, or
// (for a track fetched by an id the listen itself named: exact) a single artist
// on each side, or what is left over in equal counts, in the platform's order.
// A match we found ourselves (ISRC, search) never pairs different names: it may
// be a different recording, and an id attached to the wrong artist poisons every
// later lookup. Anything else is left unpaired.
func pairArtists(ctx context.Context, tx *sql.Tx, ours []ourArtist, t *providers.Track, exact bool) ([]artistPair, error) {
	var out []artistPair
	used := map[int64]bool{}
	var restPA []providers.Artist
	for _, pa := range t.Artists {
		if pa.ProviderID != "" {
			owner, err := identity.FindByLink(ctx, tx, identity.KindArtist, t.Provider, pa.ProviderID)
			if err != nil {
				return nil, err
			}
			if owner != 0 && !used[owner] && hasArtist(ours, owner) {
				out = append(out, artistPair{owner, pa})
				used[owner] = true
				continue
			}
		}
		restPA = append(restPA, pa)
	}
	var restOurs []ourArtist
	for _, a := range ours {
		if !used[a.id] {
			restOurs = append(restOurs, a)
		}
	}
	if len(restPA) == 0 || len(restOurs) == 0 {
		return out, nil
	}
	if len(restPA) == 1 && len(restOurs) == 1 && exact {
		return append(out, artistPair{restOurs[0].id, restPA[0]}), nil
	}
	// Equal names pair whatever the counts: a recording we hold one artist of
	// still pairs that artist with the provider's matching credit.
	var unmatched []providers.Artist
	for _, pa := range restPA {
		found := false
		for i, a := range restOurs {
			if a.id != 0 && identity.Norm(a.name) == identity.Norm(pa.Name) {
				out = append(out, artistPair{a.id, pa})
				restOurs[i].id = 0
				found = true
				break
			}
		}
		if !found {
			unmatched = append(unmatched, pa)
		}
	}
	// Left over on both sides in equal numbers, for a track fetched by id: the
	// platform's order pairs them.
	if len(unmatched) > 0 && exact {
		var left []ourArtist
		for _, a := range restOurs {
			if a.id != 0 {
				left = append(left, a)
			}
		}
		if len(left) == len(unmatched) {
			for i, pa := range unmatched {
				out = append(out, artistPair{left[i].id, pa})
			}
		}
	}
	return out, nil
}

// handEdited reports whether a person has edited the recording (its artists
// are then theirs: no lookup changes them). Setting a catalogue link by hand
// is not such an edit: the lookup through that link should fill details in.
func handEdited(ctx context.Context, tx *sql.Tx, recID int64) (bool, error) {
	var n int
	// An album's artists applied to its tracks is logged on the album; the
	// tracks it changed are in its rows.
	err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM edit_log WHERE (entity_type='recording' AND entity_id=?1 AND scope<>'link')
   OR (entity_type='release' AND scope='credits' AND EXISTS (
        SELECT 1 FROM json_each(after_json) j
        WHERE json_extract(j.value, '$.t')='recording_artist' AND json_extract(j.value, '$.k.recording_id')=?1))`, recID).Scan(&n)
	return n > 0, err
}

// addMissingCredits adds the artists another catalogue credits on a recording
// and we lack. The catalogue that set the credits (or the listen's own string)
// may name fewer artists than another does: Spotify history names only the
// first, MusicBrainz often lists the main artist alone. It only ever adds, and
// only when the list is plainly the same recording's:
//
//   - it names strictly more artists than we hold;
//   - every artist we hold is paired with one of its credits, by an id we hold
//     or an equal name (never by position), so a link that is a different
//     recording, or a different artist's, adds nothing;
//   - the recording has not been edited by hand.
//
// The extra credits are found or created by the resolver's rules (so an artist
// we already know is reused, and two different ids stay two artists) and are
// appended after ours: the first list keeps its order. The first list to extend
// ours decides the extras; a later list that does not contain what we now hold
// adds nothing, so catalogues that disagree cannot make a recording flip.
func addMissingCredits(ctx context.Context, tx *sql.Tx, recID int64, t *providers.Track) error {
	ours, err := creditedArtists(ctx, tx, recID)
	if err != nil {
		return err
	}
	if len(ours) == 0 || len(t.Artists) <= len(ours) {
		return nil
	}
	for _, pa := range t.Artists {
		if identity.Norm(pa.Name) == "" {
			return nil
		}
	}
	if edited, err := handEdited(ctx, tx, recID); err != nil || edited {
		return err
	}
	pairs, err := pairArtists(ctx, tx, ours, t, false)
	if err != nil {
		return err
	}
	paired := map[int64]bool{}
	used := map[providers.Artist]bool{}
	for _, p := range pairs {
		paired[p.ours], used[p.pa] = true, true
	}
	for _, o := range ours {
		if !paired[o.id] {
			return nil // something we hold is not in this list: not plainly the same credits
		}
	}
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position)+1, 0) FROM recording_artist WHERE recording_id=?`, recID).Scan(&next); err != nil {
		return err
	}
	have := map[int64]bool{}
	for _, o := range ours {
		have[o.id] = true
	}
	for _, pa := range t.Artists {
		if used[pa] {
			continue
		}
		id, err := identity.ArtistFor(ctx, tx, identity.CreditFor(t.Provider, pa.ProviderID, pa.Name))
		if err != nil {
			return err
		}
		if have[id] { // the same artist listed again
			continue
		}
		have[id] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO recording_artist (recording_id, artist_id, role, position, join_phrase)
			VALUES (?,?, 'primary', ?, NULL) ON CONFLICT(recording_id, artist_id, role) DO NOTHING`, recID, id, next); err != nil {
			return err
		}
		next++
	}
	return nil
}

// replaceCredits makes a catalog's credit list the recording's artists. A
// listen's artist string is taken as it says (one artist, named exactly that);
// when the listen also named a catalog id (a Spotify or Apple song, a
// MusicBrainz recording), the catalog's credits replace it: every credited
// artist, in the catalog's order with its join phrases. That is how "DJ
// Noriken" becomes "DJ Noriken & aran", and "Alice & Bob" becomes Alice and Bob.
//
// Each credit reuses an artist the recording already has when the names are
// equal; otherwise it is found or created by the resolver's rules (ids,
// aliases, the single-match name rule). An artist that drops out of the credits
// keeps its raw string as a recording alias, so the same listen still finds the
// recording, and is deleted when nothing refers to it any more and it was never
// verified or linked. A recording that has been edited by hand is left alone.
func replaceCredits(ctx context.Context, tx *sql.Tx, recID int64, t *providers.Track) error {
	if len(t.Artists) == 0 {
		return nil
	}
	for _, pa := range t.Artists {
		if identity.Norm(pa.Name) == "" {
			return nil
		}
	}
	if edited, err := handEdited(ctx, tx, recID); err != nil || edited {
		return err
	}
	old, err := creditedArtists(ctx, tx, recID)
	if err != nil {
		return err
	}
	var ids []int64
	var credits []providers.Artist
	seen := map[int64]bool{}
	for _, pa := range t.Artists {
		var id int64
		for _, o := range old {
			if seen[o.id] || identity.Norm(o.name) != identity.Norm(pa.Name) {
				continue
			}
			// An equal name is not enough: an artist that holds a different id of
			// this provider is a different artist (two Spotify ids are two artists
			// whatever their names), and reusing it would merge the two credits.
			if pa.ProviderID != "" {
				ref := identity.Ref{Provider: t.Provider, ID: pa.ProviderID}
				if conflict, err := identity.HasConflict(ctx, tx, identity.KindArtist, o.id, []identity.Ref{ref}); err != nil {
					return err
				} else if conflict {
					continue
				}
			}
			id = o.id
			break
		}
		if id == 0 {
			if id, err = identity.ArtistFor(ctx, tx, identity.CreditFor(t.Provider, pa.ProviderID, pa.Name)); err != nil {
				return err
			}
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		credits = append(credits, pa)
	}
	var title string
	if err := tx.QueryRowContext(ctx, `SELECT title FROM recording WHERE id=?`, recID).Scan(&title); err != nil {
		return err
	}
	for _, o := range old {
		if seen[o.id] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_artist WHERE recording_id=? AND artist_id=?`, recID, o.id); err != nil {
			return err
		}
		if _, err := identity.AddAlias(ctx, tx, identity.KindRecording, recID, identity.RecordingAliasKey(o.name, title)); err != nil {
			return err
		}
		if err := dropIfUnused(ctx, tx, o.id); err != nil {
			return err
		}
	}
	for i, id := range ids {
		var jp any
		if credits[i].JoinPhrase != "" {
			jp = credits[i].JoinPhrase
		}
		res, err := tx.ExecContext(ctx, `UPDATE recording_artist SET position=?, join_phrase=? WHERE recording_id=? AND artist_id=?`, i, jp, recID, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO recording_artist (recording_id, artist_id, role, position, join_phrase)
				VALUES (?,?, 'primary', ?, ?)`, recID, id, i, jp); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropIfUnused deletes an artist that no recording or album credits any more,
// but only when it was never verified and holds no provider link: whatever a
// catalog vouched for stays, so its page and plays history are not lost.
func dropIfUnused(ctx context.Context, tx *sql.Tx, artist int64) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM recording_artist WHERE artist_id=?1) + (SELECT COUNT(*) FROM release_artist WHERE artist_id=?1)
		+ (SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND entity_id=?1)
		+ (SELECT verified FROM artist WHERE id=?1)`, artist).Scan(&n); err != nil || n > 0 {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM alias WHERE kind='artist' AND entity_id=?1`,
		`DELETE FROM merge_candidate WHERE kind='artist' AND (entity_id=?1 OR other_id=?1)`,
		`DELETE FROM artist WHERE id=?1`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, artist); err != nil {
			return err
		}
	}
	return nil
}

func hasArtist(as []ourArtist, id int64) bool {
	for _, a := range as {
		if a.id == id {
			return true
		}
	}
	return false
}

type ourRelease struct {
	id    int64
	title string
}

// applyReleases attaches the provider's albums to our releases.
//
// A barcode (albums, from the catalogue) says whether two albums are the same
// release: the release that already holds it is the target, a release holding a
// different one is a different edition and gets nothing from this album, and an
// album whose release holds none yet sets it. Without a barcode the album is
// attached the old way: by an id we already hold, an equal title, or (for an id
// the listen named, with one album on each side) as the only one. An album link
// that turns out to be another edition's, by barcode, is removed, unless a
// person confirmed or pinned it.
func applyReleases(ctx context.Context, tx *sql.Tx, recID int64, t *providers.Track, exact bool, albums map[string]providers.Album) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.id, r.title FROM recording_release rr JOIN release r ON r.id=rr.release_id
		WHERE rr.recording_id=? ORDER BY rr.is_primary DESC, r.id`, recID)
	if err != nil {
		return err
	}
	var ours []ourRelease
	for rows.Next() {
		var r ourRelease
		if err := rows.Scan(&r.id, &r.title); err != nil {
			rows.Close()
			return err
		}
		ours = append(ours, r)
	}
	rows.Close()
	if len(ours) == 0 {
		return nil
	}
	for _, rr := range t.Releases {
		if rr.ProviderID == "" && strings.TrimSpace(rr.Title) == "" {
			continue
		}
		var target int64
		var upc string
		var alb providers.Album
		if a, ok := albums[albumKey(t.Provider, rr.ProviderID)]; ok && rr.ProviderID != "" {
			alb, upc = a, providers.NormalizeBarcode(a.UPC)
		}
		if rr.ProviderID != "" {
			owner, err := identity.FindByLink(ctx, tx, identity.KindRelease, t.Provider, rr.ProviderID)
			if err != nil {
				return err
			}
			for _, o := range ours {
				if o.id == owner {
					target = owner
				}
			}
		}
		if target == 0 && upc != "" {
			// The release that already holds this barcode is this album's release.
			owner, err := identity.FindByLink(ctx, tx, identity.KindRelease, identity.UPCProv, upc)
			if err != nil {
				return err
			}
			if owner != 0 {
				for _, o := range ours {
					if o.id == owner {
						target = owner
					}
				}
				if target == 0 {
					// Another release of ours holds this barcode: this album is that
					// release. Usually the recording is simply on a different release
					// too (a single and an album), which is no duplicate. But a release
					// of this recording that has the album's own title and no barcode is
					// the same album under another spelling (Apple's " - Single"): it is
					// flagged, and gets the album artists the real release has.
					for _, o := range ours {
						if identity.Norm(o.title) != identity.Norm(rr.Title) {
							continue
						}
						held, err := releaseBarcodes(ctx, tx, o.id)
						if err != nil {
							return err
						}
						if len(held) > 0 {
							continue
						}
						same, err := barcodeVariant(ctx, tx, owner, o.id, upc)
						if err != nil {
							return err
						}
						if same {
							if err := markBarcodeVariant(ctx, tx, owner, o.id); err != nil {
								return err
							}
							// Album artists too, in case the merge is left to a person.
							if err := copyAlbumArtists(ctx, tx, owner, o.id); err != nil {
								return err
							}
							continue
						}
						if err := identity.Candidate(ctx, tx, identity.KindRelease, owner, o.id, "same barcode as the catalogue's album"); err != nil {
							return err
						}
						if err := copyAlbumArtists(ctx, tx, owner, o.id); err != nil {
							return err
						}
					}
					continue
				}
			}
		}
		if target == 0 {
			for _, o := range ours {
				if identity.Norm(o.title) == identity.Norm(rr.Title) {
					target = o.id
					break
				}
			}
		}
		if target == 0 && upc != "" {
			// Our release under a second barcode (same date, same track count), when
			// the titles are worded differently ("Album" and "Album - Single").
			for _, o := range ours {
				held, err := releaseBarcodes(ctx, tx, o.id)
				if err != nil {
					return err
				}
				if len(held) > 0 && sameReleaseOtherBarcode(albums, held, alb) {
					target = o.id
					break
				}
			}
		}
		if target == 0 && exact && len(t.Releases) == 1 && len(ours) == 1 {
			target = ours[0].id
		}
		if target == 0 {
			continue
		}
		if upc != "" {
			held, err := releaseBarcodes(ctx, tx, target)
			if err != nil {
				return err
			}
			if len(held) > 0 && !contains(held, upc) && !sameReleaseOtherBarcode(albums, held, alb) {
				// Our release has other barcodes only, and nothing else says this album
				// is the same release: it is another edition.
				if _, err := tx.ExecContext(ctx, `DELETE FROM external_link WHERE entity_type='release' AND entity_id=?
					AND provider=? AND provider_id=? AND status='auto' AND pinned=0`, target, t.Provider, rr.ProviderID); err != nil {
					return err
				}
				continue
			}
		}
		if rr.ProviderID != "" {
			ref := identity.Ref{Provider: t.Provider, ID: rr.ProviderID, Method: match.MethodLookup}
			if err := attachOrFlag(ctx, tx, identity.KindRelease, target, ref, "same provider album id"); err != nil {
				return err
			}
		}
		if upc != "" {
			ref := identity.Ref{Provider: identity.UPCProv, ID: upc, Method: match.MethodLookup}
			if err := attachOrFlag(ctx, tx, identity.KindRelease, target, ref, "same barcode"); err != nil {
				return err
			}
		}
		if err := flagSameAlbumVariants(ctx, tx, target, ours, rr.Title, upc, alb, albums); err != nil {
			return err
		}
		if _, err := identity.AddAlias(ctx, tx, identity.KindRelease, target, identity.ReleaseAliasKey(rr.Title)); err != nil {
			return err
		}
		date := rr.Date
		if rr.DateIsTrack { // a song's own date is not the album's: use the album's own, or none
			date = alb.Date
		}
		if _, err := tx.ExecContext(ctx, `UPDATE release SET release_date=COALESCE(release_date, NULLIF(?, '')),
			release_type=COALESCE(release_type, NULLIF(?, '')) WHERE id=?`, date, rr.Type, target); err != nil {
			return err
		}
		if rr.Position > 0 { // the track's place on this album, when we have none yet
			disc := rr.Disc
			if disc < 1 {
				disc = 1
			}
			if _, err := tx.ExecContext(ctx, `UPDATE recording_release SET disc=?, position=?
				WHERE recording_id=? AND release_id=? AND position IS NULL`, disc, rr.Position, recID, target); err != nil {
				return err
			}
		}
		if rr.GroupID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE release SET release_group_mbid=? WHERE id=? AND release_group_mbid IS NULL`, rr.GroupID, target); err != nil {
				return err
			}
		}
		if len(rr.Artists) > 0 {
			var cs []identity.Credit
			for _, a := range rr.Artists {
				c := identity.CreditFor(t.Provider, a.ProviderID, a.Name)
				c.JoinPhrase = a.JoinPhrase
				cs = append(cs, c)
			}
			if err := identity.FillReleaseArtists(ctx, tx, target, cs); err != nil {
				return err
			}
			if err := fillCreditedNames(ctx, tx, identity.KindRelease, target, t.Provider, rr.Artists); err != nil {
				return err
			}
		}
	}
	return nil
}

// releaseBarcodes are the barcodes our release holds (a release can hold more
// than one: catalogues sometimes sell one release under two).
func releaseBarcodes(ctx context.Context, tx *sql.Tx, release int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT provider_id FROM external_link WHERE entity_type='release' AND entity_id=?
		AND provider=? AND status<>'candidate'`, release, identity.UPCProv)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// sameReleaseOtherBarcode reports whether album, whose barcode our release does
// not hold, is nevertheless our release under a second barcode: the album that
// gave our release its barcode (among those fetched now) has the same release
// date and the same track count. Different editions differ in at least one of
// the two; a different barcode with both equal is a distributor's second code.
func sameReleaseOtherBarcode(albums map[string]providers.Album, held []string, alb providers.Album) bool {
	if alb.TotalTracks <= 0 {
		return false
	}
	for _, x := range albums {
		if x.ProviderID == alb.ProviderID || !contains(held, providers.NormalizeBarcode(x.UPC)) {
			continue
		}
		if agree, ok := match.DatesAgree(x.Date, alb.Date); ok && agree && x.TotalTracks == alb.TotalTracks {
			return true
		}
	}
	return false
}

// attachOrFlag attaches ref to the entity. When another entity of the same
// kind already owns the id, nothing is moved and the pair is flagged for the
// manual merge queue.
func attachOrFlag(ctx context.Context, tx *sql.Tx, kind string, entity int64, ref identity.Ref, reason string) error {
	if _, _, err := identity.AttachIDs(ctx, tx, kind, entity, []identity.Ref{ref}); err != nil {
		return err
	}
	owner, err := identity.FindByLink(ctx, tx, kind, ref.Provider, ref.ID)
	if err != nil {
		return err
	}
	if owner != 0 && owner != entity {
		return identity.Candidate(ctx, tx, kind, entity, owner, reason)
	}
	return nil
}

func holds(ctx context.Context, tx *sql.Tx, kind string, entity int64, ref identity.Ref) (bool, error) {
	owner, err := identity.FindByLink(ctx, tx, kind, ref.Provider, ref.ID)
	return owner == entity && owner != 0, err
}
