package apple

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"younithm/internal/providers"
)

func newKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

const songJSON = `{"id":"1001","attributes":{"name":"Great Song","artistName":"The Band","albumName":"Album",
"durationInMillis":201000,"isrc":"usabc0100001","releaseDate":"2001-02-03","trackNumber":3,"discNumber":1},
"relationships":{"albums":{"data":[{"id":"2002"}]},"artists":{"data":[{"id":"3003","attributes":{"name":"The Band"}}]}}}`

type env struct {
	c     *Client
	key   *ecdsa.PrivateKey
	calls int32
}

func setup(t *testing.T, api http.HandlerFunc) *env {
	t.Helper()
	key, pemText := newKey(t)
	e := &env{key: key}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&e.calls, 1)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		hdr, claims, err := VerifyToken(tok, &key.PublicKey)
		if err != nil || hdr["kid"] != "KEY123" || hdr["alg"] != "ES256" || claims["iss"] != "TEAM456" {
			w.WriteHeader(401)
			return
		}
		api(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Credentials: func(context.Context) (string, string, string, error) {
		return "TEAM456", "KEY123", pemText, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.c = c
	return e
}

func TestTokenSignsAndVerifies(t *testing.T) {
	k, _ := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	tok, err := SignToken("T", "K", k, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h, c, err := VerifyToken(tok, &k.PublicKey)
	if err != nil || h["kid"] != "K" || c["iss"] != "T" || int64(c["exp"].(float64)) != now.Add(time.Hour).Unix() {
		t.Fatalf("%v %v %v", h, c, err)
	}
	other, _ := newKey(t)
	if _, _, err := VerifyToken(tok, &other.PublicKey); err == nil {
		t.Fatal("verified with the wrong key")
	}
}

func TestParsePrivateKeyAcceptsPastedForms(t *testing.T) {
	_, pemText := newKey(t)
	body := strings.Join(strings.Split(strings.TrimSpace(pemText), "\n")[1:4], "") // bare base64 body
	forms := map[string]string{
		"pem":            pemText,
		"escaped":        strings.ReplaceAll(strings.TrimSpace(pemText), "\n", `\n`),
		"bare":           body,
		"bare multiline": strings.Join(strings.Split(strings.TrimSpace(pemText), "\n")[1:4], "\n"),
	}
	for name, s := range forms {
		if _, err := ParsePrivateKey(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParsePrivateKey("garbage"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestSearchMapsSongWithRelationships(t *testing.T) {
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/catalog/us/search" || q.Get("types") != "songs" || q.Get("term") != "The Band Great Song" || q.Get("include") != "albums,artists" {
			t.Errorf("unexpected %s", r.URL)
		}
		w.Write([]byte(`{"results":{"songs":{"data":[` + songJSON + `]}}}`))
	})
	got, err := e.c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "The Band", Title: "Great Song"})
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	tr := got[0]
	if tr.Provider != "apple" || tr.ProviderID != "1001" || tr.Artists[0].ProviderID != "3003" ||
		tr.Releases[0].ProviderID != "2002" || tr.Releases[0].Position != 3 || tr.ISRCs[0] != "USABC0100001" {
		t.Fatalf("%+v", tr)
	}
}

func TestTokenCachedAcrossCallsAndReissuedWhenStale(t *testing.T) {
	e := setup(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":[` + songJSON + `]}`)) })
	now := time.Unix(1_800_000_000, 0)
	e.c.cfg.Now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if _, err := e.c.GetTrack(context.Background(), "1001"); err != nil {
			t.Fatal(err)
		}
	}
	first := e.c.token
	now = now.Add(tokenTTL - 30*time.Minute) // inside the refresh margin
	if _, err := e.c.GetTrack(context.Background(), "1001"); err != nil {
		t.Fatal(err)
	}
	if e.c.token == first {
		t.Fatal("token not reissued near expiry")
	}
}

func TestLookupISRCAndArtwork(t *testing.T) {
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/catalog/us/songs" && r.URL.Query().Get("filter[isrc]") == "USABC0100001":
			w.Write([]byte(`{"data":[` + songJSON + `,` + strings.Replace(songJSON, `"1001"`, `"1002"`, 1) + `]}`))
		case strings.HasPrefix(r.URL.Path, "/catalog/us/albums/"):
			w.Write([]byte(`{"data":[{"id":"2002","attributes":{"artwork":{"url":"http://i/{w}x{h}bb.jpg","width":800,"height":800}}}]}`))
		default:
			w.WriteHeader(404)
		}
	})
	got, err := e.c.LookupISRC(context.Background(), "US-ABC-01-00001")
	if err != nil || len(got) != 1 || got[0].ProviderID != "1001" {
		t.Fatalf("%+v %v", got, err)
	}
	art, err := e.c.GetArtwork(context.Background(), "2002")
	if err != nil || len(art) != 3 || art[0].Width != 800 || art[0].URL != "http://i/800x800bb.jpg" || art[2].Width != 100 {
		t.Fatalf("%+v %v", art, err)
	}
	if _, err := e.c.GetTrack(context.Background(), "999"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
}

func TestUnconfiguredBadKeyAndRejectedToken(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatal(err)
	}
	mk := func(team, kid, pk string) *Client {
		c, _ := New(Config{Credentials: func(context.Context) (string, string, string, error) { return team, kid, pk, nil }})
		return c
	}
	q := providers.TrackQuery{Artist: "a", Title: "b"}
	if _, err := mk("", "", "").SearchTrack(context.Background(), q); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := mk("T", "K", "not a key").SearchTrack(context.Background(), q); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("bad key: %v", err)
	}
	// The server rejects every token: one re-sign, then not-configured.
	e := setup(t, func(http.ResponseWriter, *http.Request) {})
	e.c.cfg.Credentials = func(context.Context) (string, string, string, error) {
		_, other := newKey(t)
		return "TEAM456", "KEY123", other, nil // valid key, but not the one the server trusts
	}
	if _, err := e.c.SearchTrack(context.Background(), q); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("rejected: %v", err)
	}
	if e.calls != 2 {
		t.Fatalf("calls %d, want one retry", e.calls)
	}
}

func TestRateLimited(t *testing.T) {
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	_, err := e.c.GetTrack(context.Background(), "1001")
	var rl *providers.RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 30 || rl.Provider != "apple" {
		t.Fatalf("%v", err)
	}
}

func TestGetAlbumReturnsBarcodeDateAndTrackCount(t *testing.T) {
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog/us/albums/2002" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"2002","attributes":{"name":"Power Move","artistName":"Cheem",
			"releaseDate":"2025-11-14","trackCount":11,"upc":"199891213535"}}]}`))
	})
	a, err := e.c.GetAlbum(context.Background(), "2002")
	if err != nil {
		t.Fatal(err)
	}
	if a.ProviderID != "2002" || a.UPC != "199891213535" || a.Date != "2025-11-14" || a.TotalTracks != 11 || a.Title != "Power Move" {
		t.Fatalf("%+v", a)
	}
	for _, bad := range []string{"", "abc", "20 02"} {
		if _, err := e.c.GetAlbum(context.Background(), bad); !errors.Is(err, providers.ErrNotFound) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestGetAlbumNotFound(t *testing.T) {
	e := setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	if _, err := e.c.GetAlbum(context.Background(), "999"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	e2 := setup(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":[]}`)) })
	if _, err := e2.c.GetAlbum(context.Background(), "999"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("empty data: %v", err)
	}
}
