package stats

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/artwork"
	"younithm/internal/identity"
	"younithm/internal/names"
)

// Library pages: one artist, album (release) or track (recording) with its
// plays, chart and related entities.
//
//	GET /artists/{id}
//	GET /releases/{id}
//	GET /recordings/{id}
//	GET /recordings/{id}/history   recent plays (behind the "recent" gate)
//	GET /library/{kind}             lists (browse.go)

// MountLibrary registers the entity routes. page gates the pages; history
// gates a track's play history, which is as revealing as recent listens.
func (h *Handler) MountLibrary(r chi.Router, page, history func(http.Handler) http.Handler) {
	if page == nil {
		page = ident
	}
	if history == nil {
		history = ident
	}
	r.With(page).Get("/artists/{id}", h.artist)
	r.With(page).Get("/releases/{id}", h.release)
	r.With(page).Get("/recordings/{id}", h.recording)
	r.With(page, history).Get("/recordings/{id}/history", h.recordingHistory)
	r.With(page).Get("/library/{kind}", h.browse)
}

type point struct {
	Label string `json:"label"`
	Plays int64  `json:"plays"`
}

type chart struct {
	Month []point `json:"month"` // last 30 days, per day
	Year  []point `json:"year"`  // last 12 months, per month
	All   []point `json:"all"`   // every year since the first play
}

type credit struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	JoinPhrase string `json:"join_phrase,omitempty"`
}

type span struct {
	Plays int64   `json:"plays"`
	First *string `json:"first"`
	Last  *string `json:"last"`
}

func entityID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		apiError(w, 400, "invalid id")
		return 0, false
	}
	return id, true
}

func fail(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not found")
		return
	}
	log.Printf("stats: %s: %v", what, err)
	apiError(w, 500, "internal error")
}

// buildChart buckets per-day counts (scrobbles matching where) into the three
// chart ranges.
func (h *Handler) buildChart(ctx context.Context, where string, args ...any) (chart, error) {
	rows, err := h.read.QueryContext(ctx, `SELECT substr(s.played_at, 1, 10), COUNT(*) FROM scrobble s
WHERE s.user_id = 1 AND `+where+` GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return chart{}, err
	}
	defer rows.Close()
	perDay := map[string]int64{}
	perMonth := map[string]int64{}
	perYear := map[string]int64{}
	first := ""
	for rows.Next() {
		var d string
		var n int64
		if err := rows.Scan(&d, &n); err != nil {
			return chart{}, err
		}
		if first == "" {
			first = d
		}
		perDay[d] += n
		if len(d) >= 7 {
			perMonth[d[:7]] += n
			perYear[d[:4]] += n
		}
	}
	if err := rows.Err(); err != nil {
		return chart{}, err
	}
	now := h.now().UTC()
	var c chart
	for i := 29; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		c.Month = append(c.Month, point{d, perDay[d]})
	}
	m := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 11; i >= 0; i-- {
		k := m.AddDate(0, -i, 0).Format("2006-01")
		c.Year = append(c.Year, point{k, perMonth[k]})
	}
	startYear := now.Year()
	if len(first) >= 4 {
		if y, err := strconv.Atoi(first[:4]); err == nil && y < startYear {
			startYear = y
		}
	}
	for y := startYear; y <= now.Year(); y++ {
		k := strconv.Itoa(y)
		c.All = append(c.All, point{k, perYear[k]})
	}
	return c, nil
}

// links returns provider -> id for an entity's accepted links (not candidates).
func (h *Handler) links(ctx context.Context, kind string, id int64) (map[string]string, error) {
	rows, err := h.read.QueryContext(ctx, `SELECT provider, provider_id FROM external_link
WHERE entity_type = ? AND entity_id = ? AND status IN ('confirmed', 'auto') AND match_method <> 'mapping'
ORDER BY status = 'confirmed' DESC, pinned DESC, confidence DESC, id`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, pid string
		if err := rows.Scan(&p, &pid); err != nil {
			return nil, err
		}
		if _, ok := out[p]; !ok {
			out[p] = pid
		}
	}
	return out, rows.Err()
}

func (h *Handler) aliases(ctx context.Context, kind string, id int64, name string) ([]string, error) {
	// Alias rows hold the normalised lookup key ("enter shikari"), not the name as
	// written, so the entity's own name has to be compared in that form.
	if kind != "recording" { // recording keys also carry the artist, so they never equal a title
		name = identity.Norm(name)
	}
	rows, err := h.read.QueryContext(ctx, `SELECT raw_string FROM alias WHERE kind = ? AND entity_id = ?
AND raw_string <> ? ORDER BY raw_string LIMIT 20`, kind, id, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (h *Handler) credits(ctx context.Context, q string, id int64) ([]credit, error) {
	rows, err := h.read.QueryContext(ctx, q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []credit{}
	for rows.Next() {
		var c credit
		var jp sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &jp); err != nil {
			return nil, err
		}
		c.JoinPhrase = jp.String
		out = append(out, c)
	}
	return out, rows.Err()
}

const (
	recordingCredits = `SELECT a.id, a.name, ra.join_phrase FROM recording_artist ra JOIN artist a ON a.id = ra.artist_id
WHERE ra.recording_id = ? ORDER BY ra.position, a.id`
	releaseCredits = `SELECT a.id, a.name, ra.join_phrase FROM release_artist ra JOIN artist a ON a.id = ra.artist_id
WHERE ra.release_id = ? ORDER BY ra.position, a.id`
	// creditLine is the joined artist credit of recording rec.id.
	creditLine = `COALESCE((SELECT group_concat(name, ', ') FROM (
  SELECT a.name FROM recording_artist ra JOIN artist a ON a.id = ra.artist_id
  WHERE ra.recording_id = rec.id ORDER BY ra.position, a.id)), '')`
)

// ---- Artist ----

type artistTrack struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Plays  int64  `json:"plays"`
	Image  string `json:"image,omitempty"`
}

type artistRelease struct {
	ID    int64   `json:"id"`
	Title string  `json:"title"`
	Date  *string `json:"date"`
	Type  *string `json:"type"`
	Plays int64   `json:"plays"`
	Image string  `json:"image,omitempty"`
}

func (h *Handler) artist(w http.ResponseWriter, r *http.Request) {
	id, ok := entityID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var name string
	if err := h.read.QueryRowContext(ctx, `SELECT name FROM artist WHERE id = ?`, id).Scan(&name); err != nil {
		fail(w, "artist", err)
		return
	}
	// Plays: every listen of a recording the artist is credited on.
	const mine = `s.recording_id IN (SELECT recording_id FROM recording_artist WHERE artist_id = ?)`
	var sp span
	if err := h.read.QueryRowContext(ctx, `SELECT COUNT(*), MIN(s.played_at), MAX(s.played_at) FROM scrobble s
WHERE s.user_id = 1 AND `+mine, id).Scan(&sp.Plays, &sp.First, &sp.Last); err != nil {
		fail(w, "artist plays", err)
		return
	}
	// Rank among artists, counted the way the top list counts (primary credits).
	var rank int64
	if sp.Plays > 0 {
		if err := h.read.QueryRowContext(ctx, `
WITH p AS (SELECT ra.artist_id, COUNT(*) AS n FROM scrobble s
  JOIN recording_artist ra ON ra.recording_id = s.recording_id AND ra.role = 'primary'
  WHERE s.user_id = 1 GROUP BY ra.artist_id)
SELECT 1 + COUNT(*) FROM p WHERE n > COALESCE((SELECT n FROM p WHERE artist_id = ?), 0)`, id).Scan(&rank); err != nil {
			fail(w, "artist rank", err)
			return
		}
	}
	tracks, err := h.artistTracks(ctx, `
SELECT rec.id, rec.title, `+creditLine+`, COUNT(s.id) AS plays
FROM recording rec JOIN recording_artist me ON me.recording_id = rec.id AND me.artist_id = ?1
LEFT JOIN scrobble s ON s.recording_id = rec.id AND s.user_id = 1
GROUP BY rec.id ORDER BY plays DESC, rec.title LIMIT 10`, id)
	if err != nil {
		fail(w, "artist tracks", err)
		return
	}
	// Tracks where someone else leads the credit.
	appears, err := h.artistTracks(ctx, `
SELECT rec.id, rec.title, `+creditLine+`, COUNT(s.id) AS plays
FROM recording rec JOIN recording_artist me ON me.recording_id = rec.id AND me.artist_id = ?1
LEFT JOIN scrobble s ON s.recording_id = rec.id AND s.user_id = 1
WHERE NOT EXISTS (SELECT 1 FROM release_artist rla JOIN recording_release rr ON rr.release_id = rla.release_id
    WHERE rr.recording_id = rec.id AND rla.artist_id = ?1)
  AND (SELECT ra.artist_id FROM recording_artist ra WHERE ra.recording_id = rec.id ORDER BY ra.position, ra.artist_id LIMIT 1) <> ?1
GROUP BY rec.id ORDER BY plays DESC, rec.title LIMIT 10`, id)
	if err != nil {
		fail(w, "artist appears on", err)
		return
	}
	rows, err := h.read.QueryContext(ctx, `
SELECT rel.id, rel.title, rel.release_date, rel.release_type,
  (SELECT COUNT(*) FROM scrobble s WHERE s.release_id = rel.id AND s.user_id = 1) AS plays, `+artwork.ReleaseImage("rel")+`
FROM release rel JOIN release_artist me ON me.release_id = rel.id AND me.artist_id = ?
ORDER BY plays DESC, rel.release_date DESC, rel.title LIMIT 50`, id)
	if err != nil {
		fail(w, "artist releases", err)
		return
	}
	albums := []artistRelease{}
	for rows.Next() {
		var a artistRelease
		var path *string
		if err := rows.Scan(&a.ID, &a.Title, &a.Date, &a.Type, &a.Plays, &path); err != nil {
			rows.Close()
			fail(w, "artist releases", err)
			return
		}
		if path != nil {
			a.Image = artwork.URL(artwork.KindRelease, a.ID, *path)
		}
		albums = append(albums, a)
	}
	rows.Close()
	c, err := h.buildChart(ctx, mine, id)
	if err != nil {
		fail(w, "artist chart", err)
		return
	}
	links, err := h.links(ctx, "artist", id)
	if err != nil {
		fail(w, "artist links", err)
		return
	}
	al, err := h.aliases(ctx, "artist", id, name)
	if err != nil {
		fail(w, "artist aliases", err)
		return
	}
	for _, ts := range [][]artistTrack{tracks, appears} {
		for i := range ts {
			ts[i].Image = h.recordingImage(ctx, ts[i].ID)
		}
	}
	stored := name
	if h.preferring(ctx) {
		name = h.disp(ctx, names.Artist, id, name)
		for _, ts := range [][]artistTrack{tracks, appears} {
			for i := range ts {
				ts[i].Title = h.disp(ctx, names.Recording, ts[i].ID, ts[i].Title)
				ts[i].Artist = h.line(ctx, names.Recording, ts[i].ID, ts[i].Artist)
			}
		}
		for i := range albums {
			albums[i].Title = h.disp(ctx, names.Release, albums[i].ID, albums[i].Title)
		}
	}
	al = h.alsoKnownAs(ctx, names.Artist, id, stored, name, al)
	writeJSON(w, 200, map[string]any{
		"id": id, "name": name, "display_name": h.override(ctx, "artist", id), "image_custom": h.customImage(ctx, "artist", id), "image": h.artistImage(ctx, id), "aliases": al, "plays": sp.Plays, "first": sp.First, "last": sp.Last,
		"rank": rank, "links": links, "chart": c, "top_tracks": tracks, "albums": albums, "appears_on": appears,
	})
}

func (h *Handler) artistTracks(ctx context.Context, q string, id int64) ([]artistTrack, error) {
	rows, err := h.read.QueryContext(ctx, q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []artistTrack{}
	for rows.Next() {
		var t artistTrack
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Plays); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- Release ----

type releaseTrack struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Disc       int64  `json:"disc"`
	Position   *int64 `json:"position"`
	DurationMs *int64 `json:"duration_ms"`
	Plays      int64  `json:"plays"` // plays from this release
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) {
	id, ok := entityID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var title string
	var date, typ *string
	if err := h.read.QueryRowContext(ctx, `SELECT title, release_date, release_type FROM release WHERE id = ?`, id).
		Scan(&title, &date, &typ); err != nil {
		fail(w, "release", err)
		return
	}
	var sp span
	var playsAny int64
	if err := h.read.QueryRowContext(ctx, `SELECT COUNT(*), MIN(played_at), MAX(played_at) FROM scrobble
WHERE user_id = 1 AND release_id = ?`, id).Scan(&sp.Plays, &sp.First, &sp.Last); err != nil {
		fail(w, "release plays", err)
		return
	}
	if err := h.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM scrobble WHERE user_id = 1
AND recording_id IN (SELECT recording_id FROM recording_release WHERE release_id = ?)`, id).Scan(&playsAny); err != nil {
		fail(w, "release plays", err)
		return
	}
	rows, err := h.read.QueryContext(ctx, `
SELECT rec.id, rec.title, `+creditLine+`, rr.disc, rr.position, rec.duration_ms,
  (SELECT COUNT(*) FROM scrobble s WHERE s.recording_id = rec.id AND s.release_id = rr.release_id AND s.user_id = 1)
FROM recording_release rr JOIN recording rec ON rec.id = rr.recording_id
WHERE rr.release_id = ? ORDER BY rr.disc, rr.position IS NULL, rr.position, rec.title`, id)
	if err != nil {
		fail(w, "release tracks", err)
		return
	}
	tracks := []releaseTrack{}
	for rows.Next() {
		var t releaseTrack
		if err := rows.Scan(&t.ID, &t.Title, &t.Artist, &t.Disc, &t.Position, &t.DurationMs, &t.Plays); err != nil {
			rows.Close()
			fail(w, "release tracks", err)
			return
		}
		tracks = append(tracks, t)
	}
	rows.Close()
	artists, err := h.credits(ctx, releaseCredits, id)
	if err != nil {
		fail(w, "release artists", err)
		return
	}
	c, err := h.buildChart(ctx, `s.release_id = ?`, id)
	if err != nil {
		fail(w, "release chart", err)
		return
	}
	links, err := h.links(ctx, "release", id)
	if err != nil {
		fail(w, "release links", err)
		return
	}
	if h.preferring(ctx) {
		title = h.disp(ctx, names.Release, id, title)
		for i := range artists {
			artists[i].Name = h.disp(ctx, names.Artist, artists[i].ID, artists[i].Name)
		}
		for i := range tracks {
			tracks[i].Title = h.disp(ctx, names.Recording, tracks[i].ID, tracks[i].Title)
			tracks[i].Artist = h.line(ctx, names.Recording, tracks[i].ID, tracks[i].Artist)
		}
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "title": title, "display_name": h.override(ctx, "release", id), "image_custom": h.customImage(ctx, "release", id), "image": h.releaseImage(ctx, id), "date": date, "type": typ, "artists": artists, "links": links,
		"plays": sp.Plays, "plays_any": playsAny, "first": sp.First, "last": sp.Last, "tracks": tracks, "chart": c,
	})
}

// ---- Recording ----

type recordingRelease struct {
	ID      int64   `json:"id"`
	Title   string  `json:"title"`
	Date    *string `json:"date"`
	Type    *string `json:"type"`
	Primary bool    `json:"primary"`
	Plays   int64   `json:"plays"`
	Image   string  `json:"image,omitempty"`
}

func (h *Handler) recording(w http.ResponseWriter, r *http.Request) {
	id, ok := entityID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var title string
	var dur *int64
	if err := h.read.QueryRowContext(ctx, `SELECT title, duration_ms FROM recording WHERE id = ?`, id).Scan(&title, &dur); err != nil {
		fail(w, "recording", err)
		return
	}
	var sp span
	if err := h.read.QueryRowContext(ctx, `SELECT COUNT(*), MIN(played_at), MAX(played_at) FROM scrobble
WHERE user_id = 1 AND recording_id = ?`, id).Scan(&sp.Plays, &sp.First, &sp.Last); err != nil {
		fail(w, "recording plays", err)
		return
	}
	rows, err := h.read.QueryContext(ctx, `
SELECT rel.id, rel.title, rel.release_date, rel.release_type, rr.is_primary,
  (SELECT COUNT(*) FROM scrobble s WHERE s.recording_id = rr.recording_id AND s.release_id = rel.id AND s.user_id = 1) AS plays
FROM recording_release rr JOIN release rel ON rel.id = rr.release_id
WHERE rr.recording_id = ? ORDER BY rr.is_primary DESC, plays DESC, rel.release_date, rel.id`, id)
	if err != nil {
		fail(w, "recording releases", err)
		return
	}
	rels := []recordingRelease{}
	for rows.Next() {
		var x recordingRelease
		if err := rows.Scan(&x.ID, &x.Title, &x.Date, &x.Type, &x.Primary, &x.Plays); err != nil {
			rows.Close()
			fail(w, "recording releases", err)
			return
		}
		rels = append(rels, x)
	}
	rows.Close()
	for i := range rels {
		rels[i].Image = h.releaseImage(ctx, rels[i].ID)
	}
	artists, err := h.credits(ctx, recordingCredits, id)
	if err != nil {
		fail(w, "recording artists", err)
		return
	}
	c, err := h.buildChart(ctx, `s.recording_id = ?`, id)
	if err != nil {
		fail(w, "recording chart", err)
		return
	}
	links, err := h.links(ctx, "recording", id)
	if err != nil {
		fail(w, "recording links", err)
		return
	}
	al, err := h.aliases(ctx, "recording", id, title)
	if err != nil {
		fail(w, "recording aliases", err)
		return
	}
	var variants []names.Variant
	if h.names != nil {
		variants = h.names.RecordingVariants(ctx, id, title)
	}
	if variants == nil {
		variants = []names.Variant{}
	}
	if h.preferring(ctx) {
		title = h.disp(ctx, names.Recording, id, title)
		for i := range artists {
			artists[i].Name = h.disp(ctx, names.Artist, artists[i].ID, artists[i].Name)
		}
		for i := range rels {
			rels[i].Title = h.disp(ctx, names.Release, rels[i].ID, rels[i].Title)
		}
	}
	writeJSON(w, 200, map[string]any{
		"names": variants,
		"id":    id, "title": title, "display_name": h.override(ctx, "recording", id), "image": h.recordingImage(ctx, id), "duration_ms": dur, "artists": artists, "aliases": al, "links": links,
		"plays": sp.Plays, "first": sp.First, "last": sp.Last, "releases": rels, "chart": c,
	})
}

type historyItem struct {
	PlayedAt  string  `json:"played_at"`
	ReleaseID *int64  `json:"release_id"`
	Album     *string `json:"album"`
}

func (h *Handler) recordingHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := entityID(w, r)
	if !ok {
		return
	}
	limit, ok := parseLimit(r, 20, 100)
	if !ok {
		apiError(w, 400, "limit must be 1..100")
		return
	}
	rows, err := h.read.QueryContext(r.Context(), `
SELECT s.played_at, s.release_id, COALESCE(rel.title, NULLIF(s.raw_album, ''))
FROM scrobble s LEFT JOIN release rel ON rel.id = s.release_id
WHERE s.user_id = 1 AND s.recording_id = ? ORDER BY s.played_at DESC, s.id DESC LIMIT ?`, id, limit)
	if err != nil {
		fail(w, "recording history", err)
		return
	}
	defer rows.Close()
	items := []historyItem{}
	for rows.Next() {
		var it historyItem
		if err := rows.Scan(&it.PlayedAt, &it.ReleaseID, &it.Album); err != nil {
			fail(w, "recording history", err)
			return
		}
		items = append(items, it)
	}
	rows.Close()
	for i := range items {
		if items[i].ReleaseID != nil && items[i].Album != nil {
			a := h.disp(r.Context(), names.Release, *items[i].ReleaseID, *items[i].Album)
			items[i].Album = &a
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// override is the display name set by hand for the entity, or nil.
func (h *Handler) override(ctx context.Context, kind string, id int64) *string {
	if h.names == nil {
		return nil
	}
	if o := h.names.Override(ctx, kind, id); o != "" {
		return &o
	}
	return nil
}

// customImage reports whether the entity shows an image chosen by hand. table
// is artist or release (never user input).
func (h *Handler) customImage(ctx context.Context, table string, id int64) bool {
	var n int
	_ = h.read.QueryRowContext(ctx, `SELECT COALESCE(image_override, '') <> '' FROM `+table+` WHERE id = ?`, id).Scan(&n)
	return n > 0
}

// alsoKnownAs is every other name the entity goes by: the catalogues' names in
// the preferred languages, the stored name, then the spellings listens came in
// with (alias keys, already normalised) that none of those cover. The shown
// name is left out, and names that differ only in case or spacing count once.
func (h *Handler) alsoKnownAs(ctx context.Context, kind string, id int64, stored, shown string, keys []string) []string {
	seen := map[string]bool{identity.Norm(shown): true}
	out := []string{}
	add := func(n string) {
		k := identity.Norm(n)
		if strings.TrimSpace(n) == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, n)
	}
	if h.names != nil {
		for _, n := range h.names.Names(ctx, kind, id, stored) {
			add(n)
		}
	}
	add(stored)
	for _, k := range keys {
		add(k)
	}
	return out
}
