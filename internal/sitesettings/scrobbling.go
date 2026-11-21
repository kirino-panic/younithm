package sitesettings

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
)

// How scrobblers sign in to the ListenBrainz endpoints. The admin picks:
// no token (anything that knows the address can submit), or one token of
// their own choosing. Unset counts as required, so an install that predates
// the setting stays closed until a token is chosen.
const (
	scrobbleRequireKey = "scrobble.require_token"
	scrobbleTokenKey   = "scrobble.token_hash" // SHA-256 of the chosen token
	maxScrobbleToken   = 128
)

var errScrobbleToken = errors.New("token must be 1-128 characters with no spaces")

func hashScrobbleToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

// RequireScrobbleToken reports whether submissions need a token.
func (s *Store) RequireScrobbleToken(ctx context.Context) bool {
	v, ok, err := s.get(ctx, scrobbleRequireKey)
	return err != nil || !ok || v != "0"
}

func (s *Store) scrobbleTokenSet(ctx context.Context) bool {
	v, _, _ := s.get(ctx, scrobbleTokenKey)
	return v != ""
}

// SetScrobbleToken turns the token requirement on or off. token replaces the
// stored token when non-empty; turning it on with no token stored is refused.
func (s *Store) SetScrobbleToken(ctx context.Context, require bool, token string) error {
	if token != "" {
		if len(token) > maxScrobbleToken || strings.ContainsAny(token, " \t\r\n") {
			return errScrobbleToken
		}
		if err := s.put(ctx, scrobbleTokenKey, hashScrobbleToken(token)); err != nil {
			return err
		}
	}
	if require && token == "" && !s.scrobbleTokenSet(ctx) {
		return errScrobbleToken
	}
	v := "0"
	if require {
		v = "1"
	}
	return s.put(ctx, scrobbleRequireKey, v)
}

// admin is the one account: scrobbles belong to it, and scrobblers are told
// its username.
func (s *Store) admin(ctx context.Context) (int64, string, error) {
	var id int64
	var name string
	err := s.read.QueryRowContext(ctx, `SELECT id, username FROM admin_user ORDER BY id LIMIT 1`).Scan(&id, &name)
	return id, name, err
}

// ScrobbleAuth is the ingest Authenticator: open when no token is required,
// otherwise the chosen token.
type ScrobbleAuth struct {
	Site *Store
}

var errBadToken = errors.New("invalid token")

func (a ScrobbleAuth) AuthenticateToken(ctx context.Context, token string) (int64, string, error) {
	id, name, err := a.Site.admin(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", err
	}
	hasAdmin := err == nil // before setup there's nobody to credit an open submission to
	if hasAdmin && !a.Site.RequireScrobbleToken(ctx) {
		return id, name, nil
	}
	if hasAdmin && token != "" && len(token) <= maxScrobbleToken {
		if stored, _, _ := a.Site.get(ctx, scrobbleTokenKey); stored != "" &&
			subtle.ConstantTimeCompare([]byte(stored), []byte(hashScrobbleToken(token))) == 1 {
			return id, name, nil
		}
	}
	return 0, "", errBadToken
}

// MountScrobbling registers:
//
//	GET /api/v1/admin/scrobbling  session            {"require_token","token_set","username"}
//	PUT /api/v1/admin/scrobbling  session + step-up  {"require_token": bool, "token": "…"}
func MountScrobbling(r chi.Router, s *Store) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession)
		r.Get("/api/v1/admin/scrobbling", func(w http.ResponseWriter, req *http.Request) {
			_, name, _ := s.admin(req.Context())
			writeJSON(w, http.StatusOK, map[string]any{
				"require_token": s.RequireScrobbleToken(req.Context()),
				"token_set":     s.scrobbleTokenSet(req.Context()),
				"username":      name,
			})
		})
		r.With(auth.RequireStepUp).Put("/api/v1/admin/scrobbling", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				RequireToken bool   `json:"require_token"`
				Token        string `json:"token"`
			}
			if !decode(w, req, &in) {
				return
			}
			if err := s.SetScrobbleToken(req.Context(), in.RequireToken, in.Token); err != nil {
				if errors.Is(err, errScrobbleToken) {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_token", "message": err.Error()})
					return
				}
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"require_token": in.RequireToken})
		})
	})
}
