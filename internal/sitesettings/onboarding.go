package sitesettings

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
)

// Onboarding is finished once the admin presses the last button of the welcome
// flow, which is also the only point at which anything from it is saved. Until
// then, signing in returns to the flow, so an interrupted onboarding can always
// be picked up again. Installs that had an admin before the flow existed are
// marked finished by migration 00230.
const onboardingKey = "onboarding.done"

// OnboardingDone reports whether the welcome flow was finished.
func (s *Store) OnboardingDone(ctx context.Context) (bool, error) {
	v, _, err := s.get(ctx, onboardingKey)
	return v == "1", err
}

// MountOnboarding registers:
//
//	GET  /api/v1/admin/onboarding       session  {"done": bool}
//	POST /api/v1/admin/onboarding/done  session  marks it finished
func MountOnboarding(r chi.Router, s *Store) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession)
		r.Get("/api/v1/admin/onboarding", func(w http.ResponseWriter, req *http.Request) {
			done, err := s.OnboardingDone(req.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"done": done})
		})
		r.Post("/api/v1/admin/onboarding/done", func(w http.ResponseWriter, req *http.Request) {
			if err := s.put(req.Context(), onboardingKey, "1"); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"done": true})
		})
	})
}
