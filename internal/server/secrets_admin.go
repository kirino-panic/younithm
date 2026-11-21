package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
	"younithm/internal/secrets"
)

// providerSecrets is the allow-list of names the admin API will accept, in
// display order. Providers (spotify, apple) read them with Store.Get.
var providerSecrets = []string{
	"musicbrainz.contact", // not secret, but stored and edited the same way
	"spotify.client_id",
	"spotify.client_secret",
	"apple.team_id",
	"apple.key_id",
	"apple.private_key",
}

const maxSecretLen = 8 << 10 // a .p8 key is ~250 bytes; stay generous but bounded

type secretMetaJSON struct {
	Name      string     `json:"name"`
	Set       bool       `json:"set"`
	Hint      string     `json:"hint,omitempty"` // last 4 chars, "...a3f2"
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

func toJSON(m secrets.Meta) secretMetaJSON {
	out := secretMetaJSON{Name: m.Name, Set: m.Set}
	if m.Set {
		out.Hint = m.Hint
		if !m.UpdatedAt.IsZero() {
			t := m.UpdatedAt
			out.UpdatedAt = &t
		}
	}
	return out
}

func knownSecret(name string) bool {
	for _, n := range providerSecrets {
		if n == name {
			return true
		}
	}
	return false
}

// mountSecretsAdmin registers the write-only provider-secret endpoints:
//
//	GET    /api/v1/admin/secrets         metadata for every known secret (session)
//	PUT    /api/v1/admin/secrets/{name}  {"value": "..."} (session + step-up; onChange runs after)
//	DELETE /api/v1/admin/secrets/{name}  (session + step-up)
//
// No endpoint ever returns a value; only {set, hint, updated_at}.
func mountSecretsAdmin(r chi.Router, st *secrets.Store, onChange func()) {
	const p = "/api/v1/admin/secrets"
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession)
		r.Get(p, func(w http.ResponseWriter, req *http.Request) {
			out := make([]secretMetaJSON, 0, len(providerSecrets))
			for _, n := range providerSecrets {
				m, err := st.Meta(req.Context(), n)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
					return
				}
				out = append(out, toJSON(m))
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
		})
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireStepUp)
			r.Put(p+"/{name}", func(w http.ResponseWriter, req *http.Request) {
				name := chi.URLParam(req, "name")
				if !knownSecret(name) {
					writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown_secret"})
					return
				}
				req.Body = http.MaxBytesReader(w, req.Body, maxSecretLen+1024)
				var in struct {
					Value string `json:"value"`
				}
				if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
					return
				}
				v := strings.TrimSpace(in.Value)
				if v == "" || len(v) > maxSecretLen {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_value"})
					return
				}
				if err := st.Set(req.Context(), name, v); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
					return
				}
				m, err := st.Meta(req.Context(), name)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
					return
				}
				if onChange != nil {
					onChange()
				}
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, http.StatusOK, toJSON(m))
			})
			r.Delete(p+"/{name}", func(w http.ResponseWriter, req *http.Request) {
				name := chi.URLParam(req, "name")
				if !knownSecret(name) {
					writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown_secret"})
					return
				}
				if err := st.Delete(req.Context(), name); err != nil && !errors.Is(err, secrets.ErrNotSet) {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
					return
				}
				if onChange != nil {
					onChange()
				}
				w.WriteHeader(http.StatusNoContent)
			})
		})
	})
}
