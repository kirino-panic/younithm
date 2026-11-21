package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
	"younithm/internal/providers"
	"younithm/internal/providers/apple"
	"younithm/internal/providers/musicbrainz"
	"younithm/internal/providers/spotify"
)

// mountConnectionCheck registers
//
//	POST /api/v1/admin/connections/{provider}/check  (session)
//
// which asks the provider one real question, so a bad key or contact shows up
// when it is entered rather than deep in a matching run. With a body of
// {"values": {"spotify.client_id": "...", ...}} it tries those details without
// storing anything (onboarding saves only at the end); with no body it uses the
// saved settings. It answers {"ok": true} or {"ok": false, "error_kind", "detail"}.
func mountConnectionCheck(r chi.Router, reg *providers.Registry) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession)
		r.Post("/api/v1/admin/connections/{provider}/check", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "provider")
			var in struct {
				Values map[string]string `json:"values"`
			}
			req.Body = http.MaxBytesReader(w, req.Body, maxSecretLen*4)
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
				return
			}
			var p providers.Provider
			if in.Values != nil {
				var err error
				if p, err = tryProvider(name, in.Values); err != nil {
					writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error_kind": "missing", "detail": err.Error()})
					return
				}
			} else {
				if saved, ok := reg.Get(name); ok {
					p = saved
				}
			}
			if p == nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown_provider"})
				return
			}
			ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
			defer cancel()
			_, err := p.SearchTrack(ctx, providers.TrackQuery{Artist: "Daft Punk", Title: "Get Lucky", Limit: 1})
			w.Header().Set("Cache-Control", "no-store")
			if err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": true})
				return
			}
			kind := "rejected"
			var rl *providers.RateLimitedError
			switch {
			case errors.As(err, &rl):
				kind = "rate_limited"
			case errors.Is(err, context.DeadlineExceeded):
				kind = "timeout"
			case errors.Is(err, providers.ErrNotConfigured) && in.Values == nil:
				kind = "not_configured"
			}
			// With details given, "not configured" can only mean the service
			// refused them: providers report a refused key that way.
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error_kind": kind, "detail": err.Error()})
		})
	})
}

// tryProvider builds a throwaway client for name from the given details.
func tryProvider(name string, v map[string]string) (providers.Provider, error) {
	get := func(k string) (string, error) {
		s := strings.TrimSpace(v[k])
		if s == "" || len(s) > maxSecretLen {
			return "", errors.New(k + " is missing")
		}
		return s, nil
	}
	switch name {
	case providers.MusicBrainz:
		c, err := get("musicbrainz.contact")
		if err != nil {
			return nil, err
		}
		return musicbrainz.New(musicbrainz.Config{Contact: c})
	case providers.Spotify:
		id, err := get("spotify.client_id")
		if err != nil {
			return nil, err
		}
		sec, err := get("spotify.client_secret")
		if err != nil {
			return nil, err
		}
		return spotify.New(spotify.Config{Credentials: func(context.Context) (string, string, error) { return id, sec, nil }})
	case providers.AppleMusic:
		team, err := get("apple.team_id")
		if err != nil {
			return nil, err
		}
		kid, err := get("apple.key_id")
		if err != nil {
			return nil, err
		}
		pk, err := get("apple.private_key")
		if err != nil {
			return nil, err
		}
		return apple.New(apple.Config{Credentials: func(context.Context) (string, string, string, error) { return team, kid, pk, nil }})
	}
	return nil, nil
}
