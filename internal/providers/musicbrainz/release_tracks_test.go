package musicbrainz

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"younithm/internal/providers"
)

const tracklistJSON = `{"id":"33333333-3333-3333-3333-333333333333","title":"Orange","date":"2001-02-03","status":"Official",
"artist-credit":[{"name":"Album Artist","joinphrase":"","artist":{"id":"44444444-4444-4444-4444-444444444444","name":"Album Artist","sort-name":"Artist, Album"}}],
"release-group":{"id":"55555555-5555-5555-5555-555555555555","primary-type":"Album"},
"media":[
 {"position":1,"tracks":[
  {"position":1,"title":"Track One (release spelling)","length":100000,
   "recording":{"id":"11111111-1111-1111-1111-111111111111","title":"Track One","length":201000,
    "artist-credit":[{"name":"A","joinphrase":" feat. ","artist":{"id":"66666666-6666-6666-6666-666666666666","name":"Artist A","sort-name":"A"}},{"name":"B","joinphrase":"","artist":{"id":"77777777-7777-7777-7777-777777777777","name":"Artist B","sort-name":"B"}}],
    "isrcs":["USABC0100001"]}},
  {"position":2,"title":"Track Two","length":150000,
   "recording":{"id":"22222222-2222-2222-2222-222222222222","title":"Track Two","length":0}}
 ]},
 {"position":2,"tracks":[
  {"position":1,"title":"Data Track","length":1000,"recording":{"id":""}}
 ]}
]}`

func TestLookupReleaseTracksReturnsTheTracklistAsRecordings(t *testing.T) {
	var asked string
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path + "?" + r.URL.RawQuery
		w.Write([]byte(tracklistJSON))
	})
	tracks, err := c.LookupReleaseTracks(context.Background(), "33333333-3333-3333-3333-333333333333")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(asked, "/release/33333333-3333-3333-3333-333333333333?") ||
		!strings.Contains(asked, "inc=recordings+artist-credits+isrcs+release-groups") {
		t.Fatalf("asked %q", asked)
	}
	if len(tracks) != 2 { // the track with no recording is skipped
		t.Fatalf("tracks = %+v", tracks)
	}
	one, two := tracks[0], tracks[1]
	if one.Provider != providers.MusicBrainz || one.ProviderID != "11111111-1111-1111-1111-111111111111" ||
		one.Title != "Track One" || one.DurationMs != 201000 || len(one.ISRCs) != 1 {
		t.Fatalf("track one = %+v", one)
	}
	if len(one.Artists) != 2 || one.Artists[0].Name != "Artist A" || one.Artists[0].JoinPhrase != " feat. " {
		t.Fatalf("credits = %+v", one.Artists)
	}
	ref := one.Releases[0]
	if len(one.Releases) != 1 || ref.ProviderID != "33333333-3333-3333-3333-333333333333" || ref.Title != "Orange" ||
		ref.Date != "2001-02-03" || ref.Type != "album" || ref.Status != "official" || ref.GroupID == "" ||
		ref.Disc != 1 || ref.Position != 1 || len(ref.Artists) != 1 {
		t.Fatalf("release = %+v", ref)
	}
	// No recording credit and no length: the track's own length and the album's credit.
	if two.DurationMs != 150000 || len(two.Artists) != 1 || two.Artists[0].Name != "Album Artist" {
		t.Fatalf("track two = %+v", two)
	}
}

func TestReleaseTracklistsAreFetchedOnce(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.URL.Path, "99999999") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(tracklistJSON))
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tracks, err := c.LookupReleaseTracks(ctx, "33333333-3333-3333-3333-333333333333")
		if err != nil || len(tracks) != 2 {
			t.Fatalf("%v %v", tracks, err)
		}
		tracks[0].Title = "changed by the caller" // must not reach the cache
	}
	if tracks, _ := c.LookupReleaseTracks(ctx, "33333333-3333-3333-3333-333333333333"); tracks[0].Title != "Track One" {
		t.Fatalf("the cache hands out its own slice: %q", tracks[0].Title)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.LookupReleaseTracks(ctx, "99999999-9999-9999-9999-999999999999"); !errors.Is(err, providers.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("%d requests for two releases", hits.Load())
	}
}

func TestReleaseTracklistRejectsNonIDsWithoutAsking(t *testing.T) {
	c, _ := newTest(t, func(http.ResponseWriter, *http.Request) { t.Error("a request was made") })
	for _, id := range []string{"", "not-an-id", "../recording"} {
		if _, err := c.LookupReleaseTracks(context.Background(), id); !errors.Is(err, providers.ErrNotFound) {
			t.Errorf("%q: err = %v", id, err)
		}
	}
}

func TestReleaseTracklistRateLimitIsNotCached(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(tracklistJSON))
	})
	id := "33333333-3333-3333-3333-333333333333"
	_, err := c.LookupReleaseTracks(context.Background(), id)
	var rl *providers.RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v", err)
	}
	if tracks, err := c.LookupReleaseTracks(context.Background(), id); err != nil || len(tracks) != 2 {
		t.Fatalf("after the limit: %v %v", tracks, err)
	}
}

func TestBuildLooseQueryUsesWordsNotPhrases(t *testing.T) {
	got := BuildLooseQuery(providers.TrackQuery{
		Title: `Don't You Know`, Artist: "Snarky Puppy & Jacob Collier", Album: `Family Dinner, Volume "Two"`,
	})
	want := `recording:(Don't AND You AND Know) AND artist:(Snarky AND Puppy AND Jacob AND Collier) AND release:(Family AND Dinner AND Volume AND Two)`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if got := BuildLooseQuery(providers.TrackQuery{Artist: "Fishmans", Album: "Orange"}); got != `artist:(Fishmans) AND release:(Orange)` {
		t.Fatalf("no title: %s", got)
	}
	// Characters that mean something to Lucene never reach the query.
	if got := BuildLooseQuery(providers.TrackQuery{Title: `x) OR artist:(* "`}); got != `recording:(x AND OR AND artist)` {
		t.Fatalf("syntax leaked: %s", got)
	}
	if got := BuildLooseQuery(providers.TrackQuery{Title: "!!! ???"}); got != "" {
		t.Fatalf("punctuation only: %q", got)
	}
}

func TestSearchLooseSendsTheWordQuery(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/recording" || q.Get("query") != `artist:(Fishmans) AND release:(Orange)` || q.Get("limit") != "25" {
			t.Errorf("url = %s", r.URL)
		}
		w.Write([]byte(`{"recordings":[` + recJSON + `]}`))
	})
	tracks, err := c.SearchLoose(context.Background(), providers.TrackQuery{Artist: "Fishmans", Album: "Orange"})
	if err != nil || len(tracks) != 1 {
		t.Fatalf("%v %v", tracks, err)
	}
	if _, err := c.SearchLoose(context.Background(), providers.TrackQuery{}); err == nil {
		t.Fatal("an empty query was sent")
	}
}
