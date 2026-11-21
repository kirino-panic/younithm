package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/db"
	"younithm/internal/identity"
)

// StaticTokenAuth accepts one fixed token. Test double only.
type StaticTokenAuth struct{ Token string }

func (a StaticTokenAuth) AuthenticateToken(_ context.Context, token string) (int64, string, error) {
	if a.Token == "" || token != a.Token {
		return 0, "", ErrInvalidToken
	}
	return 1, "admin", nil
}

func setup(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	h := New(d, d, Options{Auth: StaticTokenAuth{Token: "tok"}})
	r := chi.NewRouter()
	h.MountRoot(r)
	r.Route("/api/v1", h.MountAPI)
	return r, d
}

func do(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Token "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func single(ts int64, artist, title string) string {
	b, _ := json.Marshal(map[string]any{"listen_type": "single", "payload": []any{map[string]any{
		"listened_at": ts, "track_metadata": map[string]any{"artist_name": artist, "track_name": title,
			"release_name": "Alb", "additional_info": map[string]any{"submission_client": "test"}}}}})
	return string(b)
}

func TestAuth(t *testing.T) {
	h, _ := setup(t)
	if w := do(h, "POST", "/1/submit-listens", single(1700000000, "A", "T"), ""); w.Code != 401 {
		t.Fatalf("no header: %d", w.Code)
	}
	if w := do(h, "POST", "/1/submit-listens", single(1700000000, "A", "T"), "bad"); w.Code != 401 {
		t.Fatalf("bad token: %d", w.Code)
	}
	w := do(h, "GET", "/apis/listenbrainz/1/validate-token", "", "tok")
	if !strings.Contains(w.Body.String(), `"valid":true`) {
		t.Fatal(w.Body.String())
	}
}

func TestSubmitDedupeAndList(t *testing.T) {
	h, _ := setup(t)
	if w := do(h, "POST", "/1/submit-listens", single(1700000000, "Artist", "Song"), "tok"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := do(h, "POST", "/1/submit-listens", single(1700000001, " artist ", "SONG"), "tok")
	if !strings.Contains(w.Body.String(), `"duplicates":1`) {
		t.Fatalf("expected dup: %s", w.Body.String())
	}
	do(h, "POST", "/1/submit-listens", single(1700000300, "Artist", "Other"), "tok")
	do(h, "POST", "/1/submit-listens", single(1700000600, "Artist", "Third"), "tok")

	var list struct {
		Items      []scrobbleItem `json:"items"`
		NextCursor *string        `json:"next_cursor"`
		Total      int            `json:"total"`
	}
	w = do(h, "GET", "/api/v1/scrobbles?limit=2", "", "")
	json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 3 || len(list.Items) != 2 || list.NextCursor == nil || list.Items[0].Title != "Third" {
		t.Fatalf("page1: %s", w.Body.String())
	}
	w = do(h, "GET", "/api/v1/scrobbles?limit=2&cursor="+*list.NextCursor, "", "")
	list.NextCursor = nil
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0].Title != "Song" || list.NextCursor != nil {
		t.Fatalf("page2: %s", w.Body.String())
	}
	w = do(h, "GET", "/api/v1/scrobbles?q=oth", "", "")
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("search: %s", w.Body.String())
	}

	w = do(h, "GET", "/api/v1/scrobbles/1", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"raw_payload":{`) {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	if w = do(h, "GET", "/api/v1/scrobbles/999", "", ""); w.Code != 404 {
		t.Fatalf("404: %d", w.Code)
	}
}

func TestValidation(t *testing.T) {
	h, _ := setup(t)
	for name, body := range map[string]string{
		"badjson":   `{`,
		"noartist":  `{"listen_type":"single","payload":[{"listened_at":1700000000,"track_metadata":{"track_name":"x"}}]}`,
		"future":    single(time.Now().Add(72*time.Hour).Unix(), "A", "T"),
		"two":       `{"listen_type":"single","payload":[{},{}]}`,
		"badtype":   `{"listen_type":"x","payload":[{}]}`,
		"nowithts":  `{"listen_type":"playing_now","payload":[{"listened_at":1700000000,"track_metadata":{"artist_name":"a","track_name":"b"}}]}`,
		"notimport": `{"listen_type":"import","payload":[]}`,
	} {
		if w := do(h, "POST", "/1/submit-listens", body, "tok"); w.Code != 400 {
			t.Errorf("%s: got %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestNowPlaying(t *testing.T) {
	h, _ := setup(t)
	body := `{"listen_type":"playing_now","payload":[{"track_metadata":{"artist_name":"A","track_name":"T","additional_info":{"duration_ms":200000}}}]}`
	if w := do(h, "POST", "/1/submit-listens", body, "tok"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := do(h, "GET", "/api/v1/now-playing", "", "")
	if !strings.Contains(w.Body.String(), `"playing":true`) {
		t.Fatal(w.Body.String())
	}
	do(h, "POST", "/1/submit-listens", single(time.Now().Unix(), "a", "t"), "tok")
	w = do(h, "GET", "/api/v1/now-playing", "", "")
	if strings.TrimSpace(w.Body.String()) != `{"playing":false}` {
		t.Fatal(w.Body.String())
	}
}

func TestNowPlayingExpiry(t *testing.T) {
	n := NewNowPlaying()
	now := time.Now()
	n.now = func() time.Time { return now }
	n.Set(1, NowPlayingTrack{Artist: "a", Title: "t"}, "x")
	now = now.Add(6 * time.Minute)
	if n.Get(1).Playing {
		t.Fatal("should expire")
	}
}

func TestDefaultAuthRejectsEverything(t *testing.T) {
	d, _ := db.Open(filepath.Join(t.TempDir(), "cfg"))
	t.Cleanup(func() { d.Close() })
	_ = db.Migrate(d)
	r := chi.NewRouter()
	New(d, d, Options{}).MountRoot(r)
	if w := do(r, "POST", "/1/submit-listens", single(1700000000, "A", "T"), "anything"); w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestListGateAndDetailGate(t *testing.T) {
	d, _ := db.Open(filepath.Join(t.TempDir(), "cfg"))
	t.Cleanup(func() { d.Close() })
	_ = db.Migrate(d)
	deny := func(code int) func(http.Handler) http.Handler {
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		}
	}
	r := chi.NewRouter()
	h := New(d, d, Options{ListGate: deny(404), RequireAdmin: deny(401)})
	r.Route("/api/v1", h.MountAPI)
	if w := do(r, "GET", "/api/v1/scrobbles", "", ""); w.Code != 404 {
		t.Fatalf("gated list: %d", w.Code)
	}
	if w := do(r, "GET", "/api/v1/scrobbles/1", "", ""); w.Code != 401 {
		t.Fatalf("gated detail: %d", w.Code)
	}
	if w := do(r, "GET", "/api/v1/now-playing", "", ""); w.Code != 200 {
		t.Fatalf("now-playing: %d", w.Code)
	}
}

func TestIdentifyCreatesCreditedArtistsAtIngest(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	identify := func(ctx context.Context, tx *sql.Tx, l NewScrobble) (int64, int64, error) {
		return (&identity.Resolver{}).Resolve(ctx, tx, identity.FromBasics(l.RawPayload, identity.Basics{
			Artist: l.Artist, Title: l.Title, Album: l.Album, ArtistMBIDs: l.ArtistMBIDs,
		}))
	}
	h := New(d, d, Options{Auth: StaticTokenAuth{Token: "tok"}, Identify: identify})
	r := chi.NewRouter()
	h.MountRoot(r)
	r.Route("/api/v1", h.MountAPI)
	body := `{"listen_type":"single","payload":[{"listened_at":1700000000,"track_metadata":{
		"artist_name":"Ninajirachi, Porter Robinson","track_name":"iPod Touch","release_name":"I Love My Computer",
		"additional_info":{"artist_names":["Ninajirachi","Porter Robinson"],"spotify_artist_ids":["https://open.spotify.com/artist/a","https://open.spotify.com/artist/b"]}}}]}`
	if w := do(r, "POST", "/1/submit-listens", body, "tok"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var rec sql.NullInt64
	if err := d.QueryRow(`SELECT recording_id FROM scrobble`).Scan(&rec); err != nil || !rec.Valid {
		t.Fatalf("scrobble not linked: %v %v", rec, err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec.Int64).Scan(&n)
	if n != 2 {
		t.Fatalf("credited artists=%d", n)
	}
	var raw string
	d.QueryRow(`SELECT raw_artist FROM scrobble`).Scan(&raw)
	if raw != "Ninajirachi, Porter Robinson" {
		t.Fatalf("raw artist rewritten: %q", raw)
	}
	// The list carries each credited artist separately, in order, so each can link.
	var list struct {
		Items []struct {
			Artists []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"items"`
	}
	w := do(r, "GET", "/api/v1/scrobbles", "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %v %s", err, w.Body)
	}
	if a := list.Items[0].Artists; len(a) != 2 || a[0].Name != "Ninajirachi" || a[1].Name != "Porter Robinson" || a[0].ID == 0 {
		t.Fatalf("artists: %+v", a)
	}
}

func TestTitledListenWithoutRecordingIsRefused(t *testing.T) {
	_, d := setup(t)
	st := &Store{DB: d, DedupeTolerance: 2 * time.Second}
	st.Identify = func(context.Context, *sql.Tx, NewScrobble) (int64, int64, error) { return 0, 0, nil }
	if _, _, err := st.Insert(context.Background(), []NewScrobble{{PlayedAt: time.Unix(1700000000, 0), Artist: "A", Title: "T"}}); err == nil {
		t.Fatal("a titled listen was stored without a recording")
	}
	// No title: nothing to map, still accepted.
	if _, _, err := st.Insert(context.Background(), []NewScrobble{{PlayedAt: time.Unix(1700000100, 0), Artist: "A", Title: " "}}); err != nil {
		t.Fatalf("untitled listen: %v", err)
	}
}
