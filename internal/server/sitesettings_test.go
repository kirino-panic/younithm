package server

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
)

func anonGet(t *testing.T, e *adminEnv, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestVisibilityGate(t *testing.T) {
	e := newAdminEnv(t)

	// Public by default.
	for _, p := range []string{"/api/v1/scrobbles", "/api/v1/now-playing"} {
		if c, b := anonGet(t, e, p); c != 200 {
			t.Fatalf("%s default: %d %s", p, c, b)
		}
	}
	if c, b := anonGet(t, e, "/api/v1/site"); c != 200 || !strings.Contains(b, `"scrobbles":true`) {
		t.Fatalf("site: %d %s", c, b)
	}

	// Admin settings need a session.
	if resp, _ := e.do("PUT", "/api/v1/admin/settings/visibility", map[string]any{"sections": map[string]bool{"scrobbles": false}}); resp.StatusCode != 401 {
		t.Fatalf("anon PUT: %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/api/v1/admin/settings", nil); resp.StatusCode != 401 {
		t.Fatalf("anon GET: %d", resp.StatusCode)
	}
	e.setup()

	resp, body := e.do("PUT", "/api/v1/admin/settings/visibility", map[string]any{"sections": map[string]bool{"scrobbles": false, "now_playing": false}})
	if resp.StatusCode != 200 || !strings.Contains(body, `"scrobbles":false`) {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do("PUT", "/api/v1/admin/settings/visibility", map[string]any{"sections": map[string]bool{"bogus": false}}); resp.StatusCode != 400 {
		t.Fatalf("unknown section: %d", resp.StatusCode)
	}

	// Anonymous: 404 on list, now-playing and its stream; detail stays admin-only.
	for _, p := range []string{"/api/v1/scrobbles", "/api/v1/now-playing", "/api/v1/now-playing/stream"} {
		if c, b := anonGet(t, e, p); c != 404 || !strings.Contains(b, "not_found") {
			t.Fatalf("%s hidden: %d %s", p, c, b)
		}
	}
	// Admin still sees everything.
	for _, p := range []string{"/api/v1/scrobbles", "/api/v1/now-playing"} {
		if resp, b := e.do("GET", p, nil); resp.StatusCode != 200 {
			t.Fatalf("admin %s: %d %s", p, resp.StatusCode, b)
		}
	}
	// Re-enable.
	e.do("PUT", "/api/v1/admin/settings/visibility", map[string]any{"sections": map[string]bool{"scrobbles": true}})
	if c, _ := anonGet(t, e, "/api/v1/scrobbles"); c != 200 {
		t.Fatalf("re-enabled list: %d", c)
	}
}

func TestProfile(t *testing.T) {
	e := newAdminEnv(t)
	e.setup()

	if c, b := anonGet(t, e, "/api/v1/site/profile"); c != 200 || !strings.Contains(b, `"display_name":""`) {
		t.Fatalf("empty profile: %d %s", c, b)
	}
	if c, _ := anonGet(t, e, "/api/v1/site/profile/avatar"); c != 404 {
		t.Fatalf("no avatar: %d", c)
	}

	// PUT needs a session.
	req, _ := http.NewRequest("PUT", e.srv.URL+"/api/v1/admin/profile", strings.NewReader(`{"display_name":"x"}`))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("anon PUT: %v %v", resp, err)
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	good := base64.StdEncoding.EncodeToString(buf.Bytes())
	resp, body := e.do("PUT", "/api/v1/admin/profile", map[string]any{"display_name": "  Ace  ", "avatar": "data:image/png;base64," + good})
	if resp.StatusCode != 200 || !strings.Contains(body, `"display_name":"Ace"`) || !strings.Contains(body, "/api/v1/site/profile/avatar?v=") {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	if c, b := anonGet(t, e, "/api/v1/site/profile/avatar"); c != 200 || !strings.HasPrefix(b, "\x89PNG") {
		t.Fatalf("avatar: %d", c)
	}

	// Not square, not a PNG, name too long.
	buf.Reset()
	_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 4)))
	for _, in := range []map[string]any{
		{"avatar": base64.StdEncoding.EncodeToString(buf.Bytes())},
		{"avatar": base64.StdEncoding.EncodeToString([]byte("nope"))},
		{"display_name": strings.Repeat("a", 41)},
	} {
		if resp, _ := e.do("PUT", "/api/v1/admin/profile", in); resp.StatusCode != 400 {
			t.Fatalf("bad input %v: %d", in, resp.StatusCode)
		}
	}

	// Removing the avatar keeps the name.
	if resp, body := e.do("PUT", "/api/v1/admin/profile", map[string]any{"avatar": ""}); resp.StatusCode != 200 || !strings.Contains(body, `"avatar":""`) || !strings.Contains(body, "Ace") {
		t.Fatalf("remove: %d %s", resp.StatusCode, body)
	}
}

func TestDebugSettings(t *testing.T) {
	t.Setenv("YOUNITHM_DEBUG", "1")
	e := newAdminEnv(t)
	if resp, _ := e.do("GET", "/api/v1/admin/debug", nil); resp.StatusCode != 401 {
		t.Fatalf("anon: %d", resp.StatusCode)
	}
	e.setup()

	// Defaults: nothing playing, no import limit.
	if c, b := anonGet(t, e, "/api/v1/now-playing"); c != 200 || !strings.Contains(b, `"playing":false`) {
		t.Fatalf("now-playing default: %d %s", c, b)
	}
	if resp, body := e.do("GET", "/api/v1/admin/debug", nil); resp.StatusCode != 200 || !strings.Contains(body, `"import_limit":0`) {
		t.Fatalf("GET: %d %s", resp.StatusCode, body)
	}

	// Fake now playing shows a track to everyone; switching off removes it.
	if resp, body := e.do("PUT", "/api/v1/admin/debug", map[string]any{"fake_now_playing": true, "import_limit": 50}); resp.StatusCode != 200 || !strings.Contains(body, `"import_limit":50`) {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	if c, b := anonGet(t, e, "/api/v1/now-playing"); c != 200 || !strings.Contains(b, `"playing":true`) || !strings.Contains(b, "Fake Now Playing") {
		t.Fatalf("fake now-playing: %d %s", c, b)
	}
	e.do("PUT", "/api/v1/admin/debug", map[string]any{"fake_now_playing": false})
	if _, b := anonGet(t, e, "/api/v1/now-playing"); !strings.Contains(b, `"playing":false`) {
		t.Fatalf("fake off: %s", b)
	}
	// With tracks in the library, the fake one is taken from it.
	for _, q := range []string{
		`INSERT INTO recording (id, title, duration_ms) VALUES (900, 'Real Song', 200000)`,
		`INSERT INTO artist (id, name) VALUES (900, 'Real Artist')`,
		`INSERT INTO recording_artist (recording_id, artist_id) VALUES (900, 900)`,
		`INSERT INTO release (id, title) VALUES (900, 'Real Album')`,
		`INSERT INTO recording_release (recording_id, release_id, is_primary) VALUES (900, 900, 1)`,
	} {
		if _, err := e.pools.Write.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e.do("PUT", "/api/v1/admin/debug", map[string]any{"fake_now_playing": true})
	if _, b := anonGet(t, e, "/api/v1/now-playing"); !strings.Contains(b, "Real Song") || !strings.Contains(b, "Real Artist") || !strings.Contains(b, "Real Album") {
		t.Fatalf("fake from library: %s", b)
	}
	e.do("PUT", "/api/v1/admin/debug", map[string]any{"fake_now_playing": false})
	if resp, _ := e.do("PUT", "/api/v1/admin/debug", map[string]any{"import_limit": -1}); resp.StatusCode != 400 {
		t.Fatalf("negative limit: %d", resp.StatusCode)
	}

	// Clearing needs the typed confirmation, not a password.
	if _, err := e.pools.Write.Exec(`INSERT INTO artist (name, sort_name) VALUES ('A', 'A')`); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.do("POST", "/api/v1/admin/debug/clear-library", map[string]string{"confirm": "nope"}); resp.StatusCode != 400 {
		t.Fatalf("wrong confirm: %d", resp.StatusCode)
	}
	if resp, body := e.do("POST", "/api/v1/admin/debug/clear-library", map[string]string{"confirm": "DELETE"}); resp.StatusCode != 200 || !strings.Contains(body, `"artists":0`) {
		t.Fatalf("clear: %d %s", resp.StatusCode, body)
	}
	// Settings and the account survive.
	if resp, body := e.do("GET", "/api/v1/admin/debug", nil); resp.StatusCode != 200 || !strings.Contains(body, `"import_limit":50`) {
		t.Fatalf("after clear: %d %s", resp.StatusCode, body)
	}
}

func TestDebugOffByDefault(t *testing.T) {
	t.Setenv("YOUNITHM_DEBUG", "")
	e := newAdminEnv(t)
	e.setup()
	if resp, _ := e.do("GET", "/api/v1/admin/debug", nil); resp.StatusCode == 200 {
		t.Fatalf("debug endpoint should not exist, got %d", resp.StatusCode)
	}
	if resp, body := e.do("GET", "/api/v1/admin/settings", nil); resp.StatusCode != 200 || !strings.Contains(body, `"debug":false`) {
		t.Fatalf("settings: %d %s", resp.StatusCode, body)
	}
}
