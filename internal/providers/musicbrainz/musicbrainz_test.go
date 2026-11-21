package musicbrainz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"younithm/internal/config"
	"younithm/internal/providers"
)

const recJSON = `{"id":"11111111-1111-1111-1111-111111111111","score":100,"title":"Song","length":201000,
"artist-credit":[{"name":"A","joinphrase":"","artist":{"id":"22222222-2222-2222-2222-222222222222","name":"Artist","sort-name":"Artist"}}],
"releases":[{"id":"33333333-3333-3333-3333-333333333333","title":"Album","status":"Official","date":"2001-02-03","release-group":{"primary-type":"Album"}}],
"isrcs":["USABC0100001"]}`

func newTest(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	lim := providers.NewLimiter(time.Second)
	lim.SetClock(time.Now, func(context.Context, time.Duration) error { return nil })
	c, err := New(Config{Contact: "me@example.com", AppVersion: "1.2", BaseURL: srv.URL, CoverURL: srv.URL, Limiter: lim})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestContactRequired(t *testing.T) {
	_, err := New(Config{})
	if !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestSearchSendsUserAgentAndParses(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "younithm/1.2 ( me@example.com )" {
			t.Errorf("UA = %q", got)
		}
		if r.URL.Path != "/recording" || r.URL.Query().Get("fmt") != "json" {
			t.Errorf("url = %s", r.URL)
		}
		if q := r.URL.Query().Get("query"); !strings.Contains(q, `recording:"Song \"X\""`) {
			t.Errorf("query = %s", q)
		}
		w.Write([]byte(`{"recordings":[` + recJSON + `]}`))
	})
	tr, err := c.SearchTrack(context.Background(), providers.TrackQuery{Artist: "Artist", Title: `Song "X"`})
	if err != nil || len(tr) != 1 {
		t.Fatalf("%v %v", tr, err)
	}
	g := tr[0]
	if g.ProviderID != "11111111-1111-1111-1111-111111111111" || g.DurationMs != 201000 || g.Score != 100 ||
		g.Artists[0].Name != "Artist" || g.Releases[0].Type != "album" || g.Releases[0].Status != "official" || g.ISRCs[0] != "USABC0100001" {
		t.Fatalf("bad track %+v", g)
	}
}

func TestISRCLookupAndNotFound(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/isrc/USABC0100001" {
			w.Write([]byte(`{"isrc":"USABC0100001","recordings":[` + recJSON + `]}`))
			return
		}
		http.NotFound(w, r)
	})
	tr, err := c.LookupISRC(context.Background(), "us-abc-01-00001")
	if err != nil || len(tr) != 1 {
		t.Fatalf("%v %v", tr, err)
	}
	tr, err = c.LookupISRC(context.Background(), "USZZZ0100001")
	if err != nil || len(tr) != 0 {
		t.Fatalf("want empty, got %v %v", tr, err)
	}
	if _, err = c.GetTrack(context.Background(), "33333333-3333-3333-3333-333333333333"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err = c.GetTrack(context.Background(), "../evil"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("invalid id err = %v", err)
	}
}

func TestRateLimited503(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(503)
	})
	_, err := c.GetTrack(context.Background(), "11111111-1111-1111-1111-111111111111")
	var rl *providers.RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 7 {
		t.Fatalf("err = %v", err)
	}
}

func TestArtworkFrontFirst(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"images":[{"back":true,"image":"b"},{"front":true,"image":"f"}]}`))
	})
	imgs, err := c.GetArtwork(context.Background(), "33333333-3333-3333-3333-333333333333")
	if err != nil || len(imgs) != 2 || imgs[0].URL != "f" || imgs[1].Kind != "back" {
		t.Fatalf("%v %v", imgs, err)
	}
	if _, err := c.GetArtistImage(context.Background(), "x"); !errors.Is(err, providers.ErrUnsupported) {
		t.Fatal("want unsupported")
	}
}

func TestNewFromConfigContact(t *testing.T) {
	if _, err := NewFromConfig(config.Config{}, Config{}); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	c, err := NewFromConfig(config.Config{MusicBrainzContact: "me@example.com"}, Config{AppVersion: "1.2"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "younithm/1.2 ( me@example.com )"; c.UserAgent() != want {
		t.Fatalf("UA %q want %q", c.UserAgent(), want)
	}
	c, _ = NewFromConfig(config.Config{MusicBrainzContact: "env@example.com"}, Config{Contact: "explicit@example.com"})
	if !strings.Contains(c.UserAgent(), "explicit@") {
		t.Fatal(c.UserAgent())
	}
}

func TestReleaseArtistCreditsAreAlbumArtists(t *testing.T) {
	// A compilation: the album artist differs from the recording's artist.
	const compJSON = `{"id":"11111111-1111-1111-1111-111111111111","title":"Song","length":1000,
"artist-credit":[{"name":"A","joinphrase":"","artist":{"id":"22222222-2222-2222-2222-222222222222","name":"A"}}],
"releases":[{"id":"33333333-3333-3333-3333-333333333333","title":"Now 100","date":"2001",
  "release-group":{"id":"44444444-4444-4444-4444-444444444444","primary-type":"Album"},
  "artist-credit":[{"name":"Various Artists","joinphrase":"","artist":{"id":"89ad4ac3-39f7-470e-963a-56509c546377","name":"Various Artists"}}]}]}`
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(compJSON)) })
	tr, err := c.GetTrack(context.Background(), "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	rel := tr.Releases[0]
	if len(rel.Artists) != 1 || rel.Artists[0].Name != "Various Artists" || rel.Artists[0].ProviderID != "89ad4ac3-39f7-470e-963a-56509c546377" {
		t.Fatalf("album artists: %+v", rel.Artists)
	}
	if rel.GroupID != "44444444-4444-4444-4444-444444444444" || tr.Artists[0].Name != "A" {
		t.Fatalf("group/track artist: %+v %+v", rel, tr.Artists)
	}
}

const (
	relA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	relB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func TestBarcodeForms(t *testing.T) {
	for in, want := range map[string][]string{
		"00602468077992": {"602468077992", "0602468077992"}, // Spotify pads to 14 digits
		"199891213535":   {"199891213535", "0199891213535"},
		"0602468077992":  {"602468077992", "0602468077992"},
		"4547366570861":  {"4547366570861"},
		"123":            nil,
		"":               nil,
	} {
		got := BarcodeForms(in)
		if len(got) != len(want) {
			t.Fatalf("%q: %v want %v", in, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%q: %v want %v", in, got, want)
			}
		}
	}
}

func TestLookupBarcodeReturnsTracklistsOfMatchingReleasesOnly(t *testing.T) {
	var searches, lookups int
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/release":
			searches++
			if q := r.URL.Query().Get("query"); !strings.Contains(q, "barcode:(") || !strings.Contains(q, "199891213535") || !strings.Contains(q, "0199891213535") {
				t.Errorf("query = %q", q)
			}
			// One real hit, one the search engine returned but whose barcode differs.
			w.Write([]byte(`{"releases":[{"id":"` + relA + `","barcode":"199891213535"},{"id":"` + relB + `","barcode":"199891219999"}]}`))
		case r.URL.Path == "/release/"+relA:
			lookups++
			if r.URL.Query().Get("inc") != "recordings isrcs" { // "+" is a space once decoded
				t.Errorf("inc = %q", r.URL.Query().Get("inc"))
			}
			w.Write([]byte(`{"id":"` + relA + `","title":"Power Move","date":"2025-11-14","barcode":"0199891213535",
				"media":[{"position":1,"tracks":[
				 {"position":1,"title":"One","length":100000,"recording":{"id":"r1","title":"One","length":100010,"isrcs":["USAAA1111111"]}},
				 {"position":2,"title":"Gorilla Glue","length":184000,"recording":{"id":"07027248-cc3a-4df0-b5b6-43f11af43ea9","title":"Gorilla Glue","length":0}}]}]}`))
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	})
	rels, err := c.LookupBarcode(context.Background(), "199891213535")
	if err != nil {
		t.Fatal(err)
	}
	if searches != 1 || lookups != 1 || len(rels) != 1 {
		t.Fatalf("searches=%d lookups=%d releases=%d", searches, lookups, len(rels))
	}
	r := rels[0]
	if r.ProviderID != relA || r.Date != "2025-11-14" || len(r.Tracks) != 2 {
		t.Fatalf("%+v", r)
	}
	t1, t2 := r.Tracks[0], r.Tracks[1]
	if t1.Disc != 1 || t1.Position != 1 || t1.DurationMs != 100010 || len(t1.ISRCs) != 1 {
		t.Fatalf("%+v", t1)
	}
	if t2.Position != 2 || t2.RecordingID != "07027248-cc3a-4df0-b5b6-43f11af43ea9" || t2.DurationMs != 184000 {
		t.Fatalf("length falls back to the track's own: %+v", t2) // recording length 0
	}
	// No barcode, no request.
	searches = 0
	if got, err := c.LookupBarcode(context.Background(), "n/a"); err != nil || got != nil || searches != 0 {
		t.Fatalf("%v %v %d", got, err, searches)
	}
}

const trackID = "aaaaaaaa-1111-2222-3333-444444444444"

func TestLookupTrackIDResolvesToTheRecording(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/recording" || r.URL.Query().Get("query") != "tid:"+trackID {
			t.Errorf("url = %s", r.URL)
		}
		w.Write([]byte(`{"recordings":[` + recJSON + `]}`))
	})
	tr, err := c.LookupTrackID(context.Background(), strings.ToUpper(trackID)) // case does not matter
	if err != nil || tr == nil || tr.ProviderID != "11111111-1111-1111-1111-111111111111" || len(tr.Releases) != 1 || tr.ISRCs[0] != "USABC0100001" {
		t.Fatalf("%+v %v", tr, err)
	}
}

func TestLookupTrackIDNoAnswerIsNotFound(t *testing.T) {
	for name, body := range map[string]string{
		"none": `{"recordings":[]}`,
		"two":  `{"recordings":[` + recJSON + `,` + recJSON + `]}`, // a track id names one recording: more is no answer
	} {
		c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
		if _, err := c.LookupTrackID(context.Background(), trackID); !errors.Is(err, providers.ErrNotFound) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// Anything that is not a UUID is never sent: it would go into a search query.
func TestLookupTrackIDRejectsNonIDsWithoutARequest(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("request sent: %s", r.URL) })
	for _, id := range []string{"", "not-an-id", trackID + " OR title:x", `x" OR *:*`} {
		if _, err := c.LookupTrackID(context.Background(), id); !errors.Is(err, providers.ErrNotFound) {
			t.Fatalf("%q: err = %v", id, err)
		}
	}
}

func TestCreditedNameKeptWhenItDiffers(t *testing.T) {
	got := credits([]mbArtistCredit{
		{Name: "Casiopea", JoinPhrase: " vs. ", Artist: struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			SortName string `json:"sort-name"`
		}{ID: "c", Name: "Casiopea"}},
		{Name: "The Square", Artist: struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			SortName string `json:"sort-name"`
		}{ID: "s", Name: "T-SQUARE"}},
	})
	if len(got) != 2 || got[0].CreditedName != "" || got[1].Name != "T-SQUARE" || got[1].CreditedName != "The Square" || got[0].JoinPhrase != " vs. " {
		t.Fatalf("%+v", got)
	}
}
