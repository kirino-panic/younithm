package artwork

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/db"
	"younithm/internal/providers"
)

// A 1x1 PNG.
var png, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")

type fake struct {
	name    string
	art     map[string][]providers.ImageRef
	artist  map[string][]providers.ImageRef
	err     error
	release atomic.Int32
}

func (f *fake) Name() string { return f.name }
func (f *fake) SearchTrack(context.Context, providers.TrackQuery) ([]providers.Track, error) {
	return nil, providers.ErrUnsupported
}
func (f *fake) GetTrack(context.Context, string) (*providers.Track, error) {
	return nil, providers.ErrUnsupported
}
func (f *fake) GetArtwork(_ context.Context, id string) ([]providers.ImageRef, error) {
	f.release.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.art[id]; ok {
		return r, nil
	}
	return nil, providers.ErrNotFound
}
func (f *fake) GetArtistImage(_ context.Context, id string) ([]providers.ImageRef, error) {
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.artist[id]; ok {
		return r, nil
	}
	return nil, providers.ErrUnsupported
}

type env struct {
	w   *Worker
	d   *db.Pools
	dir string
	now time.Time
	srv *httptest.Server
	hit atomic.Int32
}

func setup(t *testing.T, ps ...providers.Provider) *env {
	t.Helper()
	d, err := db.OpenPools(filepath.Join(t.TempDir(), "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d.Write); err != nil {
		t.Fatal(err)
	}
	e := &env{d: d, dir: filepath.Join(t.TempDir(), "art"), now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hit.Add(1)
		switch r.URL.Path {
		case "/cover.png":
			w.Write(png)
		case "/page.html":
			w.Write([]byte("<html>not an image</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.srv.Close)
	e.w = New(d.Read, d.Write, e.dir, providers.NewRegistry(ps...), Options{
		Client: e.srv.Client(), Now: func() time.Time { return e.now },
	})
	return e
}

func (e *env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.d.Write.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *env) str(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := e.d.Read.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}

func (e *env) seed(t *testing.T) {
	e.exec(t, `INSERT INTO artist(id,name) VALUES (1,'A')`)
	e.exec(t, `INSERT INTO release(id,title) VALUES (1,'Album')`)
	e.exec(t, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('release',1,'spotify','sp-rel','isrc','auto'),('artist',1,'spotify','sp-art','isrc','auto')`)
}

func TestFetchStoresReleaseAndArtist(t *testing.T) {
	f := &fake{name: "spotify"}
	e := setup(t, f)
	f.art = map[string][]providers.ImageRef{"sp-rel": {
		{URL: e.srv.URL + "/small.png", Width: 64, Kind: "front"},
		{URL: e.srv.URL + "/cover.png", Width: 640, Kind: "front"},
	}}
	f.artist = map[string][]providers.ImageRef{"sp-art": {{URL: e.srv.URL + "/cover.png", Width: 640, Kind: "artist"}}}
	e.seed(t)

	n, err := e.w.Pass(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("pass: n=%d err=%v", n, err)
	}
	rel := e.str(t, `SELECT artwork_path FROM release WHERE id=1`)
	art := e.str(t, `SELECT image_path FROM artist WHERE id=1`)
	if !strings.HasPrefix(rel, "release-1-") || !strings.HasSuffix(rel, ".png") || !strings.HasPrefix(art, "artist-1-") {
		t.Fatalf("paths %q %q", rel, art)
	}
	if b, err := os.ReadFile(filepath.Join(e.dir, rel)); err != nil || !bytes.Equal(b, png) {
		t.Fatalf("stored file: %v", err)
	}
	if u := URL("release", 1, rel); !strings.HasPrefix(u, "/api/v1/art/release/1?v=") {
		t.Fatalf("url %q", u)
	}
	// Done: nothing left to do.
	if n, _ := e.w.Pass(context.Background()); n != 0 {
		t.Fatalf("second pass looked at %d", n)
	}
}

func TestNeverOverwritesAndNeedsALink(t *testing.T) {
	f := &fake{name: "spotify"}
	e := setup(t, f)
	e.exec(t, `INSERT INTO release(id,title,artwork_path) VALUES (1,'Set','mine.png'),(2,'No link',NULL)`)
	e.exec(t, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('release',1,'spotify','x','isrc','auto'),('release',2,'spotify','y','name','candidate')`)
	if n, _ := e.w.Pass(context.Background()); n != 0 {
		t.Fatalf("looked at %d: set images and candidate-only links are not work", n)
	}
	if f.release.Load() != 0 {
		t.Fatal("asked a provider")
	}
}

func TestKeepSetsOnlyAnEmptyCover(t *testing.T) {
	e := setup(t)
	e.seed(t)
	ctx := context.Background()
	if ok, err := e.w.Keep(ctx, 1, e.srv.URL+"/page.html"); ok || err == nil {
		t.Fatalf("not an image: %v %v", ok, err)
	}
	if ok, err := e.w.Keep(ctx, 1, e.srv.URL+"/cover.png"); !ok || err != nil {
		t.Fatalf("keep: %v %v", ok, err)
	}
	path := e.str(t, `SELECT artwork_path FROM release WHERE id=1`)
	if path == "" {
		t.Fatal("no artwork stored")
	}
	n := e.hit.Load()
	if ok, err := e.w.Keep(ctx, 1, e.srv.URL+"/cover.png"); ok || err != nil || e.hit.Load() != n {
		t.Fatalf("second keep: %v %v, downloaded again: %v", ok, err, e.hit.Load() != n)
	}
	if ok, err := e.w.Keep(ctx, 99, e.srv.URL+"/cover.png"); ok || err != nil {
		t.Fatalf("missing release: %v %v", ok, err)
	}
}

func TestMissesAreRemembered(t *testing.T) {
	f := &fake{name: "spotify"}
	e := setup(t, f)
	e.seed(t)
	if n, _ := e.w.Pass(context.Background()); n != 2 {
		t.Fatalf("n=%d", n)
	}
	calls := f.release.Load()
	e.now = e.now.Add(24 * time.Hour)
	if n, _ := e.w.Pass(context.Background()); n != 0 || f.release.Load() != calls {
		t.Fatalf("retried a miss after a day: n=%d", n)
	}
	e.now = e.now.Add(15 * 24 * time.Hour)
	if n, _ := e.w.Pass(context.Background()); n != 2 {
		t.Fatalf("not retried after the window: n=%d", n)
	}
	if got := e.str(t, `SELECT outcome FROM artwork_try WHERE entity_type='release'`); got != "none" {
		t.Fatalf("outcome %q", got)
	}
}

func TestBadDownloadFallsThroughAndRetriesSoon(t *testing.T) {
	sp := &fake{name: "spotify"}
	ap := &fake{name: "apple"}
	e := setup(t, sp, ap)
	sp.art = map[string][]providers.ImageRef{"sp-rel": {{URL: e.srv.URL + "/page.html", Width: 640, Kind: "front"}}}
	ap.art = map[string][]providers.ImageRef{"ap-rel": {{URL: e.srv.URL + "/cover.png", Width: 600, Kind: "front"}}}
	e.exec(t, `INSERT INTO release(id,title) VALUES (1,'Album')`)
	e.exec(t, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('release',1,'spotify','sp-rel','isrc','auto'),('release',1,'apple','ap-rel','isrc','auto')`)
	if _, err := e.w.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.str(t, `SELECT artwork_path FROM release WHERE id=1`) == "" {
		t.Fatal("an HTML page from the first provider must not stop the second")
	}

	// Only a failing source: recorded as an error and retried within the hour.
	e.exec(t, `INSERT INTO release(id,title) VALUES (2,'Broken')`)
	e.exec(t, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES ('release',2,'spotify','bad','isrc','auto')`)
	sp.art["bad"] = []providers.ImageRef{{URL: e.srv.URL + "/page.html", Width: 640, Kind: "front"}}
	e.w.Pass(context.Background())
	if got := e.str(t, `SELECT outcome FROM artwork_try WHERE entity_id=2 AND entity_type='release'`); got != "error" {
		t.Fatalf("outcome %q", got)
	}
	e.now = e.now.Add(2 * time.Hour)
	if n, _ := e.w.Pass(context.Background()); n != 1 {
		t.Fatalf("error not retried after an hour: n=%d", n)
	}
}

func TestRateLimitStopsPassWithoutRecording(t *testing.T) {
	f := &fake{name: "spotify", err: &providers.RateLimitedError{Provider: "spotify", RetryAfter: 5}}
	e := setup(t, f)
	e.seed(t)
	if _, err := e.w.Pass(context.Background()); err == nil {
		t.Fatal("want the rate-limit error")
	}
	if got := e.str(t, `SELECT COUNT(*) FROM artwork_try`); got != "0" {
		t.Fatalf("recorded %s tries", got)
	}
}

func TestPick(t *testing.T) {
	refs := []providers.ImageRef{
		{URL: "1000", Width: 1000, Kind: "front"}, {URL: "600", Width: 600, Kind: "front"}, {URL: "100", Width: 100, Kind: "front"},
	}
	if r, _ := pick(refs); r.URL != "600" {
		t.Fatalf("got %s", r.URL)
	}
	if r, _ := pick([]providers.ImageRef{{URL: "64", Width: 64, Kind: "front"}, {URL: "300", Width: 300, Kind: "front"}}); r.URL != "300" {
		t.Fatalf("widest when none reach the target, got %s", r.URL)
	}
	if r, _ := pick([]providers.ImageRef{{URL: "back", Kind: "back"}, {URL: "f", Kind: "front"}}); r.URL != "f" {
		t.Fatalf("back covers are not album art, got %s", r.URL)
	}
	if _, ok := pick([]providers.ImageRef{{URL: "back", Kind: "back"}}); ok {
		t.Fatal("only a back cover: no image")
	}
	if r, _ := pick([]providers.ImageRef{{URL: "first"}, {URL: "second"}}); r.URL != "first" {
		t.Fatalf("unsized refs rank as given, got %s", r.URL)
	}
}

func TestGuardedClientRefusesLocalAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(png) }))
	defer srv.Close()
	w := &Worker{o: Options{Client: guardedClient()}}
	if _, _, err := w.download(context.Background(), srv.URL+"/x.png"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("loopback must be refused, got %v", err)
	}
}

func TestURLVersion(t *testing.T) {
	if got := URL("artist", 7, "artist-7-ab12cd34.jpg"); got != "/api/v1/art/artist/7?v=ab12cd34" {
		t.Fatal(got)
	}
	if got := URL("release", 3, "cover.png"); got != "/api/v1/art/release/3" {
		t.Fatalf("a hand-set name has no version: %s", got)
	}
	if URL("release", 3, "") != "" {
		t.Fatal("no path, no url")
	}
}

func TestServe(t *testing.T) {
	f := &fake{name: "spotify"}
	e := setup(t, f)
	f.art = map[string][]providers.ImageRef{"sp-rel": {{URL: e.srv.URL + "/cover.png", Width: 640, Kind: "front"}}}
	e.seed(t)
	e.w.Pass(context.Background())
	name := e.str(t, `SELECT artwork_path FROM release WHERE id=1`)

	r := chi.NewRouter()
	r.Get("/api/v1/art/{kind}/{id}", Handler{Read: e.d.Read, Dir: e.dir}.ServeHTTP)
	get := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w
	}
	w := get(URL("release", 1, name))
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), png) {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("versioned url must cache forever: %q", w.Header().Get("Cache-Control"))
	}
	if get("/api/v1/art/release/1").Header().Get("Cache-Control") == w.Header().Get("Cache-Control") {
		t.Fatal("an unversioned url must not be immutable")
	}
	for _, p := range []string{"/api/v1/art/release/99", "/api/v1/art/track/1", "/api/v1/art/release/x", "/api/v1/art/artist/1"} {
		if c := get(p).Code; c != 404 {
			t.Fatalf("%s: %d", p, c)
		}
	}
	// A hand-edited path must not reach outside the folder.
	os.WriteFile(filepath.Join(filepath.Dir(e.dir), "secret.png"), png, 0o644)
	e.exec(t, `UPDATE release SET artwork_path='../secret.png' WHERE id=1`)
	if c := get("/api/v1/art/release/1").Code; c != 404 {
		t.Fatalf("path traversal served: %d", c)
	}
}

// An image chosen by hand is checked by content and shows over the fetched one,
// which stays; clearing it shows the fetched one again.
func TestUploadOverridesFetched(t *testing.T) {
	e := setup(t)
	e.exec(t, `INSERT INTO release(id,title,artwork_path) VALUES (1,'Album','fetched.png')`)
	if _, err := SaveUpload(e.dir, KindRelease, 1, []byte("<html>no</html>")); err != ErrNotImage {
		t.Fatalf("non-image: %v", err)
	}
	if _, err := SaveUpload(e.dir, "track", 1, png); err == nil {
		t.Fatal("tracks have no image of their own")
	}
	name, err := SaveUpload(e.dir, KindRelease, 1, png)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(e.dir, name)); err != nil || !bytes.Equal(b, png) {
		t.Fatalf("saved file: %v", err)
	}
	e.exec(t, `UPDATE release SET image_override = ? WHERE id = 1`, name)
	if got := e.str(t, `SELECT `+ReleaseImage("release")+` FROM release WHERE id = 1`); got != name {
		t.Fatalf("shown %q", got)
	}
	r := chi.NewRouter()
	r.Get("/api/v1/art/{kind}/{id}", Handler{Read: e.d.Read, Dir: e.dir}.ServeHTTP)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", URL(KindRelease, 1, name), nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), png) {
		t.Fatalf("serve override: %d", w.Code)
	}
	e.exec(t, `UPDATE release SET image_override = ? WHERE id = 1`, NoImage)
	if got := e.str(t, `SELECT COALESCE(`+ReleaseImage("release")+`, 'none') FROM release WHERE id = 1`); got != "none" {
		t.Fatalf("removed shows %q", got)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/art/release/1", nil))
	if w.Code != 404 {
		t.Fatalf("serve removed: %d", w.Code)
	}
	e.exec(t, `UPDATE release SET image_override = NULL WHERE id = 1`)
	if got := e.str(t, `SELECT `+ReleaseImage("release")+` FROM release WHERE id = 1`); got != "fetched.png" {
		t.Fatalf("after clear %q", got)
	}
}
