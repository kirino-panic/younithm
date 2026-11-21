package sitesettings

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
)

// The home page document: theme, tile layout and custom CSS, owned by the
// frontend. The server stores it as an opaque JSON object.
const pageKey = "home.page"

const maxPageBytes = 256 << 10

// Page returns the stored home page document, or nil when none is saved.
func (s *Store) Page(ctx context.Context) (json.RawMessage, error) {
	v, ok, err := s.get(ctx, pageKey)
	if err != nil || !ok {
		return nil, err
	}
	return json.RawMessage(v), nil
}

// SetPage stores the home page document.
func (s *Store) SetPage(ctx context.Context, doc json.RawMessage) error {
	return s.put(ctx, pageKey, string(doc))
}

// MountPage registers:
//
//	GET /api/v1/site/page          public   {"page": {...} | null}
//	PUT /api/v1/admin/site/page    session  {"page": {...}}
func MountPage(r chi.Router, s *Store) {
	r.Get("/api/v1/site/page", func(w http.ResponseWriter, req *http.Request) {
		doc, err := s.Page(req.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, map[string]any{"page": doc})
	})
	r.With(auth.RequireSession).Put("/api/v1/admin/site/page", func(w http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(w, req.Body, maxPageBytes)
		var in struct {
			Page json.RawMessage `json:"page"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
			return
		}
		trimmed := bytes.TrimSpace(in.Page)
		if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_input", "message": "page must be a JSON object"})
			return
		}
		if err := s.SetPage(req.Context(), trimmed); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"page": json.RawMessage(trimmed)})
	})
}
