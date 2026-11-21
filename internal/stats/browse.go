package stats

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"younithm/internal/artwork"
	"younithm/internal/identity"
	"younithm/internal/names"
)

// Library lists: every artist, album or track with at least one play.
//
//	GET /library/{kind}?sort=plays|name|last&q=&limit=&offset=
//
// Plays are counted the way the entity pages count them: an artist's are the
// listens of every recording it is credited on, an album's are the listens
// played from it, a track's are its own.

type browseItem struct {
	ID     int64   `json:"id"`
	Name   string  `json:"name"`
	Artist *string `json:"artist,omitempty"`
	// Artists are the album's or track's credits in order, so each can link.
	Artists []credit `json:"artists,omitempty"`
	AlbumID *int64   `json:"album_id,omitempty"`
	Album   *string  `json:"album,omitempty"`
	Plays   int64    `json:"plays"`
	Last    string   `json:"last_played"`
	Image   string   `json:"image,omitempty"`
	artPath *string
}

// browseQuery returns the columns id, name, artist, album_id, album, n (plays),
// last (last played) and art (artwork path), plus the artwork kind ("" for a
// track, whose image is its album's).
func browseQuery(kind string) (string, string, bool) {
	switch kind {
	case "artists":
		return `
SELECT a.id AS id, a.name AS name, NULL AS artist, NULL AS album_id, NULL AS album, p.n AS n, p.last AS last, ` + artwork.ArtistImage("a") + ` AS art FROM (
  SELECT ra.artist_id AS id, COUNT(DISTINCT s.id) AS n, MAX(s.played_at) AS last
  FROM scrobble s JOIN recording_artist ra ON ra.recording_id = s.recording_id
  WHERE s.user_id = 1 GROUP BY ra.artist_id) p
JOIN artist a ON a.id = p.id`, artwork.KindArtist, true
	case "albums":
		return `
SELECT rel.id AS id, rel.title AS name, (SELECT group_concat(name, ', ') FROM (
    SELECT a.name FROM release_artist ra JOIN artist a ON a.id = ra.artist_id
    WHERE ra.release_id = rel.id ORDER BY ra.position, a.id)) AS artist,
  NULL AS album_id, NULL AS album, p.n AS n, p.last AS last, ` + artwork.ReleaseImage("rel") + ` AS art FROM (
  SELECT release_id AS id, COUNT(*) AS n, MAX(played_at) AS last
  FROM scrobble WHERE user_id = 1 AND release_id IS NOT NULL GROUP BY release_id) p
JOIN release rel ON rel.id = p.id`, artwork.KindRelease, true
	case "tracks":
		return `
SELECT rec.id AS id, rec.title AS name, ` + creditLine + ` AS artist, pr.id AS album_id, pr.title AS album,
  p.n AS n, p.last AS last, NULL AS art FROM (
  SELECT recording_id AS id, COUNT(*) AS n, MAX(played_at) AS last
  FROM scrobble WHERE user_id = 1 AND recording_id IS NOT NULL GROUP BY recording_id) p
JOIN recording rec ON rec.id = p.id
LEFT JOIN release pr ON pr.id = (SELECT rr.release_id FROM recording_release rr
  WHERE rr.recording_id = rec.id ORDER BY rr.is_primary DESC, rr.release_id LIMIT 1)`, "", true
	}
	return "", "", false
}

var browseOrder = map[string]string{
	"plays": "n DESC, shown COLLATE NOCASE, id",
	"name":  "shown COLLATE NOCASE, id",
	"last":  "last DESC, id",
}

// browseEntity is the entity_name type of each list, and the table crediting
// its artists ("" for artists themselves).
var browseEntity = map[string][2]string{
	"artists": {"artist", ""},
	"albums":  {"release", "release_artist ra WHERE ra.release_id = t.id"},
	"tracks":  {"recording", "recording_artist ra WHERE ra.recording_id = t.id"},
}

// shownName is the name the display picks (names.Source.Display): a display
// name set by hand; else the first source in the priority order with a name in a preferred language, the first
// such language within it; else the stored name. ?4 is the languages as
// ",en,ja," and ?5 the sources ranked before the stored name as
// ",spotify,apple," ("" for both when catalogue names are not shown). A
// source's untranslated answer is not here, so sort and search can differ
// from the display for an entity a source names only that way.
func shownName(entity string) string {
	return `COALESCE((SELECT d.display_name FROM ` + entity + ` d WHERE d.id = t.id), (SELECT n.name FROM entity_name n WHERE n.entity_type = '` + entity + `' AND n.entity_id = t.id
    AND n.name <> '' AND instr(?4, ',' || n.lang || ',') > 0 AND instr(?5, ',' || n.provider || ',') > 0
  ORDER BY instr(?5, ',' || n.provider || ','), instr(?4, ',' || n.lang || ',') LIMIT 1), t.name)`
}

// nameMatches is true when the entity at id has a catalogue name or a display
// name like ?1, or an alias like ?6. Alias rows hold the normalised key, so ?6
// is the search normalised the same way.
func nameMatches(entity, id string) string {
	return `EXISTS (SELECT 1 FROM entity_name n WHERE n.entity_type = '` + entity + `' AND n.entity_id = ` + id + ` AND n.name LIKE ?1 ESCAPE '\')
  OR EXISTS (SELECT 1 FROM ` + entity + ` d WHERE d.id = ` + id + ` AND d.display_name LIKE ?1 ESCAPE '\')
  OR EXISTS (SELECT 1 FROM alias al WHERE al.kind = '` + entity + `' AND al.entity_id = ` + id + ` AND al.raw_string LIKE ?6 ESCAPE '\')`
}

func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

func (h *Handler) browse(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	inner, artKind, ok := browseQuery(kind)
	if !ok {
		apiError(w, 404, "kind must be artists, albums or tracks")
		return
	}
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "plays"
	}
	order, ok := browseOrder[sort]
	if !ok {
		apiError(w, 400, "sort must be plays, name or last")
		return
	}
	limit, ok := parseLimit(r, 60, 200)
	if !ok {
		apiError(w, 400, "limit must be 1..200")
		return
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			apiError(w, 400, "offset must be 0 or more")
			return
		}
		offset = n
	}
	pattern, aliasPattern := "", ""
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		pattern = likePattern(q)
		aliasPattern = likePattern(identity.Norm(q))
	}
	// Name, artist and album match the search, as stored, as a display name
	// set by hand, in any language a catalogue names the entity or its
	// artists in, or as any spelling listens came in with (an alias).
	ent := browseEntity[kind]
	inNames := nameMatches(ent[0], "t.id")
	if ent[1] != "" {
		inNames += ` OR EXISTS (SELECT 1 FROM ` + ent[1] + ` AND (` + nameMatches("artist", "ra.artist_id") + `))`
	}
	where := `WHERE (?1 = '' OR t.name LIKE ?1 ESCAPE '\' OR COALESCE(t.artist, '') LIKE ?1 ESCAPE '\'
  OR COALESCE(t.album, '') LIKE ?1 ESCAPE '\' OR ` + inNames + `)`
	q := `SELECT t.id, t.name, t.artist, t.album_id, t.album, t.n, t.last, t.art, ` + shownName(ent[0]) + ` AS shown
FROM (` + inner + `) t ` + where
	langs, sources := "", ""
	if h.preferring(r.Context()) {
		langs = "," + strings.Join(h.names.Languages(r.Context()), ",") + ","
		for _, src := range h.names.Pref(r.Context()).Order {
			if src == names.Scrobbled {
				break
			}
			sources += "," + src
		}
		sources += ","
	}
	ctx := r.Context()
	var total int64
	if err := h.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+q+`)`, pattern, nil, nil, langs, sources, aliasPattern).Scan(&total); err != nil {
		fail(w, "library "+kind, err)
		return
	}
	rows, err := h.read.QueryContext(ctx, q+` ORDER BY `+order+` LIMIT ?2 OFFSET ?3`, pattern, limit, offset, langs, sources, aliasPattern)
	if err != nil {
		fail(w, "library "+kind, err)
		return
	}
	defer rows.Close()
	items := []browseItem{}
	for rows.Next() {
		var it browseItem
		var artist, album sql.NullString
		var shown string
		if err := rows.Scan(&it.ID, &it.Name, &artist, &it.AlbumID, &album, &it.Plays, &it.Last, &it.artPath, &shown); err != nil {
			fail(w, "library "+kind, err)
			return
		}
		if artist.Valid && artist.String != "" {
			it.Artist = &artist.String
		}
		if album.Valid {
			it.Album = &album.String
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		fail(w, "library "+kind, err)
		return
	}
	rows.Close()

	ek := map[string]string{"artists": names.Artist, "albums": names.Release, "tracks": names.Recording}[kind]
	pref := h.preferring(ctx)
	for i := range items {
		it := &items[i]
		switch {
		case artKind == "":
			it.Image = h.recordingImage(ctx, it.ID)
		case it.artPath != nil && *it.artPath != "":
			it.Image = artwork.URL(artKind, it.ID, *it.artPath)
		}
		if q := map[string]string{"albums": releaseCredits, "tracks": recordingCredits}[kind]; q != "" {
			cs, err := h.credits(ctx, q, it.ID)
			if err != nil {
				fail(w, "library "+kind+" artists", err)
				return
			}
			for j := range cs {
				cs[j].Name = h.disp(ctx, names.Artist, cs[j].ID, cs[j].Name)
			}
			it.Artists = cs
		}
		if !pref {
			continue
		}
		it.Name = h.disp(ctx, ek, it.ID, it.Name)
		if it.Artist != nil && kind != "artists" {
			a := h.line(ctx, ek, it.ID, *it.Artist)
			it.Artist = &a
		}
		if it.Album != nil && it.AlbumID != nil {
			a := h.disp(ctx, names.Release, *it.AlbumID, *it.Album)
			it.Album = &a
		}
	}
	writeJSON(w, 200, map[string]any{"kind": kind, "sort": sort, "total": total, "items": items})
}
