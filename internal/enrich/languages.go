package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"younithm/internal/names"
	"younithm/internal/providers"
)

// The names pass: links artists across catalogues by the ids the catalogues
// credit, then fills names in the admin's preferred languages. Names are asked
// for in batches, and only what is missing is asked: a library of 1,000
// artists costs a few hundred requests once, then a few per new artist.
//
// Linking (no requests: it reads the cached track answers). Catalogue ids are
// exact, but each catalogue numbers artists its own way, so an Apple artist id
// says nothing about which Spotify id or which artist of ours it is. Two
// catalogues' answers for the same track settle it when their credits leave
// one pairing: every other credit on both sides is an artist we already hold
// by id, and exactly one credit is left on each side, one of them ours by id.
// The other one is then linked to that artist (match_method isrc: the track
// pairing it rests on). A track with one credited artist on each side is the
// plain case. Names, credit order and our own (as-scrobbled) credits play no
// part. Only tracks linked by id or ISRC are read, and the artist linked must
// be credited on our track. An id two tracks would link to different artists,
// or an artist that would get a second id in one catalogue, is left alone.
// Each new link can settle further tracks, so the pass repeats until none do.
//
// Names. For each preferred language: MusicBrainz aliases for artists (100 per
// search), and Apple Music and Spotify names for artists, albums and tracks
// (Apple 50/100/300 per request, Spotify 50/20/50). Each catalogue's answer
// is stored apart (entity_name, one row per provider); an empty name records
// that it was asked and had none. Nothing here changes a stored name.

// NamesNow makes Run do a names pass when the queue is next idle.
func (m *Manager) NamesNow() {
	m.namesDue.Store(true)
	m.Wake()
}

// NamePass links artists, then fills names. A rate limit or a provider that is
// not configured ends that part of the pass early; the next pass resumes.
func (m *Manager) NamePass(ctx context.Context) error {
	if err := m.linkArtists(ctx); err != nil && !skippable(err) {
		return fmt.Errorf("linking artists: %w", err)
	}
	langs := names.ReadLanguages(ctx, m.db, names.LanguagesKey)
	if err := m.fillAliases(ctx, langs); err != nil && !skippable(err) {
		return fmt.Errorf("artist names: %w", err)
	}
	for _, p := range []string{providers.AppleMusic, providers.Spotify} {
		if err := m.fillCatalogueNames(ctx, p, langs); err != nil && !skippable(err) {
			return fmt.Errorf("%s names: %w", p, err)
		}
	}
	return nil
}

// retrying calls fn again after a short rate limit (MusicBrainz answers 503
// when busy, often), waiting what the server asks, up to 30s, at most 4 times.
// A longer wait ends the attempt; the next pass resumes.
func retrying[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	for i := 0; ; i++ {
		v, err := fn()
		var rl *providers.RateLimitedError
		if !errors.As(err, &rl) || i == 4 || rl.RetryAfter > 30 {
			return v, err
		}
		wait := time.Duration(rl.RetryAfter) * time.Second
		if wait <= 0 {
			wait = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func skippable(err error) bool {
	var rl *providers.RateLimitedError
	return errors.Is(err, providers.ErrNotConfigured) || errors.Is(err, providers.ErrUnsupported) || errors.As(err, &rl)
}

// ---- Linking ----

// creditSide is one catalogue's answer for one of our tracks.
type creditSide struct {
	provider string
	ids      []string // credited artist ids, deduplicated
}

type catID struct{ provider, id string }

// linkArtists links catalogue artist ids by the credits of tracks two
// catalogues answered for (see the top of this file). It makes no requests.
func (m *Manager) linkArtists(ctx context.Context) error {
	tracks, ours, err := m.creditSides(ctx)
	if err != nil {
		return err
	}
	for round := 0; round < 20; round++ {
		held, has, err := m.heldArtists(ctx)
		if err != nil {
			return err
		}
		// Every artist each unheld id could be, across all tracks.
		cands := map[catID]map[int64]bool{}
		for rec, sides := range tracks {
			for i := range sides {
				for j := i + 1; j < len(sides); j++ {
					if sides[i].provider == sides[j].provider {
						continue
					}
					id, artist, ok := pairLeft(sides[i], sides[j], held)
					if !ok || !ours[rec][artist] {
						continue
					}
					if cands[id] == nil {
						cands[id] = map[int64]bool{}
					}
					cands[id][artist] = true
				}
			}
		}
		// One artist per id, and one new id per artist and catalogue.
		perArtist := map[catID]int{} // (provider, artist) → new ids
		pick := map[catID]int64{}
		for id, as := range cands {
			if len(as) != 1 {
				continue
			}
			for a := range as {
				if has[catID{id.provider, fmt.Sprint(a)}] {
					continue
				}
				pick[id] = a
				perArtist[catID{id.provider, fmt.Sprint(a)}]++
			}
		}
		n := 0
		for id, a := range pick {
			if perArtist[catID{id.provider, fmt.Sprint(a)}] != 1 {
				continue
			}
			if _, err := m.db.ExecContext(ctx, `INSERT OR IGNORE INTO external_link
(entity_type, entity_id, provider, provider_id, match_method, confidence, status) VALUES ('artist', ?, ?, ?, 'isrc', 1.0, 'auto')`,
				a, id.provider, id.id); err != nil {
				return err
			}
			n++
		}
		if n == 0 {
			return nil
		}
		log.Printf("enrich: names: linked %d catalogue artist ids by track credits", n)
	}
	return nil
}

// pairLeft drops the credits both sides hold by id for the same artist of
// ours. When one credit is left on each side and exactly one of the two is
// ours by id, it returns the other and that artist.
func pairLeft(x, y creditSide, held map[catID]int64) (catID, int64, bool) {
	resolve := func(s creditSide) (map[int64]bool, []string) {
		known, unknown := map[int64]bool{}, []string(nil)
		for _, id := range s.ids {
			if a, ok := held[catID{s.provider, id}]; ok {
				known[a] = true
			} else {
				unknown = append(unknown, id)
			}
		}
		return known, unknown
	}
	kx, ux := resolve(x)
	ky, uy := resolve(y)
	var onlyX, onlyY []int64
	for a := range kx {
		if !ky[a] {
			onlyX = append(onlyX, a)
		}
	}
	for a := range ky {
		if !kx[a] {
			onlyY = append(onlyY, a)
		}
	}
	switch {
	case len(onlyX) == 1 && len(ux) == 0 && len(onlyY) == 0 && len(uy) == 1:
		return catID{y.provider, uy[0]}, onlyX[0], true
	case len(onlyY) == 1 && len(uy) == 0 && len(onlyX) == 0 && len(ux) == 1:
		return catID{x.provider, ux[0]}, onlyY[0], true
	}
	return catID{}, 0, false
}

// trustedTrackLink is a track link made by id or ISRC (or confirmed by hand):
// one whose answer is certainly our track.
const trustedTrackLink = `l.status IN ('confirmed', 'auto') AND (l.status = 'confirmed'
  OR l.match_method IN ('isrc', 'lookup', 'submitted_id', 'submitted_mbid', 'manual'))`

// creditSides reads, for each of our tracks with a trusted link, every
// catalogue's credited artist ids, and the artists our track credits.
func (m *Manager) creditSides(ctx context.Context) (map[int64][]creditSide, map[int64]map[int64]bool, error) {
	rows, err := m.db.QueryContext(ctx, `
SELECT l.entity_id, l.provider, l.provider_id, s.raw_json FROM external_link l
JOIN provider_snapshot s ON s.provider = l.provider AND s.provider_id = l.provider_id AND s.kind = 'recording' AND s.status = 'ok'
WHERE l.entity_type = 'recording' AND l.provider IN ('musicbrainz', 'spotify', 'apple') AND `+trustedTrackLink+`
ORDER BY l.entity_id, l.provider, l.provider_id`)
	if err != nil {
		return nil, nil, err
	}
	tracks := map[int64][]creditSide{}
	for rows.Next() {
		var rec int64
		var prov, pid string
		var raw []byte
		if err := rows.Scan(&rec, &prov, &pid, &raw); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var t providers.Track
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		side := creditSide{provider: prov}
		seen := map[string]bool{}
		for _, a := range t.Artists {
			if a.ProviderID != "" && !seen[a.ProviderID] {
				seen[a.ProviderID] = true
				side.ids = append(side.ids, a.ProviderID)
			}
		}
		if len(side.ids) > 0 {
			tracks[rec] = append(tracks[rec], side)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for rec, sides := range tracks {
		if len(sides) < 2 {
			delete(tracks, rec)
		}
	}
	ours := map[int64]map[int64]bool{}
	rows, err = m.db.QueryContext(ctx, `SELECT recording_id, artist_id FROM recording_artist`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rec, a int64
		if err := rows.Scan(&rec, &a); err != nil {
			return nil, nil, err
		}
		if _, ok := tracks[rec]; !ok {
			continue
		}
		if ours[rec] == nil {
			ours[rec] = map[int64]bool{}
		}
		ours[rec][a] = true
	}
	return tracks, ours, rows.Err()
}

// heldArtists maps each catalogue artist id held by exactly one of our
// artists to it, and records which catalogues each artist holds an id in
// (as catID{provider, artist id}).
func (m *Manager) heldArtists(ctx context.Context) (map[catID]int64, map[catID]bool, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT provider, provider_id, entity_id FROM external_link
WHERE entity_type = 'artist' AND status IN ('confirmed', 'auto')`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	held, has := map[catID]int64{}, map[catID]bool{}
	shared := map[catID]bool{}
	for rows.Next() {
		var prov, pid string
		var a int64
		if err := rows.Scan(&prov, &pid, &a); err != nil {
			return nil, nil, err
		}
		k := catID{prov, pid}
		if b, ok := held[k]; ok && b != a {
			shared[k] = true
		}
		held[k] = a
		has[catID{prov, fmt.Sprint(a)}] = true
	}
	// An id two of our artists hold says nothing certain about either.
	for k := range shared {
		held[k] = -1
	}
	return held, has, rows.Err()
}

// ---- Names ----

// langScore says how well a catalogue's tag serves a wanted one: 2 for the
// same tag, 1 when either is the other's prefix ("en" and "en-US"), else 0.
func langScore(want, have string) int {
	w, h := strings.ToLower(want), strings.ToLower(have)
	switch {
	case w == h:
		return 2
	case strings.HasPrefix(h, w+"-"), strings.HasPrefix(w, h+"-"):
		return 1
	}
	return 0
}

// fillAliases stores MusicBrainz's names by language for artists holding a
// MusicBrainz id that miss one of the languages.
func (m *Manager) fillAliases(ctx context.Context, langs []string) error {
	mbp, ok := m.matcher.Providers.Get(providers.MusicBrainz)
	if !ok {
		return nil
	}
	lk, ok := mbp.(providers.ArtistNamesLookup)
	if !ok {
		return nil
	}
	for round := 0; round < 100; round++ {
		todo, err := m.missing(ctx, "artist", providers.MusicBrainz, langs, lk.MaxArtists())
		if err != nil || len(todo) == 0 {
			return err
		}
		ids := make([]string, 0, len(todo))
		for pid := range todo {
			ids = append(ids, pid)
		}
		got, err := retrying(ctx, func() (map[string][]providers.LocalName, error) { return lk.ArtistNames(ctx, ids) })
		if err != nil {
			return err
		}
		var rows []nameRow
		for pid, ents := range todo {
			for _, lang := range langs {
				best, score := providers.LocalName{}, 0
				for _, n := range got[pid] {
					s := langScore(lang, n.Lang) * 2
					if n.Primary {
						s++
					}
					if s > score && langScore(lang, n.Lang) > 0 {
						best, score = n, s
					}
				}
				for _, e := range ents {
					rows = append(rows, nameRow{"artist", e, lang, providers.MusicBrainz, pid, best.Name, best.Primary, true})
				}
			}
		}
		if err := m.putNames(ctx, rows); err != nil {
			return err
		}
	}
	return nil
}

var entityKinds = []struct{ entity, provider string }{
	{"artist", providers.KindArtist}, {"release", providers.KindAlbum}, {"recording", providers.KindRecording},
}

// fillCatalogueNames stores a catalogue's names for every linked entity that
// misses a language, a batch of ids per request.
func (m *Manager) fillCatalogueNames(ctx context.Context, provider string, langs []string) error {
	p, ok := m.matcher.Providers.Get(provider)
	if !ok {
		return nil
	}
	ln, ok := p.(providers.LocalizedNames)
	if !ok {
		return nil
	}
	for _, k := range entityKinds {
		size := ln.BatchSize(k.provider)
		if size == 0 {
			continue
		}
		for _, lang := range langs {
			for round := 0; round < 500; round++ {
				todo, err := m.missing(ctx, k.entity, provider, []string{lang}, size)
				if err != nil {
					return err
				}
				if len(todo) == 0 {
					break
				}
				ids := make([]string, 0, len(todo))
				for pid := range todo {
					ids = append(ids, pid)
				}
				var auth bool
				got, err := retrying(ctx, func() (map[string]string, error) {
					g, a, err := ln.NamesIn(ctx, k.provider, ids, lang)
					auth = a
					return g, err
				})
				if errors.Is(err, providers.ErrUnsupported) {
					break // no request can be made in this language
				}
				if err != nil {
					return err
				}
				var rows []nameRow
				for pid, ents := range todo {
					for _, e := range ents {
						rows = append(rows, nameRow{k.entity, e, lang, provider, pid, got[pid], false, auth})
					}
				}
				if err := m.putNames(ctx, rows); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// missing returns up to limit catalogue ids (with the entities holding them)
// of kind whose name in one of langs from provider was never asked for, or
// was asked for another id than the one linked now.
func (m *Manager) missing(ctx context.Context, kind, provider string, langs []string, limit int) (map[string][]int64, error) {
	if len(langs) == 0 || limit <= 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(langs)), ",")
	args := []any{kind, provider}
	for _, l := range langs {
		args = append(args, l)
	}
	args = append(args, len(langs), limit)
	rows, err := m.db.QueryContext(ctx, `
WITH linked AS (
  SELECT l.entity_id, (SELECT l2.provider_id FROM external_link l2 WHERE l2.entity_type = l.entity_type AND l2.entity_id = l.entity_id
      AND l2.provider = l.provider AND l2.status IN ('confirmed', 'auto')
      ORDER BY l2.status = 'confirmed' DESC, l2.pinned DESC, l2.confidence DESC, l2.id LIMIT 1) AS pid
  FROM external_link l WHERE l.entity_type = ?1 AND l.provider = ?2 AND l.status IN ('confirmed', 'auto')
  GROUP BY l.entity_id)
SELECT entity_id, pid FROM linked k
WHERE (SELECT COUNT(*) FROM entity_name n WHERE n.entity_type = ?1 AND n.entity_id = k.entity_id AND n.provider = ?2
       AND n.provider_id = k.pid AND n.lang IN (`+ph+`)) < ?`+fmt.Sprint(len(langs)+3)+`
ORDER BY entity_id LIMIT ?`+fmt.Sprint(len(langs)+4), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	n := 0
	for rows.Next() {
		var id int64
		var pid string
		if err := rows.Scan(&id, &pid); err != nil {
			return nil, err
		}
		if _, seen := out[pid]; !seen {
			n++
		}
		out[pid] = append(out[pid], id)
		if n >= limit {
			break
		}
	}
	return out, rows.Err()
}

type nameRow struct {
	kind     string
	id       int64
	lang     string
	provider string
	pid      string
	name     string
	primary  bool
	auth     bool
}

func (m *Manager) putNames(ctx context.Context, rows []nameRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := m.stamp(m.o.Now())
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO entity_name
(entity_type, entity_id, lang, provider, provider_id, name, primary_name, authoritative, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (entity_type, entity_id, lang, provider) DO UPDATE SET provider_id = excluded.provider_id,
  name = excluded.name, primary_name = excluded.primary_name, authoritative = excluded.authoritative, created_at = excluded.created_at`,
			r.kind, r.id, r.lang, r.provider, r.pid, strings.TrimSpace(r.name), r.primary, r.auth, at); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("enrich: names: %d %s %s rows (%s)", len(rows), rows[0].provider, rows[0].kind, rows[0].lang)
	return nil
}
