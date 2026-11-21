package ingest

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"younithm/internal/names"

	"github.com/go-chi/chi/v5"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Options configures the ingest handler.
type Options struct {
	// Auth validates submit tokens. Required in production; nil rejects all
	// submissions.
	Auth Authenticator
	// RequireAdmin wraps the scrobble detail endpoint (raw payloads are
	// admin-only). nil = open (tests only).
	RequireAdmin func(http.Handler) http.Handler
	// ListGate wraps the public scrobble list. This is the hook for the
	// per-section visibility toggle: a gate can 404/401 when the list is not
	// public. nil = always public (the default).
	ListGate func(http.Handler) http.Handler
	// NowPlayingGate wraps the now-playing JSON and SSE endpoints; same idea
	// as ListGate. nil = always public.
	NowPlayingGate  func(http.Handler) http.Handler
	DedupeTolerance time.Duration // default 2s
	// Identify is passed to Store.Identify (identity resolution).
	Identify func(ctx context.Context, tx *sql.Tx, l NewScrobble) (recordingID, releaseID int64, err error)
}

// Handler holds ingest state.
type Handler struct {
	read, write *sql.DB
	store       *Store
	np          *NowPlaying
	auth        Authenticator
	admin       func(http.Handler) http.Handler
	listGate    func(http.Handler) http.Handler
	npGate      func(http.Handler) http.Handler
}

// New builds the handler. read and write may be the same *sql.DB.
func New(read, write *sql.DB, o Options) *Handler {
	if o.Auth == nil {
		o.Auth = rejectAll{}
	}
	if o.DedupeTolerance == 0 {
		o.DedupeTolerance = 2 * time.Second
	}
	if o.ListGate == nil {
		o.ListGate = func(h http.Handler) http.Handler { return h }
	}
	if o.NowPlayingGate == nil {
		o.NowPlayingGate = func(h http.Handler) http.Handler { return h }
	}
	if o.RequireAdmin == nil {
		o.RequireAdmin = func(h http.Handler) http.Handler { return h }
	}
	return &Handler{read: read, write: write, np: NewNowPlaying(), auth: o.Auth, admin: o.RequireAdmin, listGate: o.ListGate, npGate: o.NowPlayingGate,
		store: &Store{DB: write, DedupeTolerance: o.DedupeTolerance, Identify: o.Identify}}
}

type rejectAll struct{}

func (rejectAll) AuthenticateToken(context.Context, string) (int64, string, error) {
	return 0, "", ErrInvalidToken
}

// Store exposes the write path (for importers).
func (h *Handler) Store() *Store { return h.store }

// MountRoot registers the ListenBrainz endpoints on the root router.
func (h *Handler) MountRoot(r chi.Router) {
	for _, p := range []string{"/1", "/apis/listenbrainz/1"} {
		r.Post(p+"/submit-listens", h.submitListens)
		r.Get(p+"/validate-token", h.validateToken)
		r.Get(p+"/user/{user}/listens", h.userListens)
	}
}

// SetFakeNowPlaying shows a made-up track when nothing is really playing and
// enabled() is true (debug only). pick supplies the track, for example a random
// one from the library; without one, or when it finds none, a fixed track shows.
func (h *Handler) SetFakeNowPlaying(enabled func() bool, pick func() (NowPlayingTrack, bool)) {
	h.np.mu.Lock()
	h.np.enabled, h.np.pick = enabled, pick
	h.np.mu.Unlock()
}

// PokeNowPlaying tells now-playing streams to look again.
func (h *Handler) PokeNowPlaying() { h.np.notify() }

// MountAPI registers read endpoints inside the /api/v1 route group.
func (h *Handler) MountAPI(r chi.Router) {
	r.With(h.npGate).Get("/now-playing", h.getNowPlaying)
	r.With(h.npGate).Get("/now-playing/stream", h.streamNowPlaying)
	r.With(h.listGate).Get("/scrobbles", h.listScrobbles)
	r.With(h.admin).Get("/scrobbles/{id}", h.getScrobble)
}

func (h *Handler) getNowPlaying(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, h.np.Get(1))
}

func (h *Handler) streamNowPlaying(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		apiError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, cancel := h.np.Subscribe()
	defer cancel()
	var last string
	send := func() {
		v := h.np.Get(1)
		b, _ := json.Marshal(v)
		// progress_ms changes every call; compare on the identity of the state.
		key := fmt.Sprint(v.Playing, v.StartedAt, v.ExpiresAt)
		if v.Track != nil {
			key += v.Track.Artist + v.Track.Title
		}
		if key == last {
			return
		}
		last = key
		fmt.Fprintf(w, "event: now-playing\ndata: %s\n\n", b)
		fl.Flush()
	}
	send()
	tick := time.NewTicker(15 * time.Second)
	exp := time.NewTicker(time.Second)
	defer tick.Stop()
	defer exp.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			send()
		case <-exp.C:
			send()
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

type scrobbleItem struct {
	ID       int64  `json:"id"`
	PlayedAt string `json:"played_at"`
	Artist   string `json:"artist"`
	// Artists are the track's credited artists in order, each linkable; empty
	// when the scrobble has no track yet (Artist then holds what was submitted).
	Artists     json.RawMessage `json:"artists"`
	Title       string          `json:"title"`
	Album       *string         `json:"album"`
	Source      *string         `json:"source"`
	RecordingID *int64          `json:"recording_id"`
	ReleaseID   *int64          `json:"release_id"`
	Matched     bool            `json:"matched"`
}

var displaySelect = `
SELECT s.id, s.played_at,
  COALESCE(` + names.LineSQL(names.Recording, "s.recording_id") + `, s.raw_artist),
  (SELECT json_group_array(json_object('id', id, 'name', name, 'join_phrase', jp, 'credited_name', cn)) FROM (
     SELECT a.id, a.name, COALESCE(ra.join_phrase, '') AS jp, COALESCE(ra.credited_name, '') AS cn FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
     WHERE ra.recording_id=s.recording_id ORDER BY ra.position, a.id)),
  COALESCE(rec.title, s.raw_title),
  COALESCE(rel.title, s.raw_album),
  s.source, s.recording_id, s.release_id
FROM scrobble s
LEFT JOIN recording rec ON rec.id = s.recording_id
LEFT JOIN release rel ON rel.id = s.release_id`

func (h *Handler) listScrobbles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			apiError(w, 400, "limit must be 1..200")
			return
		}
		limit = n
	}
	var where []string
	var args []any
	where = append(where, "s.user_id = 1")
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			apiError(w, 400, "from must be RFC 3339")
			return
		}
		where = append(where, "s.played_at >= ?")
		args = append(args, t.UTC().Format(TimeFormat))
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			apiError(w, 400, "to must be RFC 3339")
			return
		}
		where = append(where, "s.played_at < ?")
		args = append(args, t.UTC().Format(TimeFormat))
	}
	if v := q.Get("source"); v != "" {
		where = append(where, "s.source = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v) + "%"
		where = append(where, `(s.raw_artist LIKE ? ESCAPE '\' OR s.raw_title LIKE ? ESCAPE '\' OR s.raw_album LIKE ? ESCAPE '\'
		 OR rec.title LIKE ? ESCAPE '\' OR rel.title LIKE ? ESCAPE '\'
		 OR EXISTS (SELECT 1 FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
		            WHERE ra.recording_id=s.recording_id AND a.name LIKE ? ESCAPE '\'))`)
		for i := 0; i < 6; i++ {
			args = append(args, like)
		}
	}
	base := strings.Join(where, " AND ")

	var total int64
	if err := h.read.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM scrobble s LEFT JOIN recording rec ON rec.id=s.recording_id
		 LEFT JOIN release rel ON rel.id=s.release_id WHERE `+base, args...).Scan(&total); err != nil {
		log.Printf("ingest: count: %v", err)
		apiError(w, 500, "internal error")
		return
	}

	qargs := append([]any{}, args...)
	qwhere := base
	if c := q.Get("cursor"); c != "" {
		at, id, ok := decodeCursor(c)
		if !ok {
			apiError(w, 400, "invalid cursor")
			return
		}
		qwhere += " AND (s.played_at < ? OR (s.played_at = ? AND s.id < ?))"
		qargs = append(qargs, at, at, id)
	}
	qargs = append(qargs, limit+1)
	rows, err := h.read.QueryContext(r.Context(),
		displaySelect+" WHERE "+qwhere+" ORDER BY s.played_at DESC, s.id DESC LIMIT ?", qargs...)
	if err != nil {
		log.Printf("ingest: list: %v", err)
		apiError(w, 500, "internal error")
		return
	}
	defer rows.Close()
	items := []scrobbleItem{}
	for rows.Next() {
		var it scrobbleItem
		var artists string
		if err := rows.Scan(&it.ID, &it.PlayedAt, &it.Artist, &artists, &it.Title, &it.Album, &it.Source, &it.RecordingID, &it.ReleaseID); err != nil {
			apiError(w, 500, "internal error")
			return
		}
		it.Artists = json.RawMessage(artists)
		it.Matched = it.RecordingID != nil
		items = append(items, it)
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		c := base64.RawURLEncoding.EncodeToString([]byte(last.PlayedAt + "|" + strconv.FormatInt(last.ID, 10)))
		next = &c
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next, "total": total})
}

func decodeCursor(c string) (string, int64, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", 0, false
	}
	at, ids, ok := strings.Cut(string(b), "|")
	id, err := strconv.ParseInt(ids, 10, 64)
	if !ok || err != nil {
		return "", 0, false
	}
	return at, id, true
}

func (h *Handler) getScrobble(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		apiError(w, 404, "not found")
		return
	}
	var (
		playedAt, createdAt, rawArtist, rawTitle string
		dur                                      sql.NullInt64
		source, rawAlbum, rawPayload             sql.NullString
		recID, relID                             sql.NullInt64
	)
	err = h.read.QueryRowContext(r.Context(),
		`SELECT played_at, created_at, duration_played_ms, source, raw_artist, raw_title, raw_album, raw_payload, recording_id, release_id
		 FROM scrobble WHERE id=? AND user_id=1`, id).
		Scan(&playedAt, &createdAt, &dur, &source, &rawArtist, &rawTitle, &rawAlbum, &rawPayload, &recID, &relID)
	if err == sql.ErrNoRows {
		apiError(w, 404, "not found")
		return
	} else if err != nil {
		log.Printf("ingest: detail: %v", err)
		apiError(w, 500, "internal error")
		return
	}
	str := func(s sql.NullString) any {
		if s.Valid {
			return s.String
		}
		return nil
	}
	num := func(n sql.NullInt64) any {
		if n.Valid {
			return n.Int64
		}
		return nil
	}
	out := map[string]any{
		"id": id, "played_at": playedAt, "created_at": createdAt,
		"duration_played_ms": num(dur), "source": str(source), "matched": recID.Valid,
		"raw":         map[string]any{"artist": rawArtist, "title": rawTitle, "album": str(rawAlbum)},
		"raw_payload": nil, "recording": nil, "release": nil,
	}
	if rawPayload.Valid && json.Valid([]byte(rawPayload.String)) {
		out["raw_payload"] = json.RawMessage(rawPayload.String)
	}
	display := map[string]any{"artist": rawArtist, "title": rawTitle, "album": str(rawAlbum)}
	if recID.Valid {
		var title string
		var rdur sql.NullInt64
		if err := h.read.QueryRowContext(r.Context(), `SELECT title, duration_ms FROM recording WHERE id=?`, recID.Int64).Scan(&title, &rdur); err == nil {
			artists := []map[string]any{}
			rows, err := h.read.QueryContext(r.Context(),
				`SELECT a.id, a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
				 WHERE ra.recording_id=? ORDER BY ra.position, a.id`, recID.Int64)
			if err == nil {
				names := []string{}
				for rows.Next() {
					var aid int64
					var n string
					if rows.Scan(&aid, &n) == nil {
						artists = append(artists, map[string]any{"id": aid, "name": n})
						names = append(names, n)
					}
				}
				rows.Close()
				if len(names) > 0 {
					display["artist"] = strings.Join(names, ", ")
				}
			}
			display["title"] = title
			out["recording"] = map[string]any{"id": recID.Int64, "title": title, "duration_ms": num(rdur), "artists": artists}
		}
	}
	if relID.Valid {
		var title string
		var date, typ sql.NullString
		if err := h.read.QueryRowContext(r.Context(), `SELECT title, release_date, release_type FROM release WHERE id=?`, relID.Int64).Scan(&title, &date, &typ); err == nil {
			display["album"] = title
			out["release"] = map[string]any{"id": relID.Int64, "title": title, "release_date": str(date), "release_type": str(typ)}
		}
	}
	out["display"] = display
	writeJSON(w, 200, out)
}
