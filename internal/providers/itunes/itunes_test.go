package itunes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"younithm/internal/providers"
)

const song = `{"wrapperType":"track","kind":"song","trackId":111,"trackName":"Great Song","artistId":222,"artistName":"The Band",
"collectionId":333,"collectionName":"Album","trackTimeMillis":201000,"releaseDate":"2001-02-03T08:00:00Z",
"discNumber":1,"trackNumber":3,"artworkUrl100":"http://i/x/100x100bb.jpg"}`

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	lim := providers.NewLimiter(time.Second)
	lim.SetClock(time.Now, func(context.Context, time.Duration) error { return nil })
	return New(Config{BaseURL: srv.URL, Limiter: lim})
}

func TestSearchMapsSongsAndSkipsOtherKinds(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/search" || q.Get("term") != "The Band Great Song" || q.Get("entity") != "song" || q.Get("country") != "us" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write([]byte(`{"resultCount":2,"results":[` + song + `,{"wrapperType":"artist","artistId":9}]}`))
	})
	got, err := c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "The Band", Title: "Great Song"})
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	tr := got[0]
	if tr.Provider != "itunes" || tr.ProviderID != "111" || tr.DurationMs != 201000 || tr.Artists[0].Name != "The Band" ||
		tr.Releases[0].Title != "Album" || tr.Releases[0].Date != "2001-02-03" || tr.Releases[0].Position != 3 {
		t.Fatalf("%+v", tr)
	}
}

func TestLookupAndArtworkSizes(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") == "404" {
			w.Write([]byte(`{"resultCount":0,"results":[]}`))
			return
		}
		w.Write([]byte(`{"resultCount":1,"results":[` + song + `]}`))
	})
	tr, err := c.GetTrack(context.Background(), "111")
	if err != nil || tr.Title != "Great Song" {
		t.Fatalf("%+v %v", tr, err)
	}
	if _, err := c.GetTrack(context.Background(), "404"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := c.GetTrack(context.Background(), "abc"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
	art, err := c.GetArtwork(context.Background(), "333")
	if err != nil || len(art) != 3 || art[0].URL != "http://i/x/1000x1000bb.jpg" || art[0].Width != 1000 {
		t.Fatalf("%+v %v", art, err)
	}
	if _, err := c.GetArtistImage(context.Background(), "1"); !errors.Is(err, providers.ErrUnsupported) {
		t.Fatalf("artist image: %v", err)
	}
}

func TestThrottleIsRateLimited(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	_, err := c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "a", Title: "b"})
	var rl *providers.RateLimitedError
	if !errors.As(err, &rl) || rl.Provider != "itunes" {
		t.Fatalf("err %v", err)
	}
	if _, err := c.SearchTrack(context.Background(), providers.TrackQuery{}); err == nil {
		t.Fatal("empty query should fail")
	}
}
