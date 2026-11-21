package sitesettings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
	"younithm/internal/config"
)

// The owner's public profile: a display name and a small square avatar.
// Both live in admin_setting; the avatar is a PNG kept as base64.
const (
	profileNameKey   = "profile.name"
	profileAvatarKey = "profile.avatar"

	maxNameRunes   = 40
	maxAvatarBytes = 300 << 10
	maxAvatarSide  = 512
)

// Profile is what visitors see.
type Profile struct {
	DisplayName string `json:"display_name"`
	Avatar      string `json:"avatar"` // URL, empty when none
}

// Profile returns the stored profile.
func (s *Store) Profile(ctx context.Context) (Profile, error) {
	name, _, err := s.get(ctx, profileNameKey)
	if err != nil {
		return Profile{}, err
	}
	p := Profile{DisplayName: name}
	b64, ok, err := s.get(ctx, profileAvatarKey)
	if err != nil {
		return Profile{}, err
	}
	if ok && b64 != "" {
		sum := sha256.Sum256([]byte(b64))
		p.Avatar = config.BasePath() + "/api/v1/site/profile/avatar?v=" + hex.EncodeToString(sum[:4])
	}
	return p, nil
}

// decodeAvatar accepts a base64 PNG (optionally a data: URL) that is square
// and no larger than maxAvatarSide, and returns the raw base64.
func decodeAvatar(in string) (string, bool) {
	in = strings.TrimPrefix(in, "data:image/png;base64,")
	raw, err := base64.StdEncoding.DecodeString(in)
	if err != nil || len(raw) == 0 || len(raw) > maxAvatarBytes {
		return "", false
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", false
	}
	b := img.Bounds()
	if b.Dx() != b.Dy() || b.Dx() < 1 || b.Dx() > maxAvatarSide {
		return "", false
	}
	return in, true
}

// MountProfile registers:
//
//	GET /api/v1/site/profile          public   {"display_name": "", "avatar": ""}
//	GET /api/v1/site/profile/avatar   public   the avatar PNG, 404 when unset
//	PUT /api/v1/admin/profile         session  {"display_name"?: string, "avatar"?: base64 PNG | ""}
func MountProfile(r chi.Router, s *Store) {
	r.Get("/api/v1/site/profile", func(w http.ResponseWriter, req *http.Request) {
		p, err := s.Profile(req.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, p)
	})
	r.Get("/api/v1/site/profile/avatar", func(w http.ResponseWriter, req *http.Request) {
		b64, ok, err := s.get(req.Context(), profileAvatarKey)
		if err != nil || !ok || b64 == "" {
			http.NotFound(w, req)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // the URL carries a version
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(raw)
	})
	r.With(auth.RequireSession).Put("/api/v1/admin/profile", func(w http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(w, req.Body, 2*maxAvatarBytes)
		var in struct {
			DisplayName *string `json:"display_name"`
			Avatar      *string `json:"avatar"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_request"})
			return
		}
		bad := func(msg string) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_input", "message": msg})
		}
		var name, avatar string
		if in.DisplayName != nil {
			name = strings.TrimSpace(*in.DisplayName)
			if utf8.RuneCountInString(name) > maxNameRunes {
				bad("Display name is too long.")
				return
			}
		}
		if in.Avatar != nil && *in.Avatar != "" {
			var ok bool
			if avatar, ok = decodeAvatar(*in.Avatar); !ok {
				bad("Avatar must be a square PNG, up to 512 px.")
				return
			}
		}
		ctx := req.Context()
		if in.DisplayName != nil {
			if err := s.put(ctx, profileNameKey, name); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
				return
			}
		}
		if in.Avatar != nil {
			if err := s.put(ctx, profileAvatarKey, avatar); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
				return
			}
		}
		p, err := s.Profile(ctx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
			return
		}
		writeJSON(w, http.StatusOK, p)
	})
}
