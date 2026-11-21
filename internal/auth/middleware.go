package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
)

const (
	// CookieName is the session cookie.
	CookieName = "younithm_session"
	// CSRFHeader must carry Session.CSRF on unsafe requests made with a session cookie.
	CSRFHeader = "X-CSRF-Token"
)

// Principal identifies who is making a request: always the admin's session.
type Principal struct {
	UserID  int64
	Session *Session
}

type ctxKey struct{}

// PrincipalFrom returns the authenticated principal, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// Middleware holds config shared by the HTTP layer.
type Middleware struct {
	svc        *Service
	trustProxy bool           // trust forwarded headers from any peer
	proxies    []netip.Prefix // trust forwarded headers from these peers
}

type defaultHolder struct{ mw *Middleware }

var defaultSvc atomic.Pointer[defaultHolder]

func (m *Middleware) install() { defaultSvc.Store(&defaultHolder{mw: m}) }

func deny(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": code})
}

// RequireSession allows only a valid admin session. Unsafe methods must carry the CSRF token.
func RequireSession(next http.Handler) http.Handler {
	return withDefault(func(m *Middleware) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, status, code := m.sessionPrincipal(r)
			if p == nil {
				deny(w, status, code)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
		})
	})
}

// RequireStepUp must be chained after RequireSession. It demands a recent
// re-authentication (POST /api/v1/auth/reauth); otherwise 403 "step_up_required".
func RequireStepUp(next http.Handler) http.Handler {
	return withDefault(func(m *Middleware) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if p == nil || p.Session == nil {
				deny(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			if !p.Session.SteppedUp(m.svc.now()) {
				deny(w, http.StatusForbidden, "step_up_required")
				return
			}
			next.ServeHTTP(w, r)
		})
	})
}

// withDefault resolves the installed Middleware lazily so routes can be
// declared before Mount runs.
func withDefault(build func(*Middleware) http.Handler) http.Handler {
	var cached atomic.Pointer[http.Handler]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := cached.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		d := defaultSvc.Load()
		if d == nil {
			deny(w, http.StatusServiceUnavailable, "auth_not_ready")
			return
		}
		h := build(d.mw)
		cached.Store(&h)
		h.ServeHTTP(w, r)
	})
}

func isSafe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// sessionPrincipal authenticates the cookie and enforces CSRF for unsafe methods.
func (m *Middleware) sessionPrincipal(r *http.Request) (*Principal, int, string) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil, http.StatusUnauthorized, "unauthenticated"
	}
	sess, err := m.svc.LookupSession(r.Context(), c.Value)
	if err != nil {
		if errors.Is(err, ErrNoSession) {
			return nil, http.StatusUnauthorized, "unauthenticated"
		}
		return nil, http.StatusInternalServerError, "internal"
	}
	if !isSafe(r.Method) {
		if !ctEqual(r.Header.Get(CSRFHeader), sess.CSRF) {
			return nil, http.StatusForbidden, "csrf"
		}
	}
	return &Principal{UserID: sess.User.ID, Session: sess}, 0, ""
}

// peerTrusted reports whether the direct TCP peer is a trusted proxy, i.e.
// whether X-Forwarded-* headers on this request may be believed.
func (m *Middleware) peerTrusted(r *http.Request) bool {
	if m.trustProxy {
		return true
	}
	return m.ipTrusted(peerAddr(r))
}

func (m *Middleware) ipTrusted(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	for _, p := range m.proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func peerAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// secure reports whether the Secure cookie flag should be set: the request
// arrived over TLS or a trusted proxy says it did (X-Forwarded-Proto).
func (m *Middleware) secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if m.peerTrusted(r) && strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https") {
		return true
	}
	return false
}

// clientIP is the throttle / audit key. Without a trusted proxy it is the
// socket peer. With one, X-Forwarded-For is walked right to left, skipping
// trusted proxy addresses, and the first untrusted hop is the client (earlier
// entries are client-controlled and never believed). With TrustProxy (trust
// any peer) the rightmost entry is used, as the single proxy appended it.
func (m *Middleware) clientIP(r *http.Request) string {
	peer := peerAddr(r)
	if m.peerTrusted(r) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
				if err != nil {
					break // malformed hop: stop trusting the chain
				}
				a = a.Unmap()
				if m.ipTrusted(a) {
					continue
				}
				return a.String()
			}
		}
	}
	if peer.IsValid() {
		return peer.String()
	}
	return r.RemoteAddr
}
