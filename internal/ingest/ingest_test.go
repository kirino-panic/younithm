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

func TestNowPlayingFinders(t *testing.T) {
	h := &Handler{np: NewNowPlaying()}
	covers := map[string]chan string{"New": make(chan string), "First": make(chan string), "Second": make(chan string)}
	var gotRaw string
	h.SetNowPlayingFinders(NowPlayingFinders{
		Library: func(_ context.Context, raw json.RawMessage, t NowPlayingTrack) NowPlayingTrack {
			gotRaw = string(raw)
			if t.Title == "Known" {
				t.RecordingID, t.Image = 7, "/art/7"
			}
			return t
		},
		Cover: func(_ context.Context, _ json.RawMessage, t NowPlayingTrack) string { return <-covers[t.Title] },
	})
	ctx := context.Background()

	// In the library: shown with its cover at once, no catalogue lookup.
	h.setNowPlaying(ctx, 1, NowPlayingTrack{Artist: "A", Title: "Known"}, "x", json.RawMessage(`{"a":1}`))
	if tr := h.np.Get(1).Track; tr.RecordingID != 7 || tr.Image != "/art/7" || gotRaw != `{"a":1}` {
		t.Fatalf("library: %+v raw %s", tr, gotRaw)
	}

	// Not in the library: shown bare, the cover arrives later and is pushed.
	ch, stop := h.np.Subscribe()
	defer stop()
	h.setNowPlaying(ctx, 1, NowPlayingTrack{Artist: "A", Title: "New"}, "x", nil)
	<-ch
	if tr := h.np.Get(1).Track; tr.Image != "" {
		t.Fatalf("image before lookup: %+v", tr)
	}
	covers["New"] <- "https://cover"
	<-ch
	if tr := h.np.Get(1).Track; tr.Image != "https://cover" {
		t.Fatalf("late cover: %+v", tr)
	}

	// A cover that arrives after the track changed is dropped.
	h.setNowPlaying(ctx, 1, NowPlayingTrack{Artist: "A", Title: "First"}, "x", nil)
	<-ch
	h.np.now = func() time.Time { return time.Now().Add(time.Second) }
	h.setNowPlaying(ctx, 1, NowPlayingTrack{Artist: "A", Title: "Second"}, "x", nil)
	<-ch
	covers["First"] <- "https://first" // first's lookup answers late
	covers["Second"] <- "https://second"
	<-ch
	if tr := h.np.Get(1).Track; tr.Title != "Second" || tr.Image != "https://second" {
		t.Fatalf("stale cover: %+v", tr)
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

func TestUserListens(t *testing.T) {
	h, _ := setup(t)
	for i, title := range []string{"One", "Two", "Three"} {
		do(h, "POST", "/1/submit-listens", single(1700000000+int64(i)*300, "Artist", title), "tok")
	}
	type resp struct {
		Payload struct {
			Count   int `json:"count"`
			Listens []struct {
				ListenedAt    int64 `json:"listened_at"`
				TrackMetadata struct {
					ArtistName  string `json:"artist_name"`
					TrackName   string `json:"track_name"`
					ReleaseName string `json:"release_name"`
				} `json:"track_metadata"`
			} `json:"listens"`
			Latest int64 `json:"latest_listen_ts"`
		} `json:"payload"`
	}
	get := func(path, token string) resp {
		t.Helper()
		w := do(h, "GET", path, "", token)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var out resp
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	all := get("/1/user/admin/listens", "tok")
	if all.Payload.Count != 3 || all.Payload.Listens[0].TrackMetadata.TrackName != "Three" ||
		all.Payload.Listens[0].TrackMetadata.ReleaseName != "Alb" || all.Payload.Latest != 1700000600 {
		t.Fatalf("all: %+v", all)
	}
	// Both bounds are exclusive, as in ListenBrainz.
	win := get("/apis/listenbrainz/1/user/admin/listens?min_ts=1700000000&max_ts=1700000600", "tok")
	if win.Payload.Count != 1 || win.Payload.Listens[0].ListenedAt != 1700000300 {
		t.Fatalf("window: %+v", win)
	}
	if c := get("/1/user/admin/listens?count=2", "tok"); c.Payload.Count != 2 {
		t.Fatalf("count: %+v", c)
	}
	if w := do(h, "GET", "/1/user/admin/listens?min_ts=x", "", "tok"); w.Code != 400 {
		t.Fatalf("bad ts: %d", w.Code)
	}
}

func TestUserListensPrivateList(t *testing.T) {
	d, _ := db.Open(filepath.Join(t.TempDir(), "cfg"))
	t.Cleanup(func() { d.Close() })
	_ = db.Migrate(d)
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	}
	h := New(d, d, Options{Auth: StaticTokenAuth{Token: "tok"}, ListGate: deny})
	r := chi.NewRouter()
	h.MountRoot(r)
	do(r, "POST", "/1/submit-listens", single(1700000000, "Artist", "Song"), "tok")
	if w := do(r, "GET", "/1/user/admin/listens", "", "tok"); w.Code != 200 || !strings.Contains(w.Body.String(), `"count":1`) {
		t.Fatalf("token reads private list: %d %s", w.Code, w.Body.String())
	}
	for _, tok := range []string{"", "bad"} {
		if w := do(r, "GET", "/1/user/admin/listens", "", tok); w.Code != 200 || !strings.Contains(w.Body.String(), `"count":0`) {
			t.Fatalf("no token %q: %d %s", tok, w.Code, w.Body.String())
		}
	}
}
