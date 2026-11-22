package sitesettings

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
	"younithm/internal/debugadmin"
)

const (
	debugFakeNowPlayingKey = "debug.fake_now_playing"
	debugImportLimitKey    = "debug.import_limit"

	maxImportLimit = 1_000_000
	clearConfirm   = "DELETE"
)

// FakeNowPlaying reports whether the now-playing widget shows a made-up track.
func (s *Store) FakeNowPlaying(ctx context.Context) bool {
	if !s.debug {
		return false
	}
	v, ok, err := s.get(ctx, debugFakeNowPlayingKey)
	return err == nil && ok && v == "1"
}

// ImportLimit is the debug cap on listens per import (newest first); 0 is off.
func (s *Store) ImportLimit(ctx context.Context) int {
	if !s.debug {
		return 0
	}
	v, ok, err := s.get(ctx, debugImportLimitKey)
	if err != nil || !ok {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

// DebugHooks connect the debug endpoints to the rest of the server.
type DebugHooks struct {
	Counts           func(context.Context) (debugadmin.Counts, error)
	Clear            func(context.Context) error
	OnFakeNowPlaying func() // called after the fake now-playing switch changes
}

// MountDebug registers, all behind a session:
//
//	GET  /api/v1/admin/debug                {"fake_now_playing":false,"import_limit":0,"counts":{...}}
//	PUT  /api/v1/admin/debug                {"fake_now_playing"?: bool, "import_limit"?: int}
//	POST /api/v1/admin/debug/clear-library  {"confirm":"DELETE"}
func MountDebug(r chi.Router, s *Store, h DebugHooks) {
	if !s.debug {
		return
	}
	state := func(w http.ResponseWriter, req *http.Request) {
		c, err := h.Counts(req.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"fake_now_playing": s.FakeNowPlaying(req.Context()),
			"import_limit":     s.ImportLimit(req.Context()),
			"counts":           c,
		})
	}
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession)
		r.Get("/api/v1/admin/debug", state)
		r.Put("/api/v1/admin/debug", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				FakeNowPlaying *bool `json:"fake_now_playing"`
				ImportLimit    *int  `json:"import_limit"`
			}
			if !decode(w, req, &in) {
				return
			}
			if in.ImportLimit != nil && (*in.ImportLimit < 0 || *in.ImportLimit > maxImportLimit) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_input", "message": "Import limit must be 0 (off) or a positive number."})
				return
			}
			fail := func() { writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"}) }
			if in.FakeNowPlaying != nil {
				v := "0"
				if *in.FakeNowPlaying {
					v = "1"
				}
				if err := s.put(req.Context(), debugFakeNowPlayingKey, v); err != nil {
					fail()
					return
				}
				if h.OnFakeNowPlaying != nil {
					h.OnFakeNowPlaying()
				}
			}
			if in.ImportLimit != nil {
				if err := s.put(req.Context(), debugImportLimitKey, strconv.Itoa(*in.ImportLimit)); err != nil {
					fail()
					return
				}
			}
			state(w, req)
		})
		r.Post("/api/v1/admin/debug/clear-library", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Confirm string `json:"confirm"`
			}
			if !decode(w, req, &in) {
				return
			}
			if in.Confirm != clearConfirm {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "confirm_required", "message": "Type DELETE to confirm."})
				return
			}
			switch err := h.Clear(req.Context()); err {
			case nil:
				state(w, req)
			case debugadmin.ErrBusy:
				writeJSON(w, http.StatusConflict, map[string]any{"error": "busy", "message": "An import is running. Cancel it first."})
			default:
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			}
		})
	})
}
