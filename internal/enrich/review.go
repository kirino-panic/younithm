package enrich

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"younithm/internal/providers"
	"younithm/internal/providers/snapshot"
)

// Review serves the admin review queue: recordings whose provider links are
// candidates awaiting a decision.
type Review struct {
	read  *sql.DB
	m     *Manager
	snaps *snapshot.Store
}

func NewReview(read *sql.DB, m *Manager, snaps *snapshot.Store) *Review {
	return &Review{read: read, m: m, snaps: snaps}
}

// Mount registers the routes; admin guards them (session + CSRF in production).
//
//	GET  /api/v1/admin/review?after_id&limit              recordings with candidates
//	POST /api/v1/admin/review/links/{id}/confirm          pin the link, drop sibling candidates
//	POST /api/v1/admin/review/links/{id}/reject           drop it and never propose it again
//	POST /api/v1/admin/review/recordings/{id}/rematch     queue the recording for matching now
//	POST /api/v1/admin/review/recordings/{id}/link        {"url": "..."}: link by hand, replacing that provider's candidates
//	POST /api/v1/admin/enrichment/requeue                 queue every recording again (after connecting a provider)
//	GET  /api/v1/admin/enrichment                         queue counts
//	GET  /api/v1/admin/matching                           {"high_precision": bool}
//	PUT  /api/v1/admin/matching                           {"high_precision": bool}: more requests, more certainty
func (h *Review) Mount(r chi.Router, admin func(http.Handler) http.Handler) {
	h.MountAt(r, "/api/v1/admin", admin)
}

// MountAt is Mount with the path prefix chosen by the caller.
func (h *Review) MountAt(r chi.Router, p string, admin func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(admin)
		r.Get(p+"/review", h.list)
		r.Post(p+"/review/links/{id}/confirm", h.confirm)
		r.Post(p+"/review/links/{id}/reject", h.reject)
		r.Post(p+"/review/recordings/{id}/rematch", h.rematch)
		r.Post(p+"/review/recordings/{id}/link", h.manualLink)
		r.Get(p+"/enrichment", h.status)
		r.Get(p+"/matching", h.getSettings)
		r.Put(p+"/matching", h.putSettings)
		r.Post(p+"/enrichment/requeue", h.requeueAll)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func internal(w http.ResponseWriter, what string, err error) {
	log.Printf("enrich: %s: %v", what, err)
	apiError(w, 500, "internal error")
}

type candidate struct {
	LinkID     int64    `json:"link_id"`
	Provider   string   `json:"provider"`
	ProviderID string   `json:"provider_id"`
	Method     string   `json:"method"`
	Confidence float64  `json:"confidence"`
	Title      string   `json:"title,omitempty"`
	Artists    []string `json:"artists,omitempty"`
	Releases   []string `json:"releases,omitempty"`
	DurationMs int      `json:"duration_ms,omitempty"`
	// MusicBrainz's comment telling same-named recordings apart.
	Disambiguation string `json:"disambiguation,omitempty"`
	// What tells near-identical guesses apart: the releases with their dates
	// and types, how many there are in all, and the ISRCs.
	ReleaseDetails []releaseDetail `json:"release_details,omitempty"`
	ReleaseCount   int             `json:"release_count"`
	ISRCs          []string        `json:"isrcs,omitempty"`
	Known          bool            `json:"known"` // a snapshot was found
}

type releaseDetail struct {
	Title  string `json:"title"`
	Date   string `json:"date,omitempty"`
	Type   string `json:"type,omitempty"`
	Status string `json:"status,omitempty"`
}

type reviewItem struct {
	RecordingID int64       `json:"recording_id"`
	Title       string      `json:"title"`
	Artist      string      `json:"artist"`
	Album       *string     `json:"album"`
	DurationMs  *int64      `json:"duration_ms"`
	Candidates  []candidate `json:"candidates"`
}

func (h *Review) list(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			apiError(w, 400, "limit must be 1..100")
			return
		}
		limit = n
	}
	var after int64
	if v := r.URL.Query().Get("after_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			apiError(w, 400, "invalid after_id")
			return
		}
		after = n
	}
	ctx := r.Context()
	rows, err := h.read.QueryContext(ctx, `
SELECT rec.id, rec.title, rec.duration_ms,
  COALESCE((SELECT group_concat(name, ', ') FROM (
     SELECT a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id
     WHERE ra.recording_id=rec.id ORDER BY ra.position, a.id)), ''),
  (SELECT rel.title FROM recording_release rr JOIN release rel ON rel.id=rr.release_id
     WHERE rr.recording_id=rec.id ORDER BY rr.is_primary DESC, rel.id LIMIT 1)
FROM recording rec
WHERE rec.id > ? AND EXISTS (SELECT 1 FROM external_link l
  WHERE l.entity_type='recording' AND l.entity_id=rec.id AND l.status='candidate')
ORDER BY rec.id LIMIT ?`, after, limit+1)
	if err != nil {
		internal(w, "review list", err)
		return
	}
	var items []reviewItem
	for rows.Next() {
		var it reviewItem
		if err := rows.Scan(&it.RecordingID, &it.Title, &it.DurationMs, &it.Artist, &it.Album); err != nil {
			rows.Close()
			internal(w, "review scan", err)
			return
		}
		items = append(items, it)
	}
	rows.Close()
	var next *int64
	if len(items) > limit {
		items = items[:limit]
		n := items[limit-1].RecordingID
		next = &n
	}
	for i := range items {
		cs, err := h.candidates(r, items[i].RecordingID)
		if err != nil {
			internal(w, "candidates", err)
			return
		}
		items[i].Candidates = cs
	}
	if items == nil {
		items = []reviewItem{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_after_id": next})
}

func (h *Review) candidates(r *http.Request, recID int64) ([]candidate, error) {
	rows, err := h.read.QueryContext(r.Context(), `
SELECT id, provider, provider_id, match_method, COALESCE(confidence, 0) FROM external_link
WHERE entity_type='recording' AND entity_id=? AND status='candidate'
ORDER BY confidence DESC, id`, recID)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.LinkID, &c.Provider, &c.ProviderID, &c.Method, &c.Confidence); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	for i := range out {
		c := &out[i]
		e, err := h.snaps.Get(r.Context(), c.Provider, c.ProviderID, snapshot.KindRecording)
		if err != nil || e == nil || e.NotFound {
			continue
		}
		var t providers.Track
		if json.Unmarshal(e.Raw, &t) != nil {
			continue
		}
		c.Title, c.DurationMs, c.Known, c.Disambiguation = t.Title, t.DurationMs, true, t.Disambiguation
		c.ReleaseCount, c.ISRCs = len(t.Releases), t.ISRCs
		for _, a := range t.Artists {
			c.Artists = append(c.Artists, a.Name)
		}
		for j, rel := range t.Releases {
			if j < 3 {
				c.Releases = append(c.Releases, rel.Title)
			}
			if j < 5 {
				c.ReleaseDetails = append(c.ReleaseDetails, releaseDetail{rel.Title, rel.Date, rel.Type, rel.Status})
			}
		}
	}
	return out, nil
}

type linkRow struct {
	recID                int64
	provider, providerID string
	status               string
}

func (h *Review) link(r *http.Request) (linkRow, int) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		return linkRow{}, 400
	}
	var l linkRow
	err = h.m.db.QueryRowContext(r.Context(), `SELECT entity_id, provider, provider_id, status FROM external_link
WHERE id=? AND entity_type='recording'`, id).Scan(&l.recID, &l.provider, &l.providerID, &l.status)
	if errors.Is(err, sql.ErrNoRows) {
		return l, 404
	}
	if err != nil {
		log.Printf("enrich: link lookup: %v", err)
		return l, 500
	}
	if l.status != "candidate" {
		return l, 409
	}
	return l, 0
}

var linkErrors = map[int]string{400: "invalid id", 404: "no such link", 409: "link is not a candidate", 500: "internal error"}

func (h *Review) confirm(w http.ResponseWriter, r *http.Request) {
	l, code := h.link(r)
	if code != 0 {
		apiError(w, code, linkErrors[code])
		return
	}
	id := chi.URLParam(r, "id")
	tx, err := h.m.db.BeginTx(r.Context(), nil)
	if err != nil {
		internal(w, "confirm", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE external_link SET status='confirmed', pinned=1 WHERE id=?`, id); err != nil {
		internal(w, "confirm", err)
		return
	}
	res, err := tx.ExecContext(r.Context(), `DELETE FROM external_link WHERE entity_type='recording'
AND entity_id=? AND provider=? AND status='candidate'`, l.recID, l.provider)
	if err != nil {
		internal(w, "confirm", err)
		return
	}
	if err := tx.Commit(); err != nil {
		internal(w, "confirm", err)
		return
	}
	dropped, _ := res.RowsAffected()
	writeJSON(w, 200, map[string]any{"recording_id": l.recID, "status": "confirmed", "dropped_candidates": dropped})
}

func (h *Review) reject(w http.ResponseWriter, r *http.Request) {
	l, code := h.link(r)
	if code != 0 {
		apiError(w, code, linkErrors[code])
		return
	}
	tx, err := h.m.db.BeginTx(r.Context(), nil)
	if err != nil {
		internal(w, "reject", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `INSERT OR IGNORE INTO link_rejection
(entity_type, entity_id, provider, provider_id) VALUES ('recording', ?, ?, ?)`,
		l.recID, l.provider, l.providerID); err != nil {
		internal(w, "reject", err)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM external_link WHERE id=?`, chi.URLParam(r, "id")); err != nil {
		internal(w, "reject", err)
		return
	}
	if err := tx.Commit(); err != nil {
		internal(w, "reject", err)
		return
	}
	writeJSON(w, 200, map[string]any{"recording_id": l.recID, "status": "rejected"})
}

func (h *Review) rematch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		apiError(w, 400, "invalid id")
		return
	}
	var one int
	if err := h.read.QueryRowContext(r.Context(), `SELECT 1 FROM recording WHERE id=?`, id).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 404, "no such recording")
			return
		}
		internal(w, "rematch", err)
		return
	}
	if err := h.m.Requeue(r.Context(), id); err != nil {
		internal(w, "rematch", err)
		return
	}
	h.m.Wake()
	writeJSON(w, 202, map[string]any{"recording_id": id, "status": "queued"})
}

func (h *Review) status(w http.ResponseWriter, r *http.Request) {
	c, err := h.m.Counts(r.Context())
	if err != nil {
		internal(w, "status", err)
		return
	}
	writeJSON(w, 200, c)
}

func (h *Review) requeueAll(w http.ResponseWriter, r *http.Request) {
	n, err := h.m.RequeueAll(r.Context())
	if err != nil {
		internal(w, "requeue", err)
		return
	}
	writeJSON(w, 202, map[string]any{"queued": n})
}

func (h *Review) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"high_precision": h.m.HighPrecision(r.Context())})
}

func (h *Review) putSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		HighPrecision *bool `json:"high_precision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil || in.HighPrecision == nil {
		apiError(w, 400, "expected {\"high_precision\": true|false}")
		return
	}
	if err := h.m.SetHighPrecision(r.Context(), *in.HighPrecision); err != nil {
		internal(w, "settings", err)
		return
	}
	writeJSON(w, 200, map[string]bool{"high_precision": *in.HighPrecision})
}

func (h *Review) manualLink(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		apiError(w, 400, "invalid id")
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		apiError(w, 400, `expected {"url": "..."}`)
		return
	}
	provider, pid, ok := ParseTrackURL(in.URL)
	if !ok {
		apiError(w, 400, "not a MusicBrainz recording, Spotify track or Apple Music song link")
		return
	}
	ctx := r.Context()
	tx, err := h.m.db.BeginTx(ctx, nil)
	if err != nil {
		internal(w, "manual link", err)
		return
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM recording WHERE id=?`, id).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 404, "no such recording")
			return
		}
		internal(w, "manual link", err)
		return
	}
	// The person's link replaces whatever this provider had for the recording.
	if _, err := tx.ExecContext(ctx, `DELETE FROM external_link WHERE entity_type='recording' AND entity_id=?
AND provider=? AND provider_id<>?`, id, provider, pid); err != nil {
		internal(w, "manual link", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_rejection WHERE entity_type='recording' AND entity_id=?
AND provider=? AND provider_id=?`, id, provider, pid); err != nil {
		internal(w, "manual link", err)
		return
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status, pinned)
VALUES ('recording', ?, ?, ?, 'manual', 1, 'confirmed', 1)
ON CONFLICT(entity_type, entity_id, provider, provider_id) DO UPDATE SET
  match_method='manual', confidence=1, status='confirmed', pinned=1`, id, provider, pid); err != nil {
		internal(w, "manual link", err)
		return
	}
	if err := tx.Commit(); err != nil {
		internal(w, "manual link", err)
		return
	}
	// Look the linked track up so its details fill in.
	if err := h.m.Requeue(ctx, id); err != nil {
		log.Printf("enrich: requeue after manual link: %v", err)
	}
	h.m.Wake()
	writeJSON(w, 200, map[string]any{"recording_id": id, "provider": provider, "provider_id": pid, "status": "confirmed"})
}
