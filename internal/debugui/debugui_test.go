package debugui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"

	"younithm/internal/db"
)

func TestGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := guard(ok)
	cases := []struct {
		name   string
		method string
		remote string
		host   string
		hdr    map[string]string
		want   int
	}{
		{"loopback GET", "GET", "127.0.0.1:5555", "localhost:8080", nil, 204},
		{"ipv6 loopback GET", "GET", "[::1]:5555", "[::1]:8080", nil, 204},
		{"loopback POST with header", "POST", "127.0.0.1:5555", "127.0.0.1:8080", map[string]string{"X-Debug-UI": "1"}, 204},
		{"POST same origin", "POST", "127.0.0.1:5555", "localhost:8080", map[string]string{"X-Debug-UI": "1", "Origin": "http://localhost:8080"}, 204},
		{"LAN peer", "GET", "192.168.1.20:5555", "localhost:8080", nil, 403},
		{"docker bridge peer", "GET", "172.17.0.1:5555", "localhost:8080", nil, 403},
		{"foreign host (DNS rebinding)", "GET", "127.0.0.1:5555", "evil.example.com", nil, 403},
		{"proxied", "GET", "127.0.0.1:5555", "localhost:8080", map[string]string{"X-Forwarded-For": "8.8.8.8"}, 403},
		{"proxied forwarded", "GET", "127.0.0.1:5555", "localhost:8080", map[string]string{"Forwarded": "for=8.8.8.8"}, 403},
		{"POST without header", "POST", "127.0.0.1:5555", "localhost:8080", nil, 403},
		{"POST cross origin", "POST", "127.0.0.1:5555", "localhost:8080", map[string]string{"X-Debug-UI": "1", "Origin": "https://evil.example.com"}, 403},
		{"DELETE without header", "DELETE", "127.0.0.1:5555", "localhost:8080", nil, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "/debug/api/x", nil)
			r.RemoteAddr, r.Host = c.remote, c.host
			for k, v := range c.hdr {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("got %d, want %d", w.Code, c.want)
			}
		})
	}
}

// Every list and inspect query must run (a typo in one branch would only show
// up when someone clicked that kind), and association sections must be filled.
func TestListAndInspectAllKinds(t *testing.T) {
	pools, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	if err := db.Migrate(pools.Write); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO artist (id, name) VALUES (1, 'Daft Punk')`,
		`INSERT INTO recording (id, title) VALUES (1, 'Get Lucky')`,
		`INSERT INTO release (id, title) VALUES (1, 'Random Access Memories')`,
		`INSERT INTO recording_artist (recording_id, artist_id, role, position) VALUES (1, 1, 'primary', 0)`,
		`INSERT INTO release_artist (release_id, artist_id, position) VALUES (1, 1, 0)`,
		`INSERT INTO recording_release (recording_id, release_id, is_primary) VALUES (1, 1, 1)`,
		`INSERT INTO scrobble (id, played_at, recording_id, release_id, raw_artist, raw_title, raw_album, raw_payload)
		   VALUES (1, '2026-01-01T00:00:00.000Z', 1, 1, 'Daft Punk', 'Get Lucky', 'RAM', '{"a":1}')`,
		`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method) VALUES ('recording', 1, 'spotify', '69kOkLUCkxIZYexIgSqqzx', 'submitted_id')`,
		`INSERT INTO alias (kind, raw_string, entity_id) VALUES ('artist', 'daft punk ', 1)`,
		`INSERT INTO provider_snapshot (provider, provider_id, kind, fetched_at, raw_json) VALUES ('spotify', '69kOkLUCkxIZYexIgSqqzx', 'recording', '2026-01-01T00:00:00.000Z', '{"name":"Get Lucky"}')`,
	} {
		if _, err := pools.Write.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d := Deps{Read: pools.Read, Write: pools.Write}
	r := chi.NewRouter()
	r.Get("/list/{kind}", d.list)
	r.Get("/inspect/{kind}/{id}", d.inspect)
	r.Get("/stats", d.stats)
	get := func(path string, want int) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	for _, kind := range []string{"artist", "recording", "release", "scrobble"} {
		for _, q := range []string{"", "daft", "69kOkLUCkxIZYexIgSqqzx", "1", "50%_"} {
			get("/list/"+kind+"?q="+url.QueryEscape(q), 200)
		}
		get("/list/"+kind+"?sort=id&limit=1&offset=0", 200)
		in := get("/inspect/"+kind+"/1", 200)
		if in["kind"] != kind || in[kind] == nil {
			t.Fatalf("inspect %s: %v", kind, in)
		}
	}
	// A provider id finds the recording; its inspect view carries the cached snapshot decoded.
	if items := get("/list/recording?q=69kOkLUCkxIZYexIgSqqzx", 200)["items"].([]any); len(items) != 1 {
		t.Fatalf("provider id search: %v", items)
	}
	rec := get("/inspect/recording/1", 200)
	snaps := rec["provider_snapshots"].([]any)
	if len(snaps) != 1 || snaps[0].(map[string]any)["raw_json"].(map[string]any)["name"] != "Get Lucky" {
		t.Fatalf("snapshots: %v", snaps)
	}
	if rec["play_count"].(float64) != 1 || len(rec["artists"].([]any)) != 1 || len(rec["releases"].([]any)) != 1 {
		t.Fatalf("associations missing: %v", rec)
	}
	if sc := get("/inspect/scrobble/1", 200); sc["scrobble"].(map[string]any)["raw_payload"].(map[string]any)["a"] != float64(1) {
		t.Fatalf("raw payload should be decoded: %v", sc["scrobble"])
	}
	get("/inspect/artist/999", 404)
	get("/inspect/nope/1", 404)
	get("/inspect/artist/abc", 400)
	get("/list/nope", 404)
	st := get("/stats", 200)
	if st["objects"].(map[string]any)["scrobbles (plays)"].(float64) != 1 {
		t.Fatalf("stats: %v", st["objects"])
	}
}
