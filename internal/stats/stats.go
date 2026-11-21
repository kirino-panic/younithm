// Package stats serves the public read-only aggregates: recent listens,
// top artists/tracks/albums, and summary stats. Each endpoint sits behind its
// own visibility gate so the admin can hide it independently.
package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/names"
)

// timeFormat matches how scrobble.played_at is stored.
const timeFormat = "2006-01-02T15:04:05.000Z"

type Gates struct {
	Recent, Top, Stats func(http.Handler) http.Handler
}

type Handler struct {
	read  *sql.DB
	now   func() time.Time
	names *names.Source // display-name preference; nil = stored names
}

// WithNames shows names in the admin's preferred catalogue's wording.
func (h *Handler) WithNames(n *names.Source) *Handler { h.names = n; return h }

// preferring reports whether a catalogue's wording can replace stored names.
func (h *Handler) preferring(ctx context.Context) bool {
	return h.names.Active(ctx)
}

// disp is the display name of an entity (stored when no preference applies).
func (h *Handler) disp(ctx context.Context, kind string, id int64, stored string) string {
	if !h.preferring(ctx) {
		return stored
	}
	return h.names.Display(ctx, kind, id, stored)
}

// line is the display credit line of a recording or release.
func (h *Handler) line(ctx context.Context, kind string, id int64, stored string) string {
	if !h.preferring(ctx) {
		return stored
	}
	if l := h.names.CreditLine(ctx, kind, id); l != "" {
		return l
	}
	return stored
}

func New(read *sql.DB) *Handler { return &Handler{read: read, now: time.Now} }

func ident(h http.Handler) http.Handler { return h }

// Mount registers the routes on an /api/v1 router.
func (h *Handler) Mount(r chi.Router, g Gates) {
	for _, p := range []*func(http.Handler) http.Handler{&g.Recent, &g.Top, &g.Stats} {
		if *p == nil {
			*p = ident
		}
	}
	r.With(g.Recent).Get("/recent", h.recent)
	r.With(g.Top).Get("/top/{kind}", h.top)
	r.With(g.Stats).Get("/stats", h.summary)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// rangeStart returns the lower bound for a named range ("" = all time).
func (h *Handler) rangeStart(name string) (string, bool) {
	now := h.now().UTC()
	switch name {
	case "", "all":
		return "", true
	case "week":
		return now.AddDate(0, 0, -7).Format(timeFormat), true
	case "month":
		return now.AddDate(0, -1, 0).Format(timeFormat), true
	case "year":
		return now.AddDate(-1, 0, 0).Format(timeFormat), true
	case "latest_week":
		// The seven days up to the newest listen, for a library that has gone quiet.
		var last sql.NullString
		if err := h.read.QueryRow(`SELECT MAX(played_at) FROM scrobble WHERE user_id = 1`).Scan(&last); err == nil && last.Valid {
			if t, err := time.Parse(time.RFC3339Nano, last.String); err == nil {
				return t.UTC().AddDate(0, 0, -7).Format(timeFormat), true
			}
		}
		return "", true
	}
	return "", false
}

func parseLimit(r *http.Request, def, max int) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

type recentItem struct {
	ID       int64   `json:"id"`
	PlayedAt string  `json:"played_at"`
	Artist   string  `json:"artist"`
	Title    string  `json:"title"`
	Album    *string `json:"album"`
	// RecordingID is set once the listen is matched to a track.
	RecordingID *int64 `json:"recording_id"`
	ReleaseID   *int64 `json:"release_id"`
	Image       string `json:"image,omitempty"`
}

func (h *Handler) recent(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(r, 20, 100)
	if !ok {
		apiError(w, 400, "limit must be 1..100")
		return
	}
	rows, err := h.read.QueryContext(r.Context(), `
SELECT s.id, s.played_at,
  COALESCE(`+names.LineSQL(names.Recording, "s.recording_id")+`, s.raw_artist),
  COALESCE(rec.title, s.raw_title), COALESCE(rel.title, s.raw_album), s.recording_id, s.release_id
FROM scrobble s
LEFT JOIN recording rec ON rec.id = s.recording_id
LEFT JOIN release rel ON rel.id = s.release_id
WHERE s.user_id = 1 ORDER BY s.played_at DESC, s.id DESC LIMIT ?`, limit)
	if err != nil {
		log.Printf("stats: recent: %v", err)
		apiError(w, 500, "internal error")
		return
	}
	defer rows.Close()
	items := []recentItem{}
	for rows.Next() {
		var it recentItem
		if err := rows.Scan(&it.ID, &it.PlayedAt, &it.Artist, &it.Title, &it.Album, &it.RecordingID, &it.ReleaseID); err != nil {
			apiError(w, 500, "internal error")
			return
		}
		items = append(items, it)
	}
	rows.Close()
	for i := range items {
		it := &items[i]
		switch {
		case it.ReleaseID != nil:
			it.Image = h.releaseImage(r.Context(), *it.ReleaseID)
		case it.RecordingID != nil:
			it.Image = h.recordingImage(r.Context(), *it.RecordingID)
		}
	}
	if h.preferring(r.Context()) {
		ctx := r.Context()
		for i := range items {
			it := &items[i]
			if it.RecordingID != nil {
				it.Title = h.disp(ctx, names.Recording, *it.RecordingID, it.Title)
				it.Artist = h.line(ctx, names.Recording, *it.RecordingID, it.Artist)
			}
			if it.ReleaseID != nil && it.Album != nil {
				a := h.disp(ctx, names.Release, *it.ReleaseID, *it.Album)
				it.Album = &a
			}
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

type topItem struct {
	Name   string  `json:"name"`
	Artist *string `json:"artist,omitempty"`
	ID     *int64  `json:"id,omitempty"` // entity id when matched
	Plays  int64   `json:"plays"`
	Image  string  `json:"image,omitempty"`
}

// Matched entities group by id; unmatched scrobbles group by raw strings.
func topQuery(kind string) (string, bool) {
	switch kind {
	case "artists":
		return `
SELECT name, aid, NULL, COUNT(*) AS plays FROM (
  SELECT a.name AS name, a.id AS aid FROM scrobble s
    JOIN recording_artist ra ON ra.recording_id = s.recording_id AND ra.role = 'primary'
    JOIN artist a ON a.id = ra.artist_id
    WHERE s.user_id = 1 AND s.played_at >= ?1
  UNION ALL
  SELECT s.raw_artist, NULL FROM scrobble s
    WHERE s.user_id = 1 AND s.recording_id IS NULL AND s.played_at >= ?1
) GROUP BY COALESCE('a' || aid, 'r' || name) ORDER BY plays DESC, name LIMIT ?2`, true
	case "tracks":
		return `
SELECT COALESCE(rec.title, s.raw_title), s.recording_id,
  COALESCE(` + names.LineSQL(names.Recording, "s.recording_id") + `, s.raw_artist),
  COUNT(*) AS plays
FROM scrobble s LEFT JOIN recording rec ON rec.id = s.recording_id
WHERE s.user_id = 1 AND s.played_at >= ?1
GROUP BY COALESCE('r' || s.recording_id, 'u' || s.raw_artist || char(0) || s.raw_title)
ORDER BY plays DESC, 1 LIMIT ?2`, true
	case "albums":
		return `
SELECT COALESCE(rel.title, s.raw_album), s.release_id,
  COALESCE(` + names.LineSQL(names.Release, "s.release_id") + `, s.raw_artist),
  COUNT(*) AS plays
FROM scrobble s LEFT JOIN release rel ON rel.id = s.release_id
WHERE s.user_id = 1 AND s.played_at >= ?1 AND COALESCE(rel.title, s.raw_album) IS NOT NULL
  AND COALESCE(rel.title, s.raw_album) <> ''
GROUP BY COALESCE('r' || s.release_id, 'u' || s.raw_artist || char(0) || s.raw_album)
ORDER BY plays DESC, 1 LIMIT ?2`, true
	}
	return "", false
}

func (h *Handler) top(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	q, ok := topQuery(kind)
	if !ok {
		apiError(w, 404, "kind must be artists, tracks or albums")
		return
	}
	from, ok := h.rangeStart(r.URL.Query().Get("range"))
	if !ok {
		apiError(w, 400, "range must be week, month, year, all or latest_week")
		return
	}
	limit, ok := parseLimit(r, 10, 100)
	if !ok {
		apiError(w, 400, "limit must be 1..100")
		return
	}
	rows, err := h.read.QueryContext(r.Context(), q, from, limit)
	if err != nil {
		log.Printf("stats: top %s: %v", kind, err)
		apiError(w, 500, "internal error")
		return
	}
	defer rows.Close()
	items := []topItem{}
	for rows.Next() {
		var it topItem
		if err := rows.Scan(&it.Name, &it.ID, &it.Artist, &it.Plays); err != nil {
			apiError(w, 500, "internal error")
			return
		}
		items = append(items, it)
	}
	rows.Close()
	for i := range items {
		it := &items[i]
		if it.ID == nil {
			continue
		}
		switch kind {
		case "artists":
			it.Image = h.artistImage(r.Context(), *it.ID)
		case "albums":
			it.Image = h.releaseImage(r.Context(), *it.ID)
		case "tracks":
			it.Image = h.recordingImage(r.Context(), *it.ID)
		}
	}
	if h.preferring(r.Context()) {
		ctx := r.Context()
		ek := map[string]string{"artists": names.Artist, "tracks": names.Recording, "albums": names.Release}[kind]
		for i := range items {
			it := &items[i]
			if it.ID == nil {
				continue
			}
			it.Name = h.disp(ctx, ek, *it.ID, it.Name)
			if it.Artist != nil && ek != names.Artist {
				a := h.line(ctx, ek, *it.ID, *it.Artist)
				it.Artist = &a
			}
		}
	}
	writeJSON(w, 200, map[string]any{"kind": kind, "range": rangeName(r), "items": items})
}

func rangeName(r *http.Request) string {
	if v := r.URL.Query().Get("range"); v != "" {
		return v
	}
	return "all"
}

type day struct {
	Date  string `json:"date"`
	Plays int64  `json:"plays"`
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	from, ok := h.rangeStart(r.URL.Query().Get("range"))
	if !ok {
		apiError(w, 400, "range must be week, month, year or all")
		return
	}
	ctx := r.Context()
	var plays, tracks, artists, listenedMS int64
	err := h.read.QueryRowContext(ctx, `
SELECT COUNT(*),
  COUNT(DISTINCT COALESCE('r' || s.recording_id, 'u' || s.raw_artist || char(0) || s.raw_title)),
  COALESCE(SUM(COALESCE(s.duration_played_ms, rec.duration_ms)), 0)
FROM scrobble s LEFT JOIN recording rec ON rec.id = s.recording_id
WHERE s.user_id = 1 AND s.played_at >= ?`, from).Scan(&plays, &tracks, &listenedMS)
	if err == nil {
		err = h.read.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT COALESCE('a' || ra.artist_id, 'r' || s.raw_artist))
FROM scrobble s LEFT JOIN recording_artist ra
  ON ra.recording_id = s.recording_id AND ra.role = 'primary'
WHERE s.user_id = 1 AND s.played_at >= ?`, from).Scan(&artists)
	}
	var days []day
	if err == nil {
		var rows *sql.Rows
		rows, err = h.read.QueryContext(ctx, `
SELECT substr(played_at, 1, 10), COUNT(*) FROM scrobble
WHERE user_id = 1 AND played_at >= ? GROUP BY 1 ORDER BY 1`, from)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var d day
				if err = rows.Scan(&d.Date, &d.Plays); err != nil {
					break
				}
				days = append(days, d)
			}
			if err == nil {
				err = rows.Err()
			}
		}
	}
	if err != nil {
		log.Printf("stats: summary: %v", err)
		apiError(w, 500, "internal error")
		return
	}
	if days == nil {
		days = []day{}
	}
	writeJSON(w, 200, map[string]any{
		"range": rangeName(r), "plays": plays, "distinct_tracks": tracks,
		"distinct_artists": artists, "listened_ms": listenedMS, "per_day": days,
	})
}
