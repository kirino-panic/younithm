package sitesettings

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
)

// Gate returns middleware that answers 404 to anonymous visitors while
// section is hidden. Admins always pass. A settings read failure fails closed.
func (s *Store) Gate(section string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pub, err := s.Public(r.Context(), section)
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "unavailable"})
				return
			}
			if !pub && !auth.IsAdminSession(r) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found", "message": "This section is private."})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Mount registers:
//
//	GET /api/v1/site                        public  section visibility
//	GET /api/v1/admin/settings              session section list with public flags
//	PUT /api/v1/admin/settings/visibility   session {"sections":{"scrobbles":false}}
func Mount(r chi.Router, s *Store) {
	r.Get("/api/v1/site", func(w http.ResponseWriter, req *http.Request) {
		vis, err := s.Visibility(req.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, map[string]any{"sections": vis})
	})

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession)
		r.Get("/api/v1/admin/settings", func(w http.ResponseWriter, req *http.Request) {
			vis, err := s.Visibility(req.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
				return
			}
			secs := make([]map[string]any, 0, len(Sections))
			for _, sec := range Sections {
				secs = append(secs, map[string]any{"key": sec.Key, "label": sec.Label, "public": vis[sec.Key]})
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, map[string]any{"sections": secs, "debug": s.debug})
		})
		r.Put("/api/v1/admin/settings/visibility", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Sections map[string]bool `json:"sections"`
			}
			if !decode(w, req, &in) {
				return
			}
			for k := range in.Sections {
				if !Known(k) {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_input", "message": "unknown section: " + k})
					return
				}
			}
			for k, v := range in.Sections {
				if err := s.SetPublic(req.Context(), k, v); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
					return
				}
			}
			vis, err := s.Visibility(req.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"sections": vis})
		})
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
