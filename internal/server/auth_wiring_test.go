package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"younithm/internal/db"
	"younithm/internal/sitesettings"
)

// Guards against chi route conflicts between auth's /api/v1/auth/... routes
// and the /api/v1 subrouter declared here.
func TestAuthRoutesCoexistWithAPI(t *testing.T) {
	pools, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	if err := db.Migrate(pools.Write); err != nil {
		t.Fatal(err)
	}
	h := New(pools)
	for path, want := range map[string]int{
		"/api/v1/auth/me":      200,
		"/api/v1/setup/status": 200,
		"/api/v1/now-playing":  200,
		"/api/v1/scrobbles":    200, // ingest
		"/healthz":             200,
		"/api/v1/nope":         404,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: got %d want %d", path, rec.Code, want)
		}
	}
}

// End to end: chosen token -> ListenBrainz submit, list public, detail admin-only.
func TestIngestUsesScrobbleToken(t *testing.T) {
	pools, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	if err := db.Migrate(pools.Write); err != nil {
		t.Fatal(err)
	}
	h := New(pools)

	if _, err := pools.Write.Exec(`INSERT INTO admin_user (username, password_hash) VALUES ('ada', 'x')`); err != nil {
		t.Fatal(err)
	}
	const subTok = "scrobbler-token"
	if err := sitesettings.NewStore(pools.Read, pools.Write).SetScrobbleToken(t.Context(), true, subTok); err != nil {
		t.Fatal(err)
	}
	body := `{"listen_type":"single","payload":[{"listened_at":1700000000,"track_metadata":{"artist_name":"A","track_name":"T"}}]}`
	post := func(tok string) int {
		req := httptest.NewRequest("POST", "/1/submit-listens", strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Token "+tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post(subTok); c != 200 {
		t.Fatalf("token submit: %d", c)
	}
	if c := post("legacy"); c != 401 {
		t.Fatalf("legacy env token: %d", c)
	}
	if c := post(""); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	get := func(path string, hdr map[string]string) int {
		req := httptest.NewRequest("GET", path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := get("/api/v1/scrobbles", nil); c != 200 {
		t.Fatalf("public list: %d", c)
	}
	// Detail needs an admin session; the scrobble token is not enough.
	if c := get("/api/v1/scrobbles/1", nil); c != 401 {
		t.Fatalf("anonymous detail: %d", c)
	}
	if c := get("/api/v1/scrobbles/1", map[string]string{"Authorization": "Bearer " + subTok}); c != 401 {
		t.Fatalf("token detail: %d", c)
	}
}
