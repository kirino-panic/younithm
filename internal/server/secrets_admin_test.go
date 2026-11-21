package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"younithm/internal/db"
	"younithm/internal/secrets"
)

type adminEnv struct {
	t      *testing.T
	srv    *httptest.Server
	pools  *db.Pools
	cookie *http.Cookie
	csrf   string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	pools, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pools.Close() })
	if err := db.Migrate(pools.Write); err != nil {
		t.Fatal(err)
	}
	k := bytes.Repeat([]byte{5}, secrets.KeySize)
	box, _ := secrets.NewBox(k)
	h := New(pools, WithSecrets(box, secrets.NewRedactor()))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &adminEnv{t: t, srv: srv, pools: pools}
}

func (e *adminEnv) do(method, path string, body any) (*http.Response, string) {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, &buf)
	if e.cookie != nil {
		req.AddCookie(e.cookie)
		req.Header.Set("X-CSRF-Token", e.csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.Name == "younithm_session" {
			e.cookie = c
		}
	}
	return resp, string(b)
}

const adminPW = "correct horse battery"

func (e *adminEnv) setup() {
	e.t.Helper()
	resp, out := e.do("POST", "/api/v1/setup", map[string]string{"username": "admin", "password": adminPW})
	if resp.StatusCode != 201 {
		e.t.Fatalf("setup: %d %s", resp.StatusCode, out)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	e.csrf = m["csrf"].(string)
}

func TestSecretsAdminEndpoints(t *testing.T) {
	e := newAdminEnv(t)
	const p = "/api/v1/admin/secrets"

	// Unauthenticated.
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		path := p
		if m != "GET" {
			path += "/spotify.client_secret"
		}
		if resp, _ := e.do(m, path, map[string]string{"value": "x"}); resp.StatusCode != 401 {
			t.Fatalf("%s unauthenticated: %d", m, resp.StatusCode)
		}
	}

	e.setup()

	// Listing works with a session alone and shows everything unset.
	resp, body := e.do("GET", p, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"spotify.client_secret"`) || strings.Contains(body, `"set":true`) {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}

	// Writes need step-up. setup() grants a fresh session but not step-up.
	const secret = "my-spotify-secret-value-a3f2"
	resp, body = e.do("PUT", p+"/spotify.client_secret", map[string]string{"value": secret})
	if resp.StatusCode != 403 || !strings.Contains(body, "step_up_required") {
		t.Fatalf("PUT without step-up: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do("DELETE", p+"/spotify.client_secret", nil); resp.StatusCode != 403 {
		t.Fatalf("DELETE without step-up: %d", resp.StatusCode)
	}
	if resp, body := e.do("POST", "/api/v1/auth/reauth", map[string]string{"password": adminPW}); resp.StatusCode != 200 {
		t.Fatalf("reauth: %d %s", resp.StatusCode, body)
	}

	resp, body = e.do("PUT", p+"/spotify.client_secret", map[string]string{"value": secret})
	if resp.StatusCode != 200 {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, secret) || !strings.Contains(body, `"hint":"a3f2"`) || !strings.Contains(body, `"set":true`) {
		t.Fatalf("PUT response must carry only metadata: %s", body)
	}
	_, body = e.do("GET", p, nil)
	if strings.Contains(body, "my-spotify") || !strings.Contains(body, `"hint":"a3f2"`) {
		t.Fatalf("list leaks or lacks hint: %s", body)
	}

	// At rest: encrypted.
	var ct string
	if err := e.pools.Read.QueryRow(`SELECT ciphertext FROM provider_secret WHERE name='spotify.client_secret'`).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, "my-spotify") || !secrets.IsEncrypted(ct) {
		t.Fatalf("stored %q", ct)
	}

	// No read-back route, whatever the method.
	for _, m := range []string{"GET", "POST", "PATCH"} {
		if resp, body := e.do(m, p+"/spotify.client_secret", nil); resp.StatusCode == 200 || strings.Contains(body, "my-spotify") {
			t.Fatalf("%s read-back route exists: %d %s", m, resp.StatusCode, body)
		}
	}

	// Validation.
	if resp, _ := e.do("PUT", p+"/not.a.provider.secret", map[string]string{"value": "abcdef123456"}); resp.StatusCode != 404 {
		t.Fatalf("unknown name: %d", resp.StatusCode)
	}
	if resp, _ := e.do("PUT", p+"/spotify.client_secret", map[string]string{"value": "  "}); resp.StatusCode != 400 {
		t.Fatalf("blank value: %d", resp.StatusCode)
	}

	if resp, _ := e.do("DELETE", p+"/spotify.client_secret", nil); resp.StatusCode != 204 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	_, body = e.do("GET", p, nil)
	if strings.Contains(body, `"set":true`) {
		t.Fatalf("still set: %s", body)
	}
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
