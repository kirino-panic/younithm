package match

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"younithm/internal/providers"
)

// ladderFake is a MusicBrainz-like provider that also reads release tracklists
// and offers the loose search. It records every request, in order.
type ladderFake struct {
	name       string                       // "" = MusicBrainz
	isrc       map[string][]providers.Track // ISRC lookup answers
	releases   map[string][]providers.Track // by release id; absent = not found
	releaseErr error
	search     func(providers.TrackQuery) []providers.Track
	loose      func(providers.TrackQuery) []providers.Track
	searchErr  error
	log        []string // "release:<id>", "search:<title>|<artist>|<album>", "loose:..."
}

func (f *ladderFake) Name() string {
	if f.name != "" {
		return f.name
	}
	return providers.MusicBrainz
}
func (f *ladderFake) LookupISRC(_ context.Context, isrc string) ([]providers.Track, error) {
	f.log = append(f.log, "isrc:"+isrc)
	return f.isrc[isrc], nil
}
func (f *ladderFake) GetTrack(context.Context, string) (*providers.Track, error) {
	return nil, providers.ErrUnsupported
}
func (f *ladderFake) GetArtwork(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}
func (f *ladderFake) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}
func (f *ladderFake) SearchTrack(_ context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	f.log = append(f.log, "search:"+q.Title+"|"+q.Artist+"|"+q.Album)
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.search == nil {
		return nil, nil
	}
	return f.search(q), nil
}
func (f *ladderFake) SearchLoose(_ context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	f.log = append(f.log, "loose:"+q.Title+"|"+q.Artist+"|"+q.Album)
	if f.loose == nil {
		return nil, nil
	}
	return f.loose(q), nil
}
func (f *ladderFake) LookupReleaseTracks(_ context.Context, id string) ([]providers.Track, error) {
	f.log = append(f.log, "release:"+id)
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	ts, ok := f.releases[id]
	if !ok {
		return nil, providers.ErrNotFound
	}
	return ts, nil
}

// plainFake is a provider without the optional capabilities (no release lookup,
// no loose search): it only searches, and records the queries.
type plainFake struct{ log []string }

func (p *plainFake) Name() string { return providers.Spotify }
func (p *plainFake) GetTrack(context.Context, string) (*providers.Track, error) {
	return nil, providers.ErrUnsupported
}
func (p *plainFake) GetArtwork(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}
func (p *plainFake) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}
func (p *plainFake) SearchTrack(_ context.Context, q providers.TrackQuery) ([]providers.Track, error) {
	p.log = append(p.log, "search:"+q.Title+"|"+q.Artist+"|"+q.Album)
	return nil, nil
}

// onRelease is a MusicBrainz track of a release's tracklist.
func onRelease(id, title, artist, release string) providers.Track {
	return providers.Track{
		Provider: providers.MusicBrainz, ProviderID: id, Title: title,
		Artists:  []providers.Artist{{Name: artist}},
		Releases: []providers.ReleaseRef{{ProviderID: release, Title: "Orange"}},
	}
}

func ladderRun(t *testing.T, p providers.Provider, l Listen) (Result, error) {
	t.Helper()
	return New(providers.NewRegistry(p), Config{}).Match(context.Background(), l)
}

// The release the listen named holds the track: it is read off the tracklist,
// with no search at all, even though the listen's title carries a feature credit
// the catalogue leaves out.
func TestTheNamedReleaseTracklistLinksTheTrackWithoutSearching(t *testing.T) {
	f := &ladderFake{releases: map[string][]providers.Track{"r1": {
		onRelease("t1", "Other Song", "Kikuo", "r1"),
		onRelease("t2", "Moon Youkai", "Kikuo", "r1"),
		onRelease("t3", "Third Song", "Kikuo", "r1"),
	}}}
	// Title and artist as the listen wrote them.
	r, err := ladderRun(t, f, Listen{Title: "Moon Youkai (feat. Hatsune Miku)", Artist: "Kikuo", Album: "Orange", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r1"}}})
	if err != nil {
		t.Fatal(err)
	}
	best, ok := r.Best()
	if !ok || best.ProviderID != "t2" || !best.Corroborated || best.Method != MethodFuzzy {
		t.Fatalf("best = %+v ok=%v, links %+v", best, ok, r.Links)
	}
	if want := []string{"release:r1"}; !reflect.DeepEqual(f.log, want) {
		t.Fatalf("requests = %v, want %v", f.log, want)
	}
}

// Two tracks on the release read like the listen: the release does not decide,
// so both stay candidates, and the search is not run on top of them.
func TestTwoMatchingTracksOnTheReleaseStayCandidates(t *testing.T) {
	f := &ladderFake{releases: map[string][]providers.Track{"r1": {
		onRelease("t1", "Hey Nineteen", "Steely Dan", "r1"),
		onRelease("t2", "Hey Nineteen", "Steely Dan", "r1"),
	}}}
	r, _ := ladderRun(t, f, Listen{Title: "Hey Nineteen", Artist: "Steely Dan", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r1"}}})
	if _, ok := r.Best(); ok || len(r.Links) != 2 {
		t.Fatalf("links = %+v", r.Links)
	}
	for _, l := range r.Links {
		if l.Corroborated {
			t.Fatalf("an ambiguous track was corroborated: %+v", l)
		}
	}
	if len(f.log) != 1 {
		t.Fatalf("requests = %v", f.log)
	}
}

// A release that has no track like the listen's (a romanised title, a track the
// edition lacks) tells nothing: the search runs as it always did.
func TestATracklistWithoutTheTrackFallsThroughToTheSearch(t *testing.T) {
	f := &ladderFake{
		releases: map[string][]providers.Track{"r1": {onRelease("t1", "Tonakai", "Fishmans", "r1")}},
		search: func(q providers.TrackQuery) []providers.Track {
			return []providers.Track{onRelease("found", "Kibun", "Fishmans", "r9")}
		},
	}
	r, _ := ladderRun(t, f, Listen{Title: "Kibun", Artist: "Fishmans", Album: "Orange", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r1"}}})
	if best, ok := r.Best(); !ok || best.ProviderID != "found" {
		t.Fatalf("links = %+v", r.Links)
	}
	if want := []string{"release:r1", "search:Kibun|Fishmans|Orange"}; !reflect.DeepEqual(f.log, want) {
		t.Fatalf("requests = %v", f.log)
	}
}

// A release id that does not exist is no answer; a rate limit is, and aborts.
func TestADeadReleaseIDFallsThroughAndARateLimitAborts(t *testing.T) {
	f := &ladderFake{search: func(providers.TrackQuery) []providers.Track {
		return []providers.Track{onRelease("found", "Kibun", "Fishmans", "r9")}
	}}
	r, _ := ladderRun(t, f, Listen{Title: "Kibun", Artist: "Fishmans", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"gone"}}})
	if best, ok := r.Best(); !ok || best.ProviderID != "found" {
		t.Fatalf("links = %+v", r.Links)
	}
	limited := &ladderFake{releaseErr: &providers.RateLimitedError{Provider: providers.MusicBrainz, RetryAfter: 5}}
	_, err := ladderRun(t, limited, Listen{Title: "Kibun", Artist: "Fishmans", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r1"}}})
	var rl *providers.RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want rate limited", err)
	}
	if len(limited.log) != 1 {
		t.Fatalf("a rate limit did not stop the requests: %v", limited.log)
	}
}

// Each distinct release is read once, and at most maxReleaseLookups of them.
func TestReleaseLookupsAreDistinctAndCapped(t *testing.T) {
	f := &ladderFake{}
	ladderRun(t, f, Listen{Title: "x", Artist: "y", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"a", "A", "b", "c", "d", "e"}}})
	var releases []string
	for _, e := range f.log {
		if len(e) > 8 && e[:8] == "release:" {
			releases = append(releases, e)
		}
	}
	if want := []string{"release:a", "release:b", "release:c"}; !reflect.DeepEqual(releases, want) {
		t.Fatalf("release lookups = %v, want %v", releases, want)
	}
}

// The first search that finds something worth a candidate ends the ladder: a
// track found by the second query costs two requests, never five.
func TestTheLadderStopsAtTheFirstQueryThatFindsSomething(t *testing.T) {
	f := &ladderFake{search: func(q providers.TrackQuery) []providers.Track {
		if q.Album != "" {
			return nil
		}
		return []providers.Track{onRelease("found", "The In Crowd", "The Mamas & the Papas", "r9")}
	}}
	r, _ := ladderRun(t, f, Listen{Title: "The In Crowd", Artist: "The Mamas & the Papas", Album: "A Different Album Name"})
	if best, ok := r.Best(); !ok || best.ProviderID != "found" {
		t.Fatalf("links = %+v", r.Links)
	}
	want := []string{
		"search:The In Crowd|The Mamas & the Papas|A Different Album Name",
		"search:The In Crowd|The Mamas & the Papas|",
	}
	if !reflect.DeepEqual(f.log, want) {
		t.Fatalf("requests = %v, want %v", f.log, want)
	}
}

// When nothing is found the ladder runs to the end, in order: the title loses
// its credit tag and the album, then the word searches.
func TestTheLadderRunsEveryStepWhenNothingIsFound(t *testing.T) {
	f := &ladderFake{}
	r, err := ladderRun(t, f, Listen{Title: "Moon Youkai (feat. Hatsune Miku)", Artist: "Kikuo", Album: "Orange"})
	if err != nil || len(r.Links) != 0 {
		t.Fatalf("links = %+v err = %v", r.Links, err)
	}
	want := []string{
		"search:Moon Youkai (feat. Hatsune Miku)|Kikuo|Orange",
		"search:Moon Youkai|Kikuo|",
		"loose:|Kikuo|Orange",
		"loose:Moon Youkai||Orange",
		"loose:Moon Youkai|Kikuo|",
	}
	if !reflect.DeepEqual(f.log, want) {
		t.Fatalf("requests = %v\nwant       %v", f.log, want)
	}
}

// A listen with no album and a plain title has nothing to loosen in the phrase
// search, so it is still one request there (plus the word search for the rest).
func TestAPlainListenWithoutAnAlbumRepeatsNoQuery(t *testing.T) {
	f := &ladderFake{}
	ladderRun(t, f, Listen{Title: "Kibun", Artist: "Fishmans"})
	want := []string{"search:Kibun|Fishmans|", "loose:Kibun|Fishmans|"}
	if !reflect.DeepEqual(f.log, want) {
		t.Fatalf("requests = %v, want %v", f.log, want)
	}
}

// A provider without the loose search is searched exactly as before: the exact
// query, and the one without the album.
func TestAProviderWithoutLooseSearchGetsOnlyThePhraseQueries(t *testing.T) {
	f := &plainFake{}
	ladderRun(t, f, Listen{Title: "Kibun (feat. X)", Artist: "Fishmans", Album: "Orange", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r1"}}})
	want := []string{"search:Kibun (feat. X)|Fishmans|Orange", "search:Kibun|Fishmans|"}
	if !reflect.DeepEqual(f.log, want) {
		t.Fatalf("requests = %v, want %v", f.log, want)
	}
}

// A rate limit in a later step aborts the match with that error.
func TestARateLimitInTheLadderAborts(t *testing.T) {
	f := &ladderFake{searchErr: &providers.RateLimitedError{Provider: providers.MusicBrainz, RetryAfter: 5}}
	_, err := ladderRun(t, f, Listen{Title: "Kibun", Artist: "Fishmans", Album: "Orange"})
	var rl *providers.RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v", err)
	}
	if len(f.log) != 1 {
		t.Fatalf("requests after the rate limit: %v", f.log)
	}
}

// Looser queries find noisier things, but they are scored like any other: a
// track whose title and artist do not read like the listen's is not a match.
func TestALooseResultThatDoesNotReadLikeTheListenIsNotLinked(t *testing.T) {
	f := &ladderFake{loose: func(providers.TrackQuery) []providers.Track {
		return []providers.Track{onRelease("noise", "Completely Different", "Somebody Else", "r1")}
	}}
	r, _ := ladderRun(t, f, Listen{Title: "Kibun", Artist: "Fishmans", Album: "Orange"})
	if len(r.Links) != 0 {
		t.Fatalf("noise was linked: %+v", r.Links)
	}
}

// A listen found only by a looser query is a fuzzy link like any other: it needs
// the same score, and the same rules decide whether it is corroborated (here the
// only track that reads like the listen reads exactly like it, so it is lone and
// exact; with a near rival it would be a plain fuzzy auto link).
func TestALooseFindFollowsTheOrdinaryFuzzyRules(t *testing.T) {
	loose := func(rival providers.Track) *ladderFake {
		return &ladderFake{loose: func(q providers.TrackQuery) []providers.Track {
			if q.Title == "" { // artist + album only
				return []providers.Track{onRelease("a", "Kibun", "Fishmans", "r1"), rival}
			}
			return nil
		}}
	}
	l := Listen{Title: "Kibun", Artist: "Fishmans", Album: "Orange"}

	r, _ := ladderRun(t, loose(onRelease("b", "Yoru no Omoi", "Fishmans", "r1")), l)
	best, ok := r.Best()
	if !ok || best.ProviderID != "a" || best.Method != MethodFuzzy || !best.Corroborated {
		t.Fatalf("lone exact find: %+v", r.Links)
	}

	r, _ = ladderRun(t, loose(onRelease("b", "Kibun", "Fishmann", "r1")), l)
	if best, ok := r.Best(); ok && best.Corroborated {
		t.Fatalf("a find with a near rival was called corroborated: %+v", r.Links)
	}
}

func TestMergeTracksKeepsEveryReleaseOfARecordingFoundTwice(t *testing.T) {
	a := onRelease("T1", "x", "y", "r1")
	b := onRelease("t1", "x", "y", "r2") // same recording, id in other case
	b.DurationMs = 1000
	c := onRelease("t2", "z", "y", "r1")
	got := mergeTracks([]providers.Track{a}, []providers.Track{b, c})
	if len(got) != 2 || len(got[0].Releases) != 2 || got[0].DurationMs != 1000 {
		t.Fatalf("merged = %+v", got)
	}
	// Merging again changes nothing.
	again := mergeTracks(got, []providers.Track{b})
	if len(again) != 2 || len(again[0].Releases) != 2 {
		t.Fatalf("remerged = %+v", again)
	}
}

func TestSearchTitleDropsOnlyTheCreditAndRemasterTags(t *testing.T) {
	for in, want := range map[string]string{
		"Moon Youkai (feat. Hatsune Miku)":     "Moon Youkai",
		"Beggin' (Feat. C Dot Castro)":         "Beggin'",
		"Song [feat. X & Y]":                   "Song",
		"Song - 2011 Remaster":                 "Song",
		"antinomy (nyankobrq Remix) (feat. X)": "antinomy (nyankobrq Remix)",
		"Plain Title":                          "Plain Title",
	} {
		if got := SearchTitle(in); got != want {
			t.Errorf("SearchTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithIsACreditOnlyInBrackets(t *testing.T) {
	for in, want := range map[string]string{
		"With or Without You":                    "with or without you",
		"Dancing With a Stranger":                "dancing with a stranger",
		"Dancing With a Stranger (with Normani)": "dancing with a stranger",
		"Song [With X]":                          "song",
		"Song feat. X":                           "song",
	} {
		if got := NormTitle(in); got != want {
			t.Errorf("NormTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SearchTitle("With or Without You"); got != "With or Without You" {
		t.Errorf("SearchTitle = %q", got)
	}
	if got := NormArtist("Florence with the Machine"); got != "florence with the machine" {
		t.Errorf("NormArtist = %q", got)
	}
}

func TestFoldUnifiesWidths(t *testing.T) {
	for a, b := range map[string]string{"ＦＵＮ": "FUN", "ｶﾞｰﾙ": "ガール", "A＆B": "A & B"} {
		if NormTitle(a) != NormTitle(b) {
			t.Errorf("NormTitle(%q) = %q, NormTitle(%q) = %q", a, NormTitle(a), b, NormTitle(b))
		}
	}
}

// spotifyOn is a Spotify track on one album.
func spotifyOn(id, title, album string) providers.Track {
	return providers.Track{Provider: providers.Spotify, ProviderID: id, Title: title,
		Artists:  []providers.Artist{{Name: "Fishmans"}},
		Releases: []providers.ReleaseRef{{ProviderID: album, Title: "Orange"}}}
}

// Album ids belong to a catalogue: a Spotify album's tracklist places a
// Spotify track, by Spotify's own album id.
func TestASpotifyAlbumTracklistLinksTheTrack(t *testing.T) {
	f := &ladderFake{name: providers.Spotify, releases: map[string][]providers.Track{"sp-al": {
		spotifyOn("s1", "Intro", "sp-al"), spotifyOn("s2", "Kibun", "sp-al"),
	}}}
	r, err := ladderRun(t, f, Listen{Title: "Kibun (feat. X)", Artist: "Fishmans", ReleaseIDs: map[string][]string{
		providers.MusicBrainz: {"mb-rel"}, providers.Spotify: {"sp-al"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := r.Best(); !ok || b.ProviderID != "s2" || !b.Corroborated {
		t.Fatalf("links = %+v", r.Links)
	}
	if len(f.log) == 0 || f.log[0] != "release:sp-al" {
		t.Fatalf("read %v, want only the Spotify album", f.log)
	}
}

// A lone ISRC hit on another edition does not settle it: the album's
// tracklist is read, and its track wins; the ISRC hit stays a candidate.
// With no copy on the album, the ISRC hit stands.
func TestTheAlbumsTrackBeatsAnISRCHitOnAnotherEdition(t *testing.T) {
	f := &ladderFake{name: providers.Spotify,
		isrc:     map[string][]providers.Track{"ISRC1": {spotifyOn("s-deluxe", "Kibun", "sp-deluxe")}},
		releases: map[string][]providers.Track{"sp-al": {spotifyOn("s1", "Intro", "sp-al"), spotifyOn("s2", "Kibun", "sp-al")}},
	}
	l := Listen{Title: "Kibun", Artist: "Fishmans", ISRC: "ISRC1", ReleaseIDs: map[string][]string{providers.Spotify: {"sp-al"}}}
	r, _ := ladderRun(t, f, l)
	var auto []string
	for _, k := range r.Links {
		if k.Status == StatusAuto {
			auto = append(auto, k.ProviderID)
		}
	}
	if len(auto) != 1 || auto[0] != "s2" {
		t.Fatalf("auto = %v, links = %+v", auto, r.Links)
	}
	f.releases = map[string][]providers.Track{"sp-al": {spotifyOn("s1", "Intro", "sp-al")}}
	if b, ok := mustRun(t, f, l).Best(); !ok || b.ProviderID != "s-deluxe" {
		t.Fatalf("without a copy on the album the ISRC hit should stand: %+v", b)
	}
	// On the album itself, the ISRC hit is the answer with no further request.
	f.log = nil
	l.ReleaseIDs = map[string][]string{providers.Spotify: {"sp-deluxe"}}
	if b, ok := mustRun(t, f, l).Best(); !ok || b.ProviderID != "s-deluxe" || len(f.log) != 1 {
		t.Fatalf("best %+v, requests %v", b, f.log)
	}
}

func mustRun(t *testing.T, p providers.Provider, l Listen) Result {
	t.Helper()
	r, err := ladderRun(t, p, l)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
