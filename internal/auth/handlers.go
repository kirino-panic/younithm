package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/config"
)

// Options configures Mount.
//
// Proxy trust is explicit: forwarded headers (X-Forwarded-Proto/For) are
// honored only when the direct peer (RemoteAddr) is in TrustedProxies, or when
// TrustProxy is set (trust any peer; only safe if the app port is reachable
// solely through the proxy). With neither, forwarded headers are ignored.
type Options struct {
	TrustedProxies []netip.Prefix
	TrustProxy     bool
	// Read, when set, answers session lookups instead of the write pool.
	Read *sql.DB
}

// OptionsFromConfig derives Options from the loaded configuration.
func OptionsFromConfig(c config.Config) (Options, error) {
	pfx, err := c.ProxyPrefixes()
	if err != nil {
		return Options{}, err
	}
	return Options{TrustedProxies: pfx, TrustProxy: c.TrustAllProxies}, nil
}

// Mount registers the auth endpoints under /api/v1 and installs the service
// that RequireSession / RequireStepUp use.
func Mount(r chi.Router, db *sql.DB, opt Options) *Service {
	svc := New(db)
	if opt.Read != nil {
		svc.read = opt.Read
	}
	m := &Middleware{svc: svc, trustProxy: opt.TrustProxy, proxies: opt.TrustedProxies}
	m.install()
	if opt.TrustProxy {
		log.Printf("auth: trusting X-Forwarded-* from ANY peer (YOUNITHM_TRUSTED_PROXY); prefer YOUNITHM_TRUSTED_PROXIES")
	}
	if err := svc.LogSetupPending(context.Background()); err != nil {
		log.Printf("auth: setup check: %v", err)
	}
	h := &handlers{svc: svc, m: m}

	// Full paths (not r.Route) so other tracks can own other /api/v1 subtrees
	// without chi's "Mount on existing path" panic.
	const p = "/api/v1"
	r.Get(p+"/setup/status", h.setupStatus)
	r.Post(p+"/setup", h.setup)
	r.Get(p+"/auth/me", h.me)
	r.Post(p+"/auth/login", h.login)
	r.Group(func(r chi.Router) {
		r.Use(RequireSession)
		r.Post(p+"/auth/logout", h.logout)
		r.Post(p+"/auth/reauth", h.reauth)
		r.Post(p+"/auth/password", h.changePassword)
		r.Post(p+"/auth/username", h.changeUsername)
	})
	return svc
}

type handlers struct {
	svc *Service
	m   *Middleware
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		deny(w, http.StatusBadRequest, "bad_request")
		return false
	}
	return true
}

func (h *handlers) setCookie(w http.ResponseWriter, r *http.Request, token string, exp time.Time) {
	c := &http.Cookie{
		Name: CookieName, Value: token, Path: config.BasePath() + "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: h.m.secure(r),
	}
	if token == "" {
		c.MaxAge = -1
	} else {
		c.Expires = exp
	}
	http.SetCookie(w, c)
}

func (h *handlers) setupStatus(w http.ResponseWriter, r *http.Request) {
	need, err := h.svc.NeedsSetup(r.Context())
	if err != nil {
		deny(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"setup_required": need})
}

func (h *handlers) setup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if !decode(w, r, &in) {
		return
	}
	u, err := h.svc.Setup(r.Context(), in.Username, in.Password)
	switch {
	case errors.Is(err, ErrSetupDone):
		deny(w, http.StatusConflict, "setup_done")
		return
	case err != nil && (strings.HasPrefix(err.Error(), "password") || strings.HasPrefix(err.Error(), "username")):
		// A code per rule, so the setup screen can say exactly what to fix.
		code := "username_length"
		switch {
		case strings.HasPrefix(err.Error(), "password must be at least"):
			code = "password_short"
		case strings.HasPrefix(err.Error(), "password"):
			code = "password_long"
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": code, "message": err.Error(), "min_password": minPassword, "max_password": maxPassword})
		return
	case err != nil:
		log.Printf("auth: setup: %v", err)
		deny(w, http.StatusInternalServerError, "internal")
		return
	}
	res, err := h.svc.createSession(r.Context(), *u, h.m.clientIP(r), r.UserAgent())
	if err != nil {
		deny(w, http.StatusInternalServerError, "internal")
		return
	}
	h.setCookie(w, r, res.Token, res.Expires)
	writeJSON(w, http.StatusCreated, map[string]any{"username": u.Username, "csrf": res.CSRF})
}

func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		if sess, err := h.svc.LookupSession(r.Context(), c.Value); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"authenticated": true, "username": sess.User.Username, "csrf": sess.CSRF,
				"step_up": sess.SteppedUp(h.svc.now()),
			})
			return
		}
	}
	need, _ := h.svc.NeedsSetup(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false, "setup_required": need})
}

func retryAfter(w http.ResponseWriter, d time.Duration) {
	if d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
	}
}

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username, Password string
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Password) > maxPassword || len(in.Username) > 64 {
		deny(w, http.StatusBadRequest, "bad_request")
		return
	}
	res, wait, err := h.svc.Login(r.Context(), h.m.clientIP(r), r.UserAgent(), in.Username, in.Password)
	switch {
	case errors.Is(err, ErrLocked):
		retryAfter(w, wait)
		deny(w, http.StatusTooManyRequests, "locked")
	case errors.Is(err, ErrInvalidCredentials):
		deny(w, http.StatusUnauthorized, "invalid_credentials")
	case err != nil:
		log.Printf("auth: login: %v", err)
		deny(w, http.StatusInternalServerError, "internal")
	default:
		h.setCookie(w, r, res.Token, res.Expires)
		writeJSON(w, http.StatusOK, map[string]any{"username": res.User.Username, "csrf": res.CSRF})
	}
}

func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		_ = h.svc.Logout(r.Context(), c.Value)
	}
	h.setCookie(w, r, "", time.Time{})
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) reauth(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	var in struct {
		Password string
	}
	if !decode(w, r, &in) {
		return
	}
	until, err := h.svc.StepUp(r.Context(), p.Session, h.m.clientIP(r), in.Password)
	switch {
	case errors.Is(err, ErrLocked):
		deny(w, http.StatusTooManyRequests, "locked")
	case errors.Is(err, ErrInvalidCredentials):
		deny(w, http.StatusUnauthorized, "invalid_credentials")
	case err != nil:
		deny(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"step_up_until": until})
	}
}

func (h *handlers) changePassword(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	var in struct {
		Current       string `json:"current"`
		New           string `json:"new"`
		SignOutOthers bool   `json:"sign_out_others"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Current) > maxPassword {
		deny(w, http.StatusBadRequest, "bad_request")
		return
	}
	err := h.svc.ChangePassword(r.Context(), p.Session, in.Current, in.New, in.SignOutOthers)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		deny(w, http.StatusUnauthorized, "invalid_credentials")
	case err != nil && strings.HasPrefix(err.Error(), "password"):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_input", "message": err.Error()})
	case err != nil:
		deny(w, http.StatusInternalServerError, "internal")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handlers) changeUsername(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	var in struct {
		Current  string `json:"current"`
		Username string `json:"username"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Current) > maxPassword {
		deny(w, http.StatusBadRequest, "bad_request")
		return
	}
	name, err := h.svc.ChangeUsername(r.Context(), p.Session, in.Current, in.Username)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		deny(w, http.StatusUnauthorized, "invalid_credentials")
	case errors.Is(err, ErrUsernameTaken):
		deny(w, http.StatusConflict, "username_taken")
	case err != nil && strings.HasPrefix(err.Error(), "username"):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_input", "message": err.Error()})
	case err != nil:
		deny(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"username": name})
	}
}
