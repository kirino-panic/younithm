package match

import (
	"context"
	"errors"
	"testing"

	"younithm/internal/providers"
)

type fake struct {
	name   string
	isrc   map[string][]providers.Track
	search []providers.Track
	err    error
	calls  int
}

func (f *fake) Name() string { return f.name }
func (f *fake) SearchTrack(context.Context, providers.TrackQuery) ([]providers.Track, error) {
	f.calls++
	return f.search, f.err
}
func (f *fake) GetTrack(context.Context, string) (*providers.Track, error) {
	return nil, providers.ErrUnsupported
}
func (f *fake) GetArtwork(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}
func (f *fake) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}
func (f *fake) LookupISRC(_ context.Context, i string) ([]providers.Track, error) {
	f.calls++
	return f.isrc[i], nil
}

func tr(id, title, artist string, dur int, album string) providers.Track {
	t := providers.Track{Provider: "fake", ProviderID: id, Title: title, DurationMs: dur,
		Artists: []providers.Artist{{Name: artist}}}
	if album != "" {
		t.Releases = []providers.ReleaseRef{{Title: album}}
	}
	return t
}

func run(t *testing.T, f *fake, l Listen) Result {
	t.Helper()
	r, err := New(providers.NewRegistry(f), Config{}).Match(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSubmittedMBIDShortCircuits(t *testing.T) {
	f := &fake{name: "fake"}
	r := run(t, f, Listen{RecordingMBID: "abc", Title: "x", Artist: "y", ISRC: "Z"})
	if len(r.Links) != 1 || r.Links[0].Method != MethodSubmittedMBID || r.Links[0].Provider != providers.MusicBrainz || f.calls != 0 {
		t.Fatalf("%+v calls=%d", r, f.calls)
	}
}

func TestISRCSingleAuto(t *testing.T) {
	f := &fake{name: "fake", isrc: map[string][]providers.Track{"I": {tr("1", "Song", "Artist", 200000, "")}}}
	r := run(t, f, Listen{Title: "Song", Artist: "Artist", ISRC: "I"})
	b, ok := r.Best()
	if !ok || b.Method != MethodISRC || b.ProviderID != "1" || f.calls != 1 {
		t.Fatalf("%+v calls=%d", r, f.calls)
	}
}

func TestISRCSharedIsAmbiguous(t *testing.T) {
	f := &fake{name: "fake", isrc: map[string][]providers.Track{"I": {
		tr("1", "Song", "Artist", 0, ""), tr("2", "Song (Live)", "Artist", 0, "")}}}
	r := run(t, f, Listen{Title: "Song", Artist: "Artist", ISRC: "I"})
	if _, ok := r.Best(); ok {
		t.Fatalf("must not auto-pick: %+v", r)
	}
	if len(r.Links) < 2 {
		t.Fatalf("want candidates: %+v", r)
	}
}

func TestISRCTextDisagreesIsCandidate(t *testing.T) {
	f := &fake{name: "fake", isrc: map[string][]providers.Track{"I": {tr("1", "Totally Different", "Someone Else", 0, "")}}}
	r := run(t, f, Listen{Title: "Song", Artist: "Artist", ISRC: "I"})
	if len(r.Links) != 1 || r.Links[0].Status != StatusCandidate {
		t.Fatalf("%+v", r)
	}
}

// The same recording under another script or with other credits: the ISRC plus
// the same duration is enough, whatever the names look like.
func TestISRCWithMatchingDurationAutoLinksDespiteDifferentText(t *testing.T) {
	f := &fake{name: "fake", isrc: map[string][]providers.Track{"I": {tr("1", "花の塔", "さユり", 275614, "")}}}
	r := run(t, f, Listen{Title: "Tower of Flower", Artist: "Sayuri", DurationMs: 275960, ISRC: "I"})
	b, ok := r.Best()
	if !ok || b.Method != MethodISRC || b.ProviderID != "1" || b.Confidence < 0.85 {
		t.Fatalf("%+v", r)
	}
}

func TestISRCWithDifferentTextNeedsAMatchingDuration(t *testing.T) {
	hit := map[string][]providers.Track{"I": {tr("1", "花の塔", "さユり", 275614, "")}}
	for name, l := range map[string]Listen{
		"duration too far": {Title: "Tower of Flower", Artist: "Sayuri", DurationMs: 275614 + 20000, ISRC: "I"},
		"listen has none":  {Title: "Tower of Flower", Artist: "Sayuri", ISRC: "I"},
	} {
		r := run(t, &fake{name: "fake", isrc: hit}, l)
		if _, ok := r.Best(); ok || len(r.Links) != 1 || r.Links[0].Status != StatusCandidate || r.Links[0].Confidence != 0.4 {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	// The provider's own duration missing counts the same.
	noDur := map[string][]providers.Track{"I": {tr("1", "花の塔", "さユり", 0, "")}}
	if _, ok := run(t, &fake{name: "fake", isrc: noDur}, Listen{Title: "Tower of Flower", Artist: "Sayuri", DurationMs: 275614, ISRC: "I"}).Best(); ok {
		t.Fatal("auto-linked with no provider duration")
	}
}

func TestISRCSharedStaysAmbiguousEvenWithMatchingDurations(t *testing.T) {
	f := &fake{name: "fake", isrc: map[string][]providers.Track{"I": {
		tr("1", "花の塔", "さユり", 275614, ""), tr("2", "花の塔", "さユり", 275700, "")}}}
	r := run(t, f, Listen{Title: "Tower of Flower", Artist: "Sayuri", DurationMs: 275614, ISRC: "I"})
	if _, ok := r.Best(); ok || len(r.Links) != 2 {
		t.Fatalf("must not auto-pick between two hits: %+v", r)
	}
}

func TestFuzzyAuto(t *testing.T) {
	f := &fake{name: "fake", search: []providers.Track{
		tr("1", "Don't Stop Me Now (2011 Remaster)", "Queen", 209000, "Jazz"),
		tr("2", "Don't Stop Me Now (Live)", "Queen", 230000, "Live"),
	}}
	r := run(t, f, Listen{Title: "Dont Stop Me Now", Artist: "Queen feat. Someone", Album: "Jazz", DurationMs: 210000})
	b, ok := r.Best()
	if !ok || b.ProviderID != "1" || b.Method != MethodFuzzy || b.Confidence < 0.9 {
		t.Fatalf("%+v", r)
	}
}

func TestFuzzyAmbiguousBecomesCandidates(t *testing.T) {
	f := &fake{name: "fake", search: []providers.Track{
		tr("1", "Hello", "Adele", 0, ""), tr("2", "Hello", "Adele", 0, ""),
	}}
	r := run(t, f, Listen{Title: "Hello", Artist: "Adele"})
	if _, ok := r.Best(); ok || len(r.Links) != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestFuzzyLiveMarkerPenalised(t *testing.T) {
	f := &fake{name: "fake", search: []providers.Track{tr("2", "Song (Live)", "Artist", 0, "")}}
	r := run(t, f, Listen{Title: "Song", Artist: "Artist"})
	if _, ok := r.Best(); ok {
		t.Fatalf("live version must not auto-match studio listen: %+v", r)
	}
}

func TestNoMatchBelowThreshold(t *testing.T) {
	f := &fake{name: "fake", search: []providers.Track{tr("1", "Other", "Nobody", 0, "")}}
	if r := run(t, f, Listen{Title: "Song", Artist: "Artist"}); len(r.Links) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestRateLimitAborts(t *testing.T) {
	f := &fake{name: "fake", err: &providers.RateLimitedError{Provider: "fake", RetryAfter: 3}}
	_, err := New(providers.NewRegistry(f), Config{}).Match(context.Background(), Listen{Title: "a", Artist: "b"})
	var rl *providers.RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalize(t *testing.T) {
	if NormTitle("Hello (feat. X) - 2011 Remaster") != "hello" {
		t.Fatalf("got %q", NormTitle("Hello (feat. X) - 2011 Remaster"))
	}
	if Similarity("abc", "abd") < 0.6 || Similarity("abc", "") != 0 {
		t.Fatal("similarity")
	}
}

// trOn is tr with the releases (title, date) the recording is on.
func trOn(id, title, artist string, dur int, releases ...[2]string) providers.Track {
	t := tr(id, title, artist, dur, "")
	for _, r := range releases {
		t.Releases = append(t.Releases, providers.ReleaseRef{Title: r[0], Date: r[1]})
	}
	return t
}

// MusicBrainz keeps a recording per edition. The album the listen was on picks
// the one on a release from the same date; the other edition's recording is not
// a rival.
func TestFuzzyReleaseDatePicksTheRecordingOfTheListensAlbum(t *testing.T) {
	f := &fake{name: "fake", search: []providers.Track{
		trOn("A", "Gorilla Glue", "Cheem", 184235, [2]string{"Power Move", "2025-11-14"}),
		trOn("B", "Gorilla Glue", "Cheem", 184000, [2]string{"Power Move - Victory Lap", "2026-03-06"}, [2]string{"Power Move - Victory Lap", "2026-06-05"}),
	}}
	l := Listen{Title: "Gorilla Glue", Artist: "Cheem", Album: "Power Move", DurationMs: 184306}
	// Without the date: two near-identical candidates, nothing auto (as before).
	if _, ok := run(t, f, l).Best(); ok {
		t.Fatal("setup: expected an ambiguity without a release date")
	}
	l.ReleaseDate = "2025-11-14"
	r := run(t, f, l)
	b, ok := r.Best()
	if !ok || b.ProviderID != "A" {
		t.Fatalf("want A auto: %+v", r)
	}
	for _, link := range r.Links {
		if link.ProviderID == "B" && link.Status != StatusCandidate {
			t.Fatalf("the other edition's recording must stay a candidate: %+v", link)
		}
	}
}

func TestFuzzyReleaseDateNeedsEveryRivalToBeOnOtherDates(t *testing.T) {
	l := Listen{Title: "Hello", Artist: "Adele", Album: "25", DurationMs: 295000, ReleaseDate: "2015-11-20"}
	for name, rival := range map[string]providers.Track{
		"rival has no dates":          trOn("B", "Hello", "Adele", 295000),
		"rival on the same date":      trOn("B", "Hello", "Adele", 295000, [2]string{"25 (Deluxe)", "2015-11-20"}),
		"rival date unusable (empty)": trOn("B", "Hello", "Adele", 295000, [2]string{"25", ""}),
	} {
		f := &fake{name: "fake", search: []providers.Track{trOn("A", "Hello", "Adele", 295000, [2]string{"25", "2015-11-20"}), rival}}
		if _, ok := run(t, f, l).Best(); ok {
			t.Fatalf("%s: auto-picked between two equal candidates", name)
		}
	}
}

func TestDatesAgreeAtTheCoarserPrecision(t *testing.T) {
	for _, c := range []struct {
		a, b          string
		agree, usable bool
	}{
		{"2025-11-14", "2025-11-14", true, true},
		{"2025", "2025-11-14", true, true},
		{"2025-11", "2025-11-14", true, true},
		{"2025-11-14", "2025-11-15", false, true},
		{"2025-11-14", "2026-03-06", false, true},
		{"", "2025-11-14", false, false},
		{"2025-11-14", "", false, false},
		{"25", "2025-11-14", false, false},
	} {
		if a, u := datesAgree(c.a, c.b); a != c.agree || u != c.usable {
			t.Fatalf("%q vs %q: agree=%v usable=%v", c.a, c.b, a, u)
		}
	}
}

func TestISRCSharedIsResolvedByReleaseDate(t *testing.T) {
	l := Listen{Title: "Song", Artist: "Artist", ISRC: "I", ReleaseDate: "2020-01-02"}
	hits := func(second providers.Track) *fake {
		return &fake{name: "fake", isrc: map[string][]providers.Track{"I": {
			trOn("1", "Song", "Artist", 0, [2]string{"Album", "2020-01-02"}), second}}}
	}
	r := run(t, hits(trOn("2", "Song", "Artist", 0, [2]string{"Reissue", "2023-05-05"})), l)
	if b, ok := r.Best(); !ok || b.ProviderID != "1" {
		t.Fatalf("one on the date, the other only on another: want 1 auto: %+v", r)
	}
	for name, second := range map[string]providers.Track{
		"the other has no dates": trOn("2", "Song", "Artist", 0),
		"both on the date":       trOn("2", "Song", "Artist", 0, [2]string{"Album (Deluxe)", "2020-01-02"}),
	} {
		if _, ok := run(t, hits(second), l).Best(); ok {
			t.Fatalf("%s: must stay ambiguous", name)
		}
	}
}

func TestISRCSingleHitWithDifferentTextAutoLinksOnTheReleaseDate(t *testing.T) {
	f := &fake{name: "fake", isrc: map[string][]providers.Track{"I": {trOn("1", "花の塔", "さユり", 0, [2]string{"花の塔", "2022-07-03"})}}}
	r := run(t, f, Listen{Title: "Tower of Flower", Artist: "Sayuri", ISRC: "I", ReleaseDate: "2022-07-03"})
	if _, ok := r.Best(); !ok {
		t.Fatalf("%+v", r)
	}
	if _, ok := run(t, f, Listen{Title: "Tower of Flower", Artist: "Sayuri", ISRC: "I", ReleaseDate: "2019-01-01"}).Best(); ok {
		t.Fatal("a different release date must not corroborate")
	}
}

// mbTrack is a MusicBrainz recording of "Hey Nineteen" on the given releases.
func mbTrack(id string, dur int, releases ...string) providers.Track {
	t := providers.Track{Provider: providers.MusicBrainz, ProviderID: id, Title: "Hey Nineteen", DurationMs: dur,
		Artists: []providers.Artist{{Name: "Steely Dan"}}}
	for _, r := range releases {
		t.Releases = append(t.Releases, providers.ReleaseRef{ProviderID: r, Title: "Gaucho"})
	}
	return t
}

func mbRun(t *testing.T, f *fake, l Listen) Result {
	t.Helper()
	f.name = providers.MusicBrainz
	l.Artist, l.Title, l.Album = "Steely Dan", "Hey Nineteen", "Gaucho"
	return run(t, f, l)
}

func linkFor(r Result, id string) Link {
	for _, l := range r.Links {
		if l.ProviderID == id {
			return l
		}
	}
	return Link{}
}

// Several MusicBrainz recordings of one song tie on text. The release the
// listen named is on exactly one of them: that one is auto, the rest stay
// candidates.
func TestReleaseTheListenNamedPicksTheRecording(t *testing.T) {
	f := &fake{search: []providers.Track{
		mbTrack("a", 306000, "r-2004"), mbTrack("b", 304000, "r-1998"), mbTrack("c", 305893, "r-1980", "r-comp"),
	}}
	r := mbRun(t, f, Listen{ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"R-1980"}}}) // ids compare ignoring case
	c := linkFor(r, "c")
	if c.Status != StatusAuto || !c.Corroborated || c.Method != MethodFuzzy {
		t.Fatalf("recording on the named release not auto-linked: %+v", r.Links)
	}
	for _, id := range []string{"a", "b"} {
		if l := linkFor(r, id); l.Status != StatusCandidate || l.Corroborated {
			t.Fatalf("%s should stay a candidate: %+v", id, l)
		}
	}
	if best, ok := r.Best(); !ok || best.ProviderID != "c" {
		t.Fatalf("best = %+v", best)
	}
}

func TestWithoutAReleaseTheTieStaysACandidateList(t *testing.T) {
	f := &fake{search: []providers.Track{mbTrack("a", 306000, "r-2004"), mbTrack("b", 304000, "r-1998")}}
	for name, ids := range map[string][]string{
		"none named":               nil,
		"named release on neither": {"r-elsewhere"},
		"named release on both":    {"r-2004", "r-1998"},
	} {
		r := mbRun(t, f, Listen{ReleaseIDs: map[string][]string{providers.MusicBrainz: ids}})
		if _, ok := r.Best(); ok {
			t.Fatalf("%s: a tie was auto-linked: %+v", name, r.Links)
		}
	}
	// Both candidates list the one release the listen named: still ambiguous.
	f = &fake{search: []providers.Track{mbTrack("a", 306000, "r-1"), mbTrack("b", 304000, "r-1")}}
	if _, ok := mbRun(t, f, Listen{ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r-1"}}}).Best(); ok {
		t.Fatal("two recordings on the named release were auto-linked")
	}
}

// The release id says which recording, not what the text means: a candidate
// whose title or artist does not read as the listen's is never auto-linked by it.
func TestReleaseIDDoesNotLinkARecordingWithTheWrongText(t *testing.T) {
	wrong := mbTrack("w", 306000, "r-1")
	wrong.Title = "Something Else Entirely"
	f := &fake{search: []providers.Track{wrong, mbTrack("b", 304000, "r-2")}}
	if r := mbRun(t, f, Listen{ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r-1"}}}); linkFor(r, "w").Status == StatusAuto {
		t.Fatalf("auto-linked on the id alone: %+v", r.Links)
	}
}

// Release ids belong to a provider: another provider's candidates are never
// judged by a MusicBrainz release id, and a candidate listing no releases says
// nothing for or against.
func TestReleaseIDOnlyJudgesMusicBrainzCandidates(t *testing.T) {
	other := mbTrack("x", 306000, "r-1")
	other.Provider = "spotify"
	f := &fake{search: []providers.Track{other, mbTrack("y", 304000, "r-2")}}
	if r := mbRun(t, f, Listen{ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r-1"}}}); linkFor(r, "x").Corroborated {
		t.Fatalf("another provider's candidate was matched on a MusicBrainz id: %+v", r.Links)
	}
	f = &fake{search: []providers.Track{mbTrack("p", 306000), mbTrack("q", 304000, "r-1"), mbTrack("s", 305000, "r-2")}}
	r := mbRun(t, f, Listen{ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r-1"}}})
	if q := linkFor(r, "q"); q.Status != StatusAuto || !q.Corroborated {
		t.Fatalf("the one listing the release should win over one with no release list: %+v", r.Links)
	}
}

// Several recordings share an ISRC: the named release singles one out, as the
// release date can.
func TestReleaseTheListenNamedPicksAmongISRCHits(t *testing.T) {
	f := &fake{isrc: map[string][]providers.Track{"ISRC1": {mbTrack("a", 306000, "r-1"), mbTrack("b", 306000, "r-2")}}}
	r := mbRun(t, f, Listen{ISRC: "ISRC1", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r-2"}}})
	if b := linkFor(r, "b"); b.Status != StatusAuto {
		t.Fatalf("ISRC hit on the named release not auto-linked: %+v", r.Links)
	}
	if a := linkFor(r, "a"); a.Status == StatusAuto {
		t.Fatalf("ISRC hit on another release auto-linked: %+v", a)
	}
	if _, ok := mbRun(t, f, Listen{ISRC: "ISRC1"}).Best(); ok {
		t.Fatal("two ISRC hits with no release were auto-linked")
	}
}

// An ISRC hit that lists no releases could be the one played, so it keeps the
// others from being singled out by the release id.
func TestISRCHitWithNoReleaseListKeepsTheTieOpen(t *testing.T) {
	f := &fake{isrc: map[string][]providers.Track{"ISRC1": {mbTrack("a", 306000, "r-1"), mbTrack("b", 306000)}}}
	if _, ok := mbRun(t, f, Listen{ISRC: "ISRC1", ReleaseIDs: map[string][]string{providers.MusicBrainz: []string{"r-1"}}}).Best(); ok {
		t.Fatal("a hit with an unknown release list was ruled out")
	}
}

// A lone candidate that matches exactly is Corroborated (no name guess between
// candidates); a near match, or one of several, is not.
func TestLoneExactCandidateIsCorroborated(t *testing.T) {
	exact := &fake{search: []providers.Track{mbTrack("a", 306000, "r-1")}}
	if l := linkFor(mbRun(t, exact, Listen{}), "a"); l.Status != StatusAuto || !l.Corroborated {
		t.Fatalf("lone exact match: %+v", l)
	}
	near := mbTrack("n", 306000, "r-1")
	near.Artists = []providers.Artist{{Name: "Steely Dans"}}
	if l := linkFor(mbRun(t, &fake{search: []providers.Track{near}}, Listen{}), "n"); l.Status != StatusAuto || l.Corroborated {
		t.Fatalf("a near match must stay a plain fuzzy auto link: %+v", l)
	}
	two := &fake{search: []providers.Track{mbTrack("a", 306000, "r-1"), mbTrack("b", 304000, "r-2")}}
	for _, l := range mbRun(t, two, Listen{}).Links {
		if l.Corroborated {
			t.Fatalf("one of two exact matches was corroborated: %+v", l)
		}
	}
	// An exact match with a clearly weaker rival is auto, but it is not alone:
	// the rival is a name that could have been the one, so the gate still applies.
	weak := mbTrack("w", 306000, "r-2")
	weak.Title = "Hey Ninety"
	if l := linkFor(mbRun(t, &fake{search: []providers.Track{mbTrack("a", 306000, "r-1"), weak}}, Listen{}), "a"); l.Status != StatusAuto || l.Corroborated {
		t.Fatalf("a candidate with a rival was called lone: %+v", l)
	}
	// A different duration is a difference: not exact, however close.
	off := mbTrack("d", 330000, "r-1")
	if l := linkFor(mbRun(t, &fake{search: []providers.Track{off}}, Listen{DurationMs: 306000}), "d"); l.Corroborated {
		t.Fatalf("a candidate with a different duration was called exact: %+v", l)
	}
}
