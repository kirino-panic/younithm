package spotify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"younithm/internal/providers"
)

const (
	idA = "4uLU6hMCjMI75M1A2tKUQC" // 22 chars
	idB = "7ouMYWpwJ422jRcDASZB7P"
	idC = "0Vi4FvLpkr2sLnTNNaB9qn"
)

const trackJSON = `{"id":"` + idA + `","name":"Great Song","duration_ms":201000,"disc_number":1,"track_number":3,"popularity":%d,
"artists":[{"id":"art1","name":"The Band"}],
"album":{"id":"alb1","name":"Album","release_date":"2001-02-03","album_type":"ALBUM","images":[{"url":"http://i/640","width":640,"height":640}]},
"external_ids":{"isrc":"usabc0100001"}}`

type fake struct {
	srv       *httptest.Server
	tokens    int32
	apiCalls  int32
	api       http.HandlerFunc
	tokenCode int
	expiresIn string
}

func newFake(t *testing.T, api http.HandlerFunc) (*Client, *fake) {
	t.Helper()
	f := &fake{api: api, tokenCode: 200, expiresIn: "3600"}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.tokens, 1)
		if u, p, ok := r.BasicAuth(); !ok || u != "cid" || p != "sec" {
			w.WriteHeader(401)
			return
		}
		if r.PostFormValue("grant_type") != "client_credentials" {
			w.WriteHeader(400)
			return
		}
		if f.tokenCode != 200 {
			w.WriteHeader(f.tokenCode)
			return
		}
		w.Write([]byte(`{"access_token":"tok` + string(rune('0'+atomic.LoadInt32(&f.tokens))) + `","expires_in":` + f.expiresIn + `}`))
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.apiCalls, 1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok") {
			w.WriteHeader(401)
			return
		}
		f.api(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	c, err := New(Config{
		APIURL: f.srv.URL + "/v1", TokenURL: f.srv.URL + "/token",
		Credentials: func(context.Context) (string, string, error) { return "cid", "sec", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func okTrack(pop int) string { return strings.Replace(trackJSON, "%d", string(rune('0'+pop)), 1) }

func TestSearchMapsTrackAndCachesToken(t *testing.T) {
	c, f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" || r.URL.Query().Get("type") != "track" ||
			r.URL.Query().Get("q") != `track:"Great Song" artist:"The Band"` {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Write([]byte(`{"tracks":{"items":[` + okTrack(5) + `]}}`))
	})
	q := providers.TrackQuery{Artist: "The Band", Title: "Great Song", Album: "ignored"}
	for i := 0; i < 2; i++ {
		got, err := c.SearchTrack(context.Background(), q)
		if err != nil || len(got) != 1 {
			t.Fatalf("%v %v", got, err)
		}
		tr := got[0]
		if tr.Provider != "spotify" || tr.ProviderID != idA || tr.DurationMs != 201000 ||
			tr.Artists[0].Name != "The Band" || tr.ISRCs[0] != "USABC0100001" ||
			tr.Releases[0].Title != "Album" || tr.Releases[0].Type != "album" || tr.Releases[0].Position != 3 {
			t.Fatalf("mapped %+v", tr)
		}
	}
	if f.tokens != 1 {
		t.Fatalf("token fetched %d times, want 1", f.tokens)
	}
}

func TestTokenRefreshedWhenExpiredOrRejected(t *testing.T) {
	c, f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"tracks":{"items":[]}}`)) })
	now := time.Now()
	c.cfg.Now = func() time.Time { return now }
	f.expiresIn = "100"
	c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "a", Title: "b"})
	now = now.Add(80 * time.Second) // inside the 30s safety margin
	c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "a", Title: "b"})
	if f.tokens != 2 {
		t.Fatalf("tokens %d, want a refresh near expiry", f.tokens)
	}

	// A 401 from the API forces one refresh and a retry.
	var first int32
	f.api = func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&first, 1) == 1 {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"tracks":{"items":[]}}`))
	}
	before := f.tokens
	if _, err := c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "a", Title: "b"}); err != nil {
		t.Fatal(err)
	}
	if f.tokens != before+1 {
		t.Fatalf("tokens %d -> %d, want one refresh", before, f.tokens)
	}
}

func TestNotConfiguredAndRejectedCredentials(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("err %v", err)
	}
	empty, _ := New(Config{Credentials: func(context.Context) (string, string, error) { return "", "", nil }})
	if _, err := empty.SearchTrack(context.Background(), providers.TrackQuery{Artist: "a", Title: "b"}); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("empty creds: %v", err)
	}
	c, _ := newFake(t, func(http.ResponseWriter, *http.Request) { t.Error("API called without a token") })
	c.cfg.Credentials = func(context.Context) (string, string, error) { return "cid", "wrong", nil }
	if _, err := c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "a", Title: "b"}); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("bad creds: %v", err)
	}
}

func TestRateLimitAndNotFound(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, idB) {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(429)
	})
	_, err := c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "a", Title: "b"})
	var rl *providers.RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 42 {
		t.Fatalf("err %v", err)
	}
	if _, err := c.GetTrack(context.Background(), idB); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if _, err := c.GetTrack(context.Background(), "not-an-id"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestISRCLookupCollapsesCopiesToMostPopular(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "isrc:USABC0100001" {
			t.Errorf("q = %q", r.URL.Query().Get("q"))
		}
		a := strings.Replace(okTrack(2), idA, idA, 1)
		b := strings.Replace(okTrack(9), idA, idC, 1)
		other := strings.Replace(strings.Replace(okTrack(9), idA, idB, 1), "usabc0100001", "zzzzz9999999", 1)
		w.Write([]byte(`{"tracks":{"items":[` + a + `,` + b + `,` + other + `]}}`))
	})
	got, err := c.LookupISRC(context.Background(), "US-ABC-01-00001")
	if err != nil || len(got) != 1 || got[0].ProviderID != idC {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ := c.LookupISRC(context.Background(), "short"); got != nil {
		t.Fatal("short isrc should be rejected")
	}
}

func TestArtworkAndArtistImage(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"images":[{"url":"http://i/big","width":640,"height":640},{"url":"http://i/small","width":64,"height":64}]}`))
	})
	art, err := c.GetArtwork(context.Background(), idA)
	if err != nil || len(art) != 2 || art[0].Kind != "front" || art[0].Width != 640 {
		t.Fatalf("%+v %v", art, err)
	}
	img, err := c.GetArtistImage(context.Background(), idA)
	if err != nil || img[0].Kind != "artist" {
		t.Fatalf("%+v %v", img, err)
	}
}

func TestBuildQueryStripsQuotes(t *testing.T) {
	got := BuildQuery(providers.TrackQuery{Title: `Say "Hi"`, Artist: ` A  B `})
	if got != `track:"Say Hi" artist:"A B"` {
		t.Fatal(got)
	}
}

func TestGetAlbumReturnsBarcodeAndDate(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/albums/"+idB {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"id":"` + idB + `","name":"Power Move","release_date":"2025-11-14","total_tracks":11,
			"external_ids":{"upc":"199891213535"},"images":[]}`))
	})
	a, err := c.GetAlbum(context.Background(), idB)
	if err != nil {
		t.Fatal(err)
	}
	if a.UPC != "199891213535" || a.Date != "2025-11-14" || a.TotalTracks != 11 || a.Title != "Power Move" {
		t.Fatalf("%+v", a)
	}
	if _, err := c.GetAlbum(context.Background(), "not-an-id"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestAcceptLanguageIsEnforcedAndOverridable(t *testing.T) {
	var got []string
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Accept-Language"))
		if r.URL.Path == "/v1/artists" {
			w.Write([]byte(`{"artists":[{"id":"` + idA + `","name":"米津玄師"},null]}`))
			return
		}
		w.Write([]byte(okTrack(5)))
	})
	if _, err := c.GetTrack(context.Background(), idA); err != nil {
		t.Fatal(err)
	}
	names, auth, err := c.NamesIn(context.Background(), providers.KindArtist, []string{idA, idB, "bad"}, "ja")
	if err != nil || auth || names[idA] != "米津玄師" || len(names) != 1 {
		t.Fatalf("NamesIn = %v %v %v", names, auth, err)
	}
	if _, err := c.GetAlbum(context.Background(), idB); err != nil {
		t.Fatal(err)
	}
	if want := []string{"en", "ja", "en"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Accept-Language sent = %v, want %v", got, want)
	}
}

// An album's tracklist comes from the album request, then one request per
// further page; each track carries the album with its disc and track number,
// and the answer is cached.
func TestLookupReleaseTracksPagesAndCaches(t *testing.T) {
	item := func(id, name string, disc, n int) string {
		return `{"id":"` + id + `","name":"` + name + `","duration_ms":200000,"disc_number":` + string(rune('0'+disc)) +
			`,"track_number":` + string(rune('0'+n)) + `,"artists":[{"id":"art1","name":"Artist"}]}`
	}
	c, f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/albums/" + idB:
			w.Write([]byte(`{"id":"` + idB + `","name":"Blue","release_date":"2024-03-01","album_type":"album",
				"artists":[{"id":"art1","name":"Artist"}],
				"tracks":{"total":3,"items":[` + item("t1", "Intro", 1, 1) + `,` + item("t2", "Song", 1, 2) + `]}}`))
		case "/v1/albums/" + idB + "/tracks":
			if r.URL.Query().Get("offset") != "2" {
				t.Errorf("offset = %s", r.URL.Query().Get("offset"))
			}
			w.Write([]byte(`{"items":[` + item("t3", "Outro", 2, 1) + `]}`))
		default:
			w.WriteHeader(404)
		}
	})
	ts, err := c.LookupReleaseTracks(context.Background(), idB)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 3 || ts[1].ProviderID != "t2" || ts[1].Title != "Song" || ts[1].Artists[0].ProviderID != "art1" {
		t.Fatalf("%+v", ts)
	}
	r := ts[2].Releases[0]
	if r.ProviderID != idB || r.Title != "Blue" || r.Date != "2024-03-01" || r.Disc != 2 || r.Position != 1 || r.Artists[0].Name != "Artist" {
		t.Fatalf("release = %+v", r)
	}
	calls := atomic.LoadInt32(&f.apiCalls)
	if _, err := c.LookupReleaseTracks(context.Background(), idB); err != nil || atomic.LoadInt32(&f.apiCalls) != calls {
		t.Fatalf("not cached: %v", err)
	}
	if _, err := c.LookupReleaseTracks(context.Background(), idA); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("missing album: %v", err)
	}
}
