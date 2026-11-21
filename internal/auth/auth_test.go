package auth_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
	"younithm/internal/db"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	svc *auth.Service
	jar []*http.Cookie
}

func newEnv(t *testing.T, opt auth.Options) *env {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc := auth.Mount(r, d, opt)
	// A route a different track would own, plus an admin-only one.
	r.With(auth.RequireSession).Post("/api/v1/scrobble", func(w http.ResponseWriter, r *http.Request) {})
	r.With(auth.RequireSession).Get("/api/v1/admin-only", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, svc: svc}
}

func (e *env) do(method, path string, body any, hdr map[string]string) (*http.Response, map[string]any) {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, &buf)
	for _, c := range e.jar {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			e.jar = nil
		} else {
			e.jar = []*http.Cookie{c}
		}
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

const pw = "correct horse battery"

func (e *env) setup() string {
	e.t.Helper()
	resp, out := e.do("POST", "/api/v1/setup", map[string]string{"username": "admin", "password": pw}, nil)
	if resp.StatusCode != 201 {
		e.t.Fatalf("setup: %d %v", resp.StatusCode, out)
	}
	return out["csrf"].(string)
}

func TestSetupFlow(t *testing.T) {
	e := newEnv(t, auth.Options{})
	_, out := e.do("GET", "/api/v1/setup/status", nil, nil)
	if out["setup_required"] != true {
		t.Fatal("expected setup required")
	}
	resp, _ := e.do("POST", "/api/v1/setup", map[string]string{"username": "a", "password": "short"}, nil)
	if resp.StatusCode != 403 && resp.StatusCode != 400 {
		t.Fatalf("short pw: %d", resp.StatusCode)
	}
	e.setup()
	resp, _ = e.do("POST", "/api/v1/setup", map[string]string{"username": "b", "password": pw}, nil)
	if resp.StatusCode == 201 {
		t.Fatal("second setup must fail")
	}
	if resp, _ := e.do("GET", "/api/v1/admin-only", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("auto-login after setup failed: %d", resp.StatusCode)
	}
}

func TestSessionCookieFlagsAndCSRF(t *testing.T) {
	e := newEnv(t, auth.Options{TrustProxy: true})
	csrf := e.setup()
	resp, _ := e.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": pw},
		map[string]string{"X-Forwarded-Proto": "https"})
	c := resp.Cookies()[0]
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags: %+v", c)
	}
	// Plain HTTP, no proxy header: not Secure.
	resp, _ = e.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": pw}, nil)
	if resp.Cookies()[0].Secure {
		t.Fatal("should not be Secure over plain http")
	}
	_ = csrf
	_, out := e.do("GET", "/api/v1/auth/me", nil, nil)
	csrf = out["csrf"].(string)

	// Unsafe request without CSRF token is rejected; with it passes.
	if resp, _ := e.do("POST", "/api/v1/scrobble", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("no csrf: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/v1/scrobble", nil, map[string]string{"X-CSRF-Token": csrf}); resp.StatusCode != 200 {
		t.Fatalf("with csrf: %d", resp.StatusCode)
	}
	// Logout invalidates.
	e.do("POST", "/api/v1/auth/logout", nil, map[string]string{"X-CSRF-Token": csrf})
	if resp, _ := e.do("GET", "/api/v1/admin-only", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t, auth.Options{})
	e.setup()
	e.jar = nil
	for i := 0; i < 6; i++ { // 5 free failures; the 6th starts the first lockout
		resp, _ := e.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong-password"}, nil)
		if resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := e.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": pw}, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected lockout even for correct password, got %d", resp.StatusCode)
	}
}

func TestStepUp(t *testing.T) {
	e := newEnv(t, auth.Options{})
	e.setup()
	_, me := e.do("GET", "/api/v1/auth/me", nil, nil)
	h := map[string]string{"X-CSRF-Token": me["csrf"].(string)}
	if su, _ := me["step_up"].(bool); su {
		t.Fatal("fresh session already stepped up")
	}
	// A wrong password does not open the window; the right one does.
	if resp, _ := e.do("POST", "/api/v1/auth/reauth", map[string]string{"password": "nope-nope-nope"}, h); resp.StatusCode != 401 {
		t.Fatalf("bad reauth: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/v1/auth/reauth", map[string]string{"password": pw}, h); resp.StatusCode != 200 {
		t.Fatalf("reauth: %d", resp.StatusCode)
	}
	_, me = e.do("GET", "/api/v1/auth/me", nil, nil)
	if su, _ := me["step_up"].(bool); !su {
		t.Fatal("reauth did not open the step-up window")
	}
	// Two-factor endpoints are gone.
	for _, p := range []string{"/api/v1/auth/totp/begin", "/api/v1/auth/totp/confirm", "/api/v1/auth/recovery-codes/regenerate"} {
		if resp, _ := e.do("POST", p, nil, h); resp.StatusCode != 404 && resp.StatusCode != 405 {
			t.Fatalf("%s still exists: %d", p, resp.StatusCode)
		}
	}
	// Login needs only the password.
	e.jar = nil
	if resp, _ := e.do("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": pw}, nil); resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
}

func TestChangeUsernameAndPassword(t *testing.T) {
	e := newEnv(t, auth.Options{})
	csrf := e.setup()
	h := map[string]string{"X-CSRF-Token": csrf}

	// Username: needs the current password and a valid name.
	if resp, _ := e.do("POST", "/api/v1/auth/username", map[string]string{"current": "nope-nope-nope", "username": "me"}, h); resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/v1/auth/username", map[string]string{"current": pw, "username": "  "}, h); resp.StatusCode != 400 {
		t.Fatalf("blank name: %d", resp.StatusCode)
	}
	resp, out := e.do("POST", "/api/v1/auth/username", map[string]string{"current": pw, "username": " Me "}, h)
	if resp.StatusCode != 200 || out["username"] != "Me" {
		t.Fatalf("rename: %d %v", resp.StatusCode, out)
	}
	if _, me := e.do("GET", "/api/v1/auth/me", nil, nil); me["username"] != "Me" {
		t.Fatalf("session still shows %v", me["username"])
	}

	// Password: the new one works, the old one doesn't. Other sessions end
	// only when asked.
	other := newEnvClient(e)
	if resp, _ := other.do("POST", "/api/v1/auth/login", map[string]string{"username": "me", "password": pw}, nil); resp.StatusCode != 200 {
		t.Fatalf("login with new name: %d", resp.StatusCode)
	}
	const mid, next = "an in-between password", "a much newer password"
	if resp, _ := e.do("POST", "/api/v1/auth/password", map[string]any{"current": pw, "new": "short"}, h); resp.StatusCode != 400 {
		t.Fatalf("short password: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/v1/auth/password", map[string]any{"current": pw, "new": mid}, h); resp.StatusCode != 204 {
		t.Fatalf("change password: %d", resp.StatusCode)
	}
	if resp, _ := other.do("GET", "/api/v1/admin-only", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("other session ended without being asked: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/v1/auth/password", map[string]any{"current": mid, "new": next, "sign_out_others": true}, h); resp.StatusCode != 204 {
		t.Fatalf("change password and sign out others: %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/api/v1/admin-only", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("own session ended: %d", resp.StatusCode)
	}
	if resp, _ := other.do("GET", "/api/v1/admin-only", nil, nil); resp.StatusCode == 200 {
		t.Fatal("other session survived sign_out_others")
	}
	e.jar = nil
	if resp, _ := e.do("POST", "/api/v1/auth/login", map[string]string{"username": "Me", "password": pw}, nil); resp.StatusCode != 401 {
		t.Fatalf("old password still works: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/v1/auth/login", map[string]string{"username": "Me", "password": next}, nil); resp.StatusCode != 200 {
		t.Fatalf("new password: %d", resp.StatusCode)
	}
}

// Account changes need a session already, so typos there never lock you out.
func TestChangePasswordNotThrottled(t *testing.T) {
	e := newEnv(t, auth.Options{})
	h := map[string]string{"X-CSRF-Token": e.setup()}
	for i := 0; i < 8; i++ {
		if resp, _ := e.do("POST", "/api/v1/auth/password", map[string]string{"current": "nope-nope-nope", "new": "whatever-long-enough"}, h); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := e.do("POST", "/api/v1/auth/password", map[string]string{"current": pw, "new": "whatever-long-enough"}, h); resp.StatusCode != 204 {
		t.Fatalf("right password after typos: %d", resp.StatusCode)
	}
}

// newEnvClient is a second browser against the same server.
func newEnvClient(e *env) *env { return &env{t: e.t, srv: e.srv, svc: e.svc} }

func TestPasswordHash(t *testing.T) {
	h, err := auth.HashPassword("hunter2hunter2")
	if err != nil || !strings.HasPrefix(h, "$argon2id$") {
		t.Fatal(h, err)
	}
	if ok, _ := auth.VerifyPassword("hunter2hunter2", h); !ok {
		t.Fatal("verify failed")
	}
	if ok, _ := auth.VerifyPassword("other", h); ok {
		t.Fatal("wrong password verified")
	}
	if _, err := auth.VerifyPassword("x", "garbage"); err == nil {
		t.Fatal("expected error")
	}
}
