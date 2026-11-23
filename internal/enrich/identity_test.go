package enrich

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"younithm/internal/edit"

	"github.com/go-chi/chi/v5"

	"younithm/internal/db"
	"younithm/internal/identity"
	"younithm/internal/providers"
	"younithm/internal/providers/match"
	"younithm/internal/providers/snapshot"
)

// catalogFake is a provider with a catalogue: lookup by id and by ISRC.
type catalogFake struct {
	name   string
	byID   map[string]providers.Track
	byISRC map[string]providers.Track
	search []providers.Track
	gets   int
	isrcs  int // ISRC lookups served
}

func (f *catalogFake) Name() string { return f.name }
func (f *catalogFake) SearchTrack(context.Context, providers.TrackQuery) ([]providers.Track, error) {
	return f.search, nil
}
func (f *catalogFake) GetTrack(_ context.Context, id string) (*providers.Track, error) {
	f.gets++
	t, ok := f.byID[id]
	if !ok {
		return nil, providers.ErrNotFound
	}
	return &t, nil
}
func (f *catalogFake) LookupISRC(_ context.Context, isrc string) ([]providers.Track, error) {
	f.isrcs++
	if t, ok := f.byISRC[isrc]; ok {
		return []providers.Track{t}, nil
	}
	return nil, nil
}
func (f *catalogFake) GetArtwork(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}
func (f *catalogFake) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}

type idEnv struct {
	db  *sql.DB
	m   *Manager
	now time.Time
}

func newIDEnv(t *testing.T, ps ...providers.Provider) *idEnv {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	e := &idEnv{db: d, now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	e.m = NewManager(d, match.New(providers.NewRegistry(ps...), match.Config{}), snapshot.New(d),
		Options{Now: func() time.Time { return e.now }, MaxAttempts: 3})
	return e
}

func (e *idEnv) listen(t *testing.T, in identity.Input) (rec, rel int64) {
	t.Helper()
	rec, rel, err := (&identity.Resolver{}).Resolve(context.Background(), e.db, in)
	if err != nil {
		t.Fatal(err)
	}
	return rec, rel
}

func (e *idEnv) run(t *testing.T) {
	t.Helper()
	if _, err := e.m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *idEnv) n(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func spotifyTrack() providers.Track {
	return providers.Track{
		Provider: providers.Spotify, ProviderID: "spT", Title: "Song", DurationMs: 200000, ISRCs: []string{"USAAA1111111"},
		Artists: []providers.Artist{{ProviderID: "spA", Name: "Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "spAl", Title: "Album", Date: "2020-01-02", Type: "album",
			Artists: []providers.Artist{{ProviderID: "spAA", Name: "Various Artists"}}}},
	}
}

func appleTrack() providers.Track {
	return providers.Track{
		Provider: providers.AppleMusic, ProviderID: "apT", Title: "Song - Remastered", ISRCs: []string{"USAAA1111111"},
		Artists:  []providers.Artist{{ProviderID: "apA", Name: "Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "apAl", Title: "Album"}},
	}
}

func TestCatalogIDLookupFillsISRCArtistsAlbumAndCrossPlatformIDs(t *testing.T) {
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spT": spotifyTrack()}}
	ap := &catalogFake{name: providers.AppleMusic, byISRC: map[string]providers.Track{"USAAA1111111": appleTrack()}}
	e := newIDEnv(t, sp, ap)

	// A Spotify listen: catalog ids only, no ISRC, no album artist.
	rec, rel := e.listen(t, identity.Input{
		Artist: "Artist", Title: "Song", Album: "Album",
		Credits:        []identity.Credit{{Name: "Artist", SpotifyID: "spA"}},
		SpotifyTrackID: "spT", SpotifyAlbumID: "spAl",
	})
	e.run(t)

	link := func(kind string, id int64, prov string) int {
		return e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type=? AND entity_id=? AND provider=? AND status<>'candidate'`, kind, id, prov)
	}
	if link("recording", rec, "isrc") != 1 {
		t.Fatal("ISRC not attached from the Spotify lookup")
	}
	if link("recording", rec, "apple") != 1 {
		t.Fatal("Apple song not found through the ISRC")
	}
	var artist int64
	e.db.QueryRow(`SELECT artist_id FROM recording_artist WHERE recording_id=?`, rec).Scan(&artist)
	if link("artist", artist, "apple") != 1 {
		t.Fatal("Apple artist id not attached to the paired artist")
	}
	if e.n(t, `SELECT COUNT(*) FROM alias WHERE kind='recording' AND entity_id=? AND raw_string=?`, rec,
		identity.RecordingAliasKey("Artist", "Song - Remastered")) != 1 {
		t.Fatal("title variant not kept as a recording alias")
	}
	if link("release", rel, "apple") != 1 {
		t.Fatal("Apple album id not attached")
	}
	// Album artist, date and type came from the lookup; the track's artist was not copied.
	if e.n(t, `SELECT COUNT(*) FROM release_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.release_id=? AND a.name='Various Artists'`, rel) != 1 {
		t.Fatal("album artist not filled from the provider")
	}
	if e.n(t, `SELECT COUNT(*) FROM release WHERE id=? AND release_date='2020-01-02' AND release_type='album'`, rel) != 1 {
		t.Fatal("album date/type not filled")
	}
	if e.n(t, `SELECT verified FROM artist WHERE id=?`, artist) != 1 {
		t.Fatal("artist not verified")
	}
	// The variant title now finds the same recording.
	got, _ := e.listen(t, identity.Parse(nil, identity.Input{Artist: "Artist", Title: "Song - Remastered"}))
	if got != rec {
		t.Fatalf("alias not used by resolving: got %d want %d", got, rec)
	}
}

func TestMBIDOnlyGetsISRCAndThenOtherPlatforms(t *testing.T) {
	mbTrack := providers.Track{Provider: providers.MusicBrainz, ProviderID: "mb1", Title: "Song", ISRCs: []string{"USAAA1111111"},
		Artists: []providers.Artist{{ProviderID: "mbA", Name: "Artist"}}}
	mb := &catalogFake{name: providers.MusicBrainz, byID: map[string]providers.Track{"mb1": mbTrack}}
	sp := &catalogFake{name: providers.Spotify, byISRC: map[string]providers.Track{"USAAA1111111": spotifyTrack()}}
	e := newIDEnv(t, mb, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", RecordingMBID: "mb1",
		Credits: []identity.Credit{{Name: "Artist"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='spotify' AND status='auto'`, rec) != 1 {
		t.Fatal("Spotify id not found from the MusicBrainz recording's ISRC")
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='isrc'`, rec) != 1 {
		t.Fatal("ISRC not attached")
	}
}

func TestUnconfiguredPlatformKeepsItsIDsAndWorksOnceConnected(t *testing.T) {
	e := newIDEnv(t) // nothing connected
	rec, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", SpotifyTrackID: "spT",
		Credits: []identity.Credit{{Name: "Artist", SpotifyID: "spA"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='spotify'`, rec) != 1 {
		t.Fatal("submitted Spotify id was lost")
	}
	// Connect Spotify later and requeue.
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spT": spotifyTrack()}}
	e.m.matcher = match.New(providers.NewRegistry(sp), match.Config{})
	e.now = e.now.Add(2 * time.Hour)
	if _, err := e.m.RequeueAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='isrc'`, rec) != 1 {
		t.Fatal("lookup did not run after the platform was connected")
	}
}

func TestProviderIDOwnedByAnotherRecordingBecomesMergeCandidate(t *testing.T) {
	ap := &catalogFake{name: providers.AppleMusic, byISRC: map[string]providers.Track{"USAAA1111111": appleTrack()}}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spT": spotifyTrack()}}
	e := newIDEnv(t, sp, ap)
	// Two recordings that are really one: an Apple-only listen, and a Spotify one.
	r1, _ := e.listen(t, identity.Input{Artist: "Artiste", Title: "Song - Remastered", AppleSongID: "apT",
		Credits: []identity.Credit{{Name: "Artiste"}}})
	r2, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", SpotifyTrackID: "spT",
		Credits: []identity.Credit{{Name: "Artist", SpotifyID: "spA"}}})
	if r1 == r2 {
		t.Fatal("setup: expected two recordings")
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE kind='recording'`) < 1 {
		t.Fatal("duplicate recordings were not flagged")
	}
	// Nothing was merged or moved.
	if e.n(t, `SELECT COUNT(*) FROM recording`) != 2 {
		t.Fatal("recordings were merged automatically")
	}
}

func TestFuzzyConsentOffMakesFuzzyHitsCandidates(t *testing.T) {
	// A near match (the catalogue spells the artist slightly differently) is a
	// name guess: without consent it is only offered for review.
	near := track("mb-9", "Great Song", "The Bands")
	fz := &catalogFake{name: providers.MusicBrainz, search: []providers.Track{near}}
	e := newIDEnv(t, fz)
	rec, _ := e.listen(t, identity.Input{Artist: "The Band", Title: "Great Song", Credits: []identity.Credit{{Name: "The Band"}}})
	e.db.Exec(`UPDATE recording SET fuzzy_consent=0 WHERE id=?`, rec)
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND status='auto'`, rec) != 0 {
		t.Fatal("fuzzy match auto-linked without consent")
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND status='candidate'`, rec) != 1 {
		t.Fatal("fuzzy match not offered as a candidate")
	}
}

// A lone candidate that reads exactly like the listen is not a guess between
// names, so it links even where fuzzy matching was not consented to. Two exact
// candidates are a tie and stay for review.
func TestLoneExactMatchLinksWithoutFuzzyConsent(t *testing.T) {
	fz := &catalogFake{name: providers.MusicBrainz, search: []providers.Track{track("mb-9", "Great Song", "The Band")}}
	e := newIDEnv(t, fz)
	rec, _ := e.listen(t, identity.Input{Artist: "The Band", Title: "Great Song", Credits: []identity.Credit{{Name: "The Band"}}})
	e.db.Exec(`UPDATE recording SET fuzzy_consent=0 WHERE id=?`, rec)
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider_id='mb-9' AND status='auto'`, rec) != 1 {
		t.Fatal("a lone exact match was not linked")
	}

	two := &catalogFake{name: providers.MusicBrainz, search: []providers.Track{track("mb-a", "Great Song", "The Band"), track("mb-b", "Great Song", "The Band")}}
	e2 := newIDEnv(t, two)
	rec2, _ := e2.listen(t, identity.Input{Artist: "The Band", Title: "Great Song", Credits: []identity.Credit{{Name: "The Band"}}})
	e2.db.Exec(`UPDATE recording SET fuzzy_consent=0 WHERE id=?`, rec2)
	e2.run(t)
	if e2.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND status='auto'`, rec2) != 0 {
		t.Fatal("a tie of exact matches was linked")
	}
}

func TestMostPlayedRecordingsAreEnrichedFirst(t *testing.T) {
	e := newIDEnv(t, &catalogFake{name: providers.MusicBrainz})
	low, _ := e.listen(t, identity.Input{Artist: "A", Title: "Rare", Credits: []identity.Credit{{Name: "A"}}})
	high, _ := e.listen(t, identity.Input{Artist: "A", Title: "Hit", Credits: []identity.Credit{{Name: "A"}}})
	for i := 0; i < 5; i++ {
		e.db.Exec(`INSERT INTO scrobble (played_at, raw_artist, raw_title, recording_id) VALUES (?,?,?,?)`,
			time.Now().Add(time.Duration(i)*time.Minute).Format(time.RFC3339), "A", "Hit", high)
	}
	e.db.Exec(`INSERT INTO scrobble (played_at, raw_artist, raw_title, recording_id) VALUES ('2020-01-01T00:00:00Z','A','Rare',?)`, low)
	if _, err := e.m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, err := e.m.claim(context.Background())
	if err != nil || j == nil || j.recordingID != high {
		t.Fatalf("first job = %+v err=%v, want recording %d", j, err, high)
	}
}

// A listen's artist string is one artist, as written. A catalog id the listen
// itself named replaces it with the catalog's credits.
func TestCatalogCreditsReplaceTheArtistString(t *testing.T) {
	collab := providers.Track{
		Provider: providers.Spotify, ProviderID: "spC", Title: "Duet", ISRCs: []string{"USBBB2222222"},
		Artists: []providers.Artist{{ProviderID: "spA", Name: "Alice", JoinPhrase: " & "}, {ProviderID: "spB", Name: "Bob"}},
	}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spC": collab}}
	e := newIDEnv(t, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "Alice & Bob", Title: "Duet", SpotifyTrackID: "spC",
		Credits: []identity.Credit{{Name: "Alice & Bob"}}})
	if e.n(t, `SELECT COUNT(*) FROM artist WHERE name='Alice & Bob'`) != 1 {
		t.Fatal("setup: the string should be one artist")
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 2 {
		t.Fatal("recording was not given the catalog's two artists")
	}
	if e.n(t, `SELECT COUNT(*) FROM artist WHERE name='Alice & Bob'`) != 0 {
		t.Fatal("the replaced string artist was not removed")
	}
	var jp string
	e.db.QueryRow(`SELECT join_phrase FROM recording_artist WHERE recording_id=? AND position=0`, rec).Scan(&jp)
	if jp != " & " {
		t.Fatalf("join phrase %q", jp)
	}
	if e.n(t, `SELECT COUNT(*) FROM artist a JOIN external_link l ON l.entity_type='artist' AND l.entity_id=a.id
		WHERE a.name IN ('Alice','Bob') AND l.provider='spotify' AND a.verified=1`) != 2 {
		t.Fatal("credited artists not verified with their Spotify ids")
	}
	// The same raw listen resolves straight to the recording and creates nothing.
	got, _ := e.listen(t, identity.Input{Artist: "Alice & Bob", Title: "Duet", Credits: []identity.Credit{{Name: "Alice & Bob"}}})
	if got != rec {
		t.Fatalf("raw string alias: got %d want %d", got, rec)
	}
	if e.n(t, `SELECT COUNT(*) FROM artist`) != 2 {
		t.Fatalf("resolving the alias left a stray artist: %d artists", e.n(t, `SELECT COUNT(*) FROM artist`))
	}
}

func TestMusicBrainzAlbumArtistsFillAnAlbumWithNone(t *testing.T) {
	mbTrack := providers.Track{Provider: providers.MusicBrainz, ProviderID: "mb1", Title: "Song",
		Artists: []providers.Artist{{ProviderID: "mbA", Name: "Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "mbR", Title: "Now 100", GroupID: "mbG",
			Artists: []providers.Artist{{ProviderID: "mbV", Name: "Various", JoinPhrase: " & "}, {ProviderID: "mbW", Name: "Artists"}}}}}
	e := newIDEnv(t, &catalogFake{name: providers.MusicBrainz, byID: map[string]providers.Track{"mb1": mbTrack}})
	// A sparse listen made the album with no album artist.
	rec, rel := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Now 100", RecordingMBID: "mb1",
		Credits: []identity.Credit{{Name: "Artist"}}})
	if e.n(t, `SELECT COUNT(*) FROM release_artist WHERE release_id=?`, rel) != 0 {
		t.Fatal("setup: album should have no album artist yet")
	}
	e.run(t)
	_ = rec
	if e.n(t, `SELECT COUNT(*) FROM release_artist ra JOIN artist a ON a.id=ra.artist_id
		JOIN external_link l ON l.entity_type='artist' AND l.entity_id=a.id AND l.provider='musicbrainz'
		WHERE ra.release_id=? AND a.name IN ('Various','Artists')`, rel) != 2 {
		t.Fatal("MusicBrainz album artists not attached with their MBIDs")
	}
	var jp string
	e.db.QueryRow(`SELECT join_phrase FROM release_artist WHERE release_id=? AND position=0`, rel).Scan(&jp)
	if jp != " & " {
		t.Fatalf("join phrase %q", jp)
	}
	// The track's own artist was not copied onto the album.
	if e.n(t, `SELECT COUNT(*) FROM release_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.release_id=? AND a.name='Artist'`, rel) != 0 {
		t.Fatal("track artist copied to the album")
	}
	if e.n(t, `SELECT COUNT(*) FROM release WHERE id=? AND release_group_mbid='mbG'`, rel) != 1 {
		t.Fatal("release group not attached")
	}
}

// A Spotify history row names only the first artist. When a provider credits
// more, the missing artists are added (found by id when we already know them),
// in the provider's order, with its join phrases.
func TestCatalogCreditsAddTheMissingArtists(t *testing.T) {
	duet := providers.Track{
		Provider: providers.Spotify, ProviderID: "spD", Title: "Comet", ISRCs: []string{"JPAAA0000001"},
		Artists: []providers.Artist{{ProviderID: "spNor", Name: "Noriken"}, {ProviderID: "spAran", Name: "aran"}},
	}
	mb := providers.Track{
		Provider: providers.MusicBrainz, ProviderID: "mbD", Title: "Comet", ISRCs: []string{"JPAAA0000001"},
		Artists: []providers.Artist{{ProviderID: "mbNor", Name: "Noriken", JoinPhrase: " & "}, {ProviderID: "mbAran", Name: "aran"}},
	}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spD": duet}}
	mbp := &catalogFake{name: providers.MusicBrainz, byISRC: map[string]providers.Track{"JPAAA0000001": mb}}
	e := newIDEnv(t, sp, mbp)

	// aran already exists from another recording, with her Spotify id.
	e.listen(t, identity.Input{Artist: "aran", Title: "IO", Credits: []identity.Credit{{Name: "aran", SpotifyID: "spAran"}}})
	var aran int64
	e.db.QueryRow(`SELECT id FROM artist WHERE name='aran'`).Scan(&aran)

	rec, _ := e.listen(t, identity.Input{Artist: "Noriken", Title: "Comet", SpotifyTrackID: "spD",
		Credits: []identity.Credit{{Name: "Noriken", SpotifyID: "spNor"}}})
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 1 {
		t.Fatal("setup: the listen should carry one artist")
	}
	e.run(t)

	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 2 {
		t.Fatal("the credited second artist was not added")
	}
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=? AND artist_id=? AND position=1`, rec, aran) != 1 {
		t.Fatal("the existing artist (found by Spotify id) was not used, or is out of order")
	}
	if e.n(t, `SELECT COUNT(*) FROM artist WHERE lower(name)='aran'`) != 1 {
		t.Fatal("a duplicate aran was created")
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND entity_id=? AND provider='musicbrainz' AND provider_id='mbAran'`, aran) != 1 {
		t.Fatal("MusicBrainz id not attached to the added artist")
	}
	if e.n(t, `SELECT COUNT(*) FROM artist WHERE verified=0`) != 0 {
		t.Fatal("a credited artist stayed unverified")
	}
	// Running it again changes nothing.
	if _, err := e.m.RequeueAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 2 || e.n(t, `SELECT COUNT(*) FROM artist`) != 2 {
		t.Fatal("a second run duplicated credits or artists")
	}
}

func TestCreditsAreReorderedToTheProvidersOrderWhenAdding(t *testing.T) {
	track := providers.Track{
		Provider: providers.Spotify, ProviderID: "spE", Title: "Feature",
		Artists: []providers.Artist{{ProviderID: "spB", Name: "Bea", JoinPhrase: " feat. "}, {ProviderID: "spA", Name: "Alf"}},
	}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spE": track}}
	e := newIDEnv(t, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "Alf", Title: "Feature", SpotifyTrackID: "spE",
		Credits: []identity.Credit{{Name: "Alf", SpotifyID: "spA"}}})
	e.run(t)
	var first, second string
	e.db.QueryRow(`SELECT a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=? AND ra.position=0`, rec).Scan(&first)
	e.db.QueryRow(`SELECT a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=? AND ra.position=1`, rec).Scan(&second)
	if first != "Bea" || second != "Alf" {
		t.Fatalf("order %q, %q", first, second)
	}
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=? AND position=0 AND join_phrase=' feat. '`, rec) != 1 {
		t.Fatal("join phrase not set on the first credit")
	}
}

// The catalog is the truth for a recording whose listen named it: an artist it
// does not credit is replaced (and dropped, being unverified and unlinked).
func TestCatalogCreditsReplaceAnArtistItDoesNotCredit(t *testing.T) {
	track := providers.Track{
		Provider: providers.Spotify, ProviderID: "spF", Title: "Odd",
		Artists: []providers.Artist{{ProviderID: "spA", Name: "Alice"}, {ProviderID: "spB", Name: "Bob"}},
	}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spF": track}}
	e := newIDEnv(t, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "Alice", Title: "Odd", SpotifyTrackID: "spF",
		Credits: []identity.Credit{{Name: "Alice", SpotifyID: "spA"}, {Name: "Zed"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=? AND a.name IN ('Alice','Bob')`, rec) != 2 ||
		e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 2 {
		t.Fatal("recording does not have exactly the catalog's artists")
	}
	if e.n(t, `SELECT COUNT(*) FROM artist WHERE name='Zed'`) != 0 {
		t.Fatal("the artist the catalog does not credit was kept")
	}
}

// Equal names pair even when the provider credits more artists than we hold,
// so a first-time artist with no ids yet still gets the rest of the credits.
func TestCreditsCompleteForAnArtistWithNoIDsYet(t *testing.T) {
	track := providers.Track{
		Provider: providers.Spotify, ProviderID: "spG", Title: "Remix",
		Artists: []providers.Artist{{ProviderID: "spN", Name: "NOMA"}, {ProviderID: "spS", Name: "Synthion"}},
	}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spG": track}}
	e := newIDEnv(t, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "NOMA", Title: "Remix", SpotifyTrackID: "spG", Credits: []identity.Credit{{Name: "NOMA"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 2 {
		t.Fatal("Synthion was not added")
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link l JOIN artist a ON a.id=l.entity_id WHERE l.entity_type='artist' AND a.name='NOMA' AND l.provider_id='spN'`) != 1 {
		t.Fatal("NOMA did not get its Spotify id")
	}
}

// Only a catalog id the listen named replaces credits. A link we found
// ourselves (here MusicBrainz, found through the ISRC) may be wrong, so even held
// and looked up by id it never replaces the artists: here it names a different
// artist altogether, which must change nothing.
func TestLinksWeFoundDoNotReplaceCredits(t *testing.T) {
	mb := providers.Track{Provider: providers.MusicBrainz, ProviderID: "mbI", Title: "Song", ISRCs: []string{"USIII0000001"},
		Artists: []providers.Artist{{ProviderID: "mbM", Name: "Mallory"}, {ProviderID: "mbN", Name: "Niaj"}}}
	mbp := &catalogFake{name: providers.MusicBrainz, byISRC: map[string]providers.Track{"USIII0000001": mb}, byID: map[string]providers.Track{"mbI": mb}}
	e := newIDEnv(t, mbp)
	rec, _ := e.listen(t, identity.Input{Artist: "Alice", Title: "Song", ISRC: "USIII0000001", Credits: []identity.Credit{{Name: "Alice"}}})
	e.run(t) // MusicBrainz is found by the ISRC
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz' AND match_method='isrc'`, rec) != 1 {
		t.Fatal("setup: MusicBrainz was not linked by ISRC")
	}
	if _, err := e.m.RequeueAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.run(t) // now the link is held and fetched by id
	if e.n(t, `SELECT COUNT(*) FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=? AND a.name='Alice'`, rec) != 1 ||
		e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 1 ||
		e.n(t, `SELECT COUNT(*) FROM artist WHERE name IN ('Mallory','Niaj')`) != 0 {
		t.Fatal("a link we found ourselves replaced or extended the credits with artists that are not ours")
	}
}

// A recording that was edited by hand keeps its artists.
func TestHandEditedRecordingKeepsItsArtists(t *testing.T) {
	track := providers.Track{Provider: providers.Spotify, ProviderID: "spJ", Title: "Song",
		Artists: []providers.Artist{{ProviderID: "spA", Name: "Alice"}, {ProviderID: "spB", Name: "Bob"}}}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spJ": track}}
	e := newIDEnv(t, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "Alice", Title: "Song", SpotifyTrackID: "spJ", Credits: []identity.Credit{{Name: "Alice"}}})
	if _, err := e.db.Exec(`INSERT INTO edit_log (actor, entity_type, entity_id, scope) VALUES ('admin','recording',?, 'entity')`, rec); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 1 {
		t.Fatal("a hand-edited recording had its artists replaced")
	}
}

// Album artists applied to its tracks count as a hand edit of each track the
// edit changed, though the edit is logged on the album.
func TestAlbumArtistsAppliedByHandAreKept(t *testing.T) {
	track := providers.Track{Provider: providers.Spotify, ProviderID: "spJ", Title: "Song",
		Artists: []providers.Artist{{ProviderID: "spA", Name: "Alice"}, {ProviderID: "spB", Name: "Bob"}}}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spJ": track}}
	e := newIDEnv(t, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "Alice", Title: "Song", SpotifyTrackID: "spJ", Credits: []identity.Credit{{Name: "Alice"}}})
	if _, err := e.db.Exec(`INSERT INTO edit_log (actor, entity_type, entity_id, scope, after_json) VALUES ('admin','release',99,'credits',?)`,
		fmt.Sprintf(`[{"t":"recording_artist","k":{"recording_id":%d,"artist_id":1,"role":"primary"},"r":{}}]`, rec)); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 1 {
		t.Fatal("a track whose album artists were applied by hand had its artists replaced")
	}
}

// A match we found ourselves never pairs a differently named artist: the single
// artist on each side is not enough, or one wrong ISRC hit would put another
// artist's ids on ours.
func TestLinkWeFoundNeverPairsADifferentlyNamedArtist(t *testing.T) {
	mb := providers.Track{Provider: providers.MusicBrainz, ProviderID: "mbK", Title: "Song", ISRCs: []string{"USKKK0000001"},
		Artists: []providers.Artist{{ProviderID: "mbAlise", Name: "Alise"}}}
	mbp := &catalogFake{name: providers.MusicBrainz, byISRC: map[string]providers.Track{"USKKK0000001": mb}, byID: map[string]providers.Track{"mbK": mb}}
	e := newIDEnv(t, mbp)
	rec, _ := e.listen(t, identity.Input{Artist: "Alice", Title: "Song", ISRC: "USKKK0000001", Credits: []identity.Credit{{Name: "Alice"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz'`, rec) != 1 {
		t.Fatal("setup: MusicBrainz was not linked by ISRC")
	}
	if _, err := e.m.RequeueAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND provider='musicbrainz'`) != 0 {
		t.Fatal("a differently named artist got the id of an ISRC match we found ourselves")
	}
}

// The same recording under another script links on the first run: the ISRC plus
// the duration the Spotify lookup just returned (ours is unknown until then).
func TestISRCLinkAcrossScriptsNeedsNoSecondRun(t *testing.T) {
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spT": {
		Provider: providers.Spotify, ProviderID: "spT", Title: "Tower of Flower", DurationMs: 275614, ISRCs: []string{"JPLLL0000001"},
		Artists: []providers.Artist{{ProviderID: "spA", Name: "Sayuri"}}}}}
	mb := providers.Track{Provider: providers.MusicBrainz, ProviderID: "mbT", Title: "花の塔", DurationMs: 275700, ISRCs: []string{"JPLLL0000001"},
		Artists: []providers.Artist{{ProviderID: "mbA", Name: "さユり"}}}
	mbp := &catalogFake{name: providers.MusicBrainz, byISRC: map[string]providers.Track{"JPLLL0000001": mb}}
	e := newIDEnv(t, sp, mbp)
	rec, _ := e.listen(t, identity.Input{Artist: "Sayuri", Title: "Tower of Flower", SpotifyTrackID: "spT", Credits: []identity.Credit{{Name: "Sayuri"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz' AND status='auto' AND match_method='isrc'`, rec) != 1 {
		t.Fatal("the MusicBrainz recording was not auto-linked by ISRC and duration")
	}
	// Different names, so no artist id crosses over (see the pairing rules).
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND provider='musicbrainz'`) != 0 {
		t.Fatal("a differently named artist got a MusicBrainz id")
	}
}

// ---- release-aware matching

type albumFake struct {
	*catalogFake
	albums map[string]providers.Album
	calls  int
}

func (a *albumFake) GetAlbum(_ context.Context, id string) (*providers.Album, error) {
	a.calls++
	al, ok := a.albums[id]
	if !ok {
		return nil, providers.ErrNotFound
	}
	return &al, nil
}

type barcodeFake struct {
	*catalogFake
	releases map[string][]providers.Release
	calls    int
}

func (b *barcodeFake) LookupBarcode(_ context.Context, upc string) ([]providers.Release, error) {
	b.calls++
	return b.releases[upc], nil
}

const (
	hpISRC  = "QZWV32586126"
	hpUPC   = "199891213535"
	hpAlbum = "spAlbumPowerMove"
)

// gorilla builds the scenario: a Spotify history listen on track 2 of an album
// whose barcode MusicBrainz knows, and MusicBrainz holding the recording twice.
func gorilla(t *testing.T) (*idEnv, *albumFake, *barcodeFake) {
	t.Helper()
	sp := &albumFake{
		catalogFake: &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{
			"spGG": {Provider: providers.Spotify, ProviderID: "spGG", Title: "Gorilla Glue", DurationMs: 184306, ISRCs: []string{hpISRC},
				Artists:  []providers.Artist{{ProviderID: "spCheem", Name: "Cheem"}},
				Releases: []providers.ReleaseRef{{ProviderID: hpAlbum, Title: "Power Move", Date: "2025-11-14", Disc: 1, Position: 2}}},
			"spOne": {Provider: providers.Spotify, ProviderID: "spOne", Title: "Opener", DurationMs: 100000, ISRCs: []string{"QZWV32586001"},
				Artists:  []providers.Artist{{ProviderID: "spCheem", Name: "Cheem"}},
				Releases: []providers.ReleaseRef{{ProviderID: hpAlbum, Title: "Power Move", Date: "2025-11-14", Disc: 1, Position: 1}}},
		}},
		albums: map[string]providers.Album{hpAlbum: {ProviderID: hpAlbum, Title: "Power Move", Date: "2025-11-14", UPC: hpUPC, TotalTracks: 2}},
	}
	mb := gorillaMB()
	return newIDEnv(t, sp, mb), sp, mb
}

// gorillaMB is MusicBrainz as in the scenario above.
func gorillaMB() *barcodeFake {
	mbTrack := func(id string, dur int, rel [2]string) providers.Track {
		return providers.Track{Provider: providers.MusicBrainz, ProviderID: id, Title: "Gorilla Glue", DurationMs: dur,
			Artists:  []providers.Artist{{ProviderID: "mbCheem", Name: "Cheem"}},
			Releases: []providers.ReleaseRef{{ProviderID: "mbRel-" + rel[0], Title: rel[0], Date: rel[1]}}}
	}
	mb := &barcodeFake{
		catalogFake: &catalogFake{name: providers.MusicBrainz, byID: map[string]providers.Track{
			"mbGG":   mbTrack("mbGG", 184235, [2]string{"Power Move", "2025-11-14"}),
			"mbOne":  {Provider: providers.MusicBrainz, ProviderID: "mbOne", Title: "Opener", DurationMs: 100010, Artists: []providers.Artist{{ProviderID: "mbCheem", Name: "Cheem"}}},
			"mbDupe": mbTrack("mbDupe", 184000, [2]string{"Power Move - Victory Lap", "2026-03-06"}),
		}},
		releases: map[string][]providers.Release{hpUPC: {{ProviderID: "mbRel", Title: "Power Move", Date: "2025-11-14", Barcode: hpUPC,
			Tracks: []providers.ReleaseTrack{
				{Disc: 1, Position: 1, RecordingID: "mbOne", Title: "Opener", DurationMs: 100010},
				{Disc: 1, Position: 2, RecordingID: "mbGG", Title: "Gorilla Glue", DurationMs: 184235, ISRCs: []string{hpISRC}},
			}}}},
	}
	// By name MusicBrainz offers both recordings (the duplicate), which is the
	// ambiguity album-first resolves; by ISRC it knows neither.
	mb.search = []providers.Track{mb.byID["mbGG"], mb.byID["mbDupe"]}
	return mb
}

func gorillaListen(t *testing.T, e *idEnv) (gg, one int64) {
	t.Helper()
	gg, _ = e.listen(t, identity.Input{Artist: "Cheem", Title: "Gorilla Glue", Album: "Power Move", SpotifyTrackID: "spGG",
		Credits: []identity.Credit{{Name: "Cheem", SpotifyID: "spCheem"}}})
	one, _ = e.listen(t, identity.Input{Artist: "Cheem", Title: "Opener", Album: "Power Move", SpotifyTrackID: "spOne",
		Credits: []identity.Credit{{Name: "Cheem", SpotifyID: "spCheem"}}})
	return gg, one
}

func mbLink(e *idEnv, t *testing.T, rec int64) (id, method, status string) {
	t.Helper()
	e.db.QueryRow(`SELECT provider_id, match_method, status FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz'
		ORDER BY CASE status WHEN 'candidate' THEN 1 ELSE 0 END LIMIT 1`, rec).Scan(&id, &method, &status)
	return
}

func TestHighPrecisionFindsTheRecordingByBarcodeAndPosition(t *testing.T) {
	e, sp, mb := gorilla(t)
	if err := e.m.SetHighPrecision(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	gg, one := gorillaListen(t, e)
	e.run(t)
	if id, method, status := mbLink(e, t, gg); id != "mbGG" || method != "lookup" || status != "auto" {
		t.Fatalf("Gorilla Glue: %q %q %q", id, method, status)
	}
	if id, _, status := mbLink(e, t, one); id != "mbOne" || status != "auto" {
		t.Fatalf("Opener: %q %q", id, status)
	}
	if mb.isrcs != 0 || mb.calls != 1 || sp.calls != 1 {
		t.Fatalf("ISRC lookups %d (want 0), barcode lookups %d (want 1: the second recording uses the cache), Spotify album calls %d (want 1)", mb.isrcs, mb.calls, sp.calls)
	}
	// MusicBrainz ids reach our release too (it was identified, not guessed).
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='release' AND provider='musicbrainz' AND provider_id='mbRel-Power Move'`) != 1 {
		t.Fatal("the MusicBrainz release id was not attached to our album")
	}
}

func TestHighPrecisionOffMakesNoMusicBrainzBarcodeRequests(t *testing.T) {
	e, sp, mb := gorilla(t)
	if !e.m.HighPrecision(context.Background()) {
		t.Fatal("must default to on")
	}
	if err := e.m.SetHighPrecision(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	gorillaListen(t, e)
	e.run(t)
	if mb.calls != 0 {
		t.Fatalf("MusicBrainz barcode lookups %d: the normal mode must not make them", mb.calls)
	}
	if sp.calls != 1 {
		t.Fatalf("Spotify album lookups %d: the album's barcode links albums across catalogues, once per album", sp.calls)
	}
	if mb.isrcs == 0 {
		t.Fatal("the normal ladder should have asked by ISRC")
	}
}

// Nothing may contradict the album: a duration or ISRC that disagrees means the
// tracklist position is not this recording, so no link is made from it.
func TestHighPrecisionRefusesWhenSomethingContradicts(t *testing.T) {
	for name, mutate := range map[string]func(r *providers.Release, a *albumFake){
		"duration differs": func(r *providers.Release, _ *albumFake) { r.Tracks[1].DurationMs = 184306 + 20000 },
		"ISRC differs":     func(r *providers.Release, _ *albumFake) { r.Tracks[1].ISRCs = []string{"ZZZZZ0000000"} },
		"no barcode": func(_ *providers.Release, a *albumFake) {
			a.albums[hpAlbum] = providers.Album{ProviderID: hpAlbum, Title: "Power Move"}
		},
	} {
		e, sp, mb := gorilla(t)
		rel := mb.releases[hpUPC][0]
		rel.Tracks = append([]providers.ReleaseTrack(nil), rel.Tracks...)
		mutate(&rel, sp)
		mb.releases[hpUPC] = []providers.Release{rel}
		e.m.SetHighPrecision(context.Background(), true)
		gg, _ := gorillaListen(t, e)
		e.run(t)
		if _, method, _ := mbLink(e, t, gg); method == "lookup" {
			t.Fatalf("%s: a link was made by album position", name)
		}
	}
}

func TestHighPrecisionCachesAMissingBarcode(t *testing.T) {
	e, _, mb := gorilla(t)
	mb.releases = map[string][]providers.Release{} // MusicBrainz does not know the album
	e.m.SetHighPrecision(context.Background(), true)
	gorillaListen(t, e)
	e.run(t)
	if mb.calls != 1 {
		t.Fatalf("barcode lookups %d: a miss must be cached for the album's other recordings", mb.calls)
	}
}

// In the normal mode, the album's release date (from the Spotify lookup on the
// first run, before our own album has one) settles the duplicate recordings that
// a name search returns, with no extra request.
func TestNormalModeUsesTheReleaseDateToChooseBetweenDuplicates(t *testing.T) {
	e, _, mb := gorilla(t)
	if err := e.m.SetHighPrecision(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	gg, _ := gorillaListen(t, e)
	e.run(t)
	if id, method, status := mbLink(e, t, gg); id != "mbGG" || status != "auto" || method != "fuzzy" {
		t.Fatalf("want mbGG auto by name + release date; got %q %q %q", id, method, status)
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz' AND status='auto'`, gg) != 1 {
		t.Fatal("the duplicate recording must not be linked too")
	}
	if mb.calls != 0 {
		t.Fatal("normal mode made a barcode request")
	}
}

func TestHighPrecisionSettingEndpoint(t *testing.T) {
	e := newIDEnv(t)
	r := chi.NewRouter()
	NewReview(e.db, e.m, snapshot.New(e.db)).MountAt(r, "/admin", func(h http.Handler) http.Handler { return h })
	do := func(method, body string) (int, string) {
		req := httptest.NewRequest(method, "/admin/matching", strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	if c, b := do("GET", ""); c != 200 || b != `{"high_precision":true}` {
		t.Fatalf("default: %d %s", c, b)
	}
	if c, b := do("PUT", `{"high_precision":false}`); c != 200 || b != `{"high_precision":false}` {
		t.Fatalf("put off: %d %s", c, b)
	}
	if e.m.HighPrecision(context.Background()) {
		t.Fatal("the manager does not see the setting turned off")
	}
	if c, b := do("PUT", `{"high_precision":true}`); c != 200 || b != `{"high_precision":true}` {
		t.Fatalf("put: %d %s", c, b)
	}
	if !e.m.HighPrecision(context.Background()) {
		t.Fatal("the manager does not see the setting")
	}
	if c, b := do("GET", ""); c != 200 || b != `{"high_precision":true}` {
		t.Fatalf("get after put: %d %s", c, b)
	}
	for _, bad := range []string{``, `{}`, `{"high_precision":"yes"}`, `not json`} {
		if c, _ := do("PUT", bad); c != 400 {
			t.Fatalf("%q: %d", bad, c)
		}
	}
	if c, _ := do("PUT", `{"high_precision":false}`); c != 200 || e.m.HighPrecision(context.Background()) {
		t.Fatal("could not turn it off again")
	}
}

// ---- Apple Music is a first-class source

type appleFake struct {
	*catalogFake
	albums map[string]providers.Album
	calls  int
}

func (a *appleFake) GetAlbum(_ context.Context, id string) (*providers.Album, error) {
	a.calls++
	al, ok := a.albums[id]
	if !ok {
		return nil, providers.ErrNotFound
	}
	return &al, nil
}

const apAlbum = "1700000001"

// appleGorilla: an Apple catalogue holding the same song. Its song date is the
// track's own (wrong for the album), its album date and barcode are right.
func appleGorilla(songDate string, albumUPC, albumDate string) *appleFake {
	return &appleFake{
		catalogFake: &catalogFake{name: providers.AppleMusic, byID: map[string]providers.Track{
			"1800000002": {Provider: providers.AppleMusic, ProviderID: "1800000002", Title: "Gorilla Glue", DurationMs: 184306, ISRCs: []string{hpISRC},
				Artists:  []providers.Artist{{ProviderID: "apCheem", Name: "Cheem"}},
				Releases: []providers.ReleaseRef{{ProviderID: apAlbum, Title: "Power Move", Date: songDate, DateIsTrack: true, Disc: 1, Position: 2}}},
		}},
		albums: map[string]providers.Album{apAlbum: {ProviderID: apAlbum, Title: "Power Move", Date: albumDate, UPC: albumUPC, TotalTracks: 2}},
	}
}

func TestHighPrecisionWorksFromAnAppleSongTheListenNamed(t *testing.T) {
	ap := appleGorilla("2021-01-01", hpUPC, "2025-11-14")
	mb := gorillaMB()
	e := newIDEnv(t, ap, mb)
	e.m.SetHighPrecision(context.Background(), true)
	rec, _ := e.listen(t, identity.Input{Artist: "Cheem", Title: "Gorilla Glue", Album: "Power Move", AppleSongID: "1800000002",
		Credits: []identity.Credit{{Name: "Cheem"}}})
	e.run(t)
	if id, method, status := mbLink(e, t, rec); id != "mbGG" || method != "lookup" || status != "auto" {
		t.Fatalf("Apple-driven album match: %q %q %q", id, method, status)
	}
	if mb.isrcs != 0 || mb.calls != 1 {
		t.Fatalf("ISRC lookups %d (want 0), barcode lookups %d (want 1)", mb.isrcs, mb.calls)
	}
}

// An Apple song we found by ISRC may sit on another edition of the album (Apple
// lists one song per release, all sharing the ISRC). Its barcode and position
// then describe that edition, so it is only used when its album is the one we
// know: here the Apple album is the 2026 Victory Lap edition.
func TestHighPrecisionIgnoresAnAppleAlbumFromAnotherEdition(t *testing.T) {
	e, mb := editionScenario(t, "2026-06-05", 184000)
	if mb.calls != 0 {
		t.Fatalf("asked MusicBrainz for the other edition's barcode (%d times)", mb.calls)
	}
	var id, method string
	e.db.QueryRow(`SELECT provider_id, match_method FROM external_link WHERE entity_type='recording' AND provider='musicbrainz' LIMIT 1`).Scan(&id, &method)
	if id != "" {
		t.Fatalf("linked MusicBrainz %q by %q through an Apple album from another edition", id, method)
	}
}

// Even with the right edition, a track whose duration does not match OUR
// recording is not it: the catalogue and MusicBrainz agreeing with each other is
// not enough when the Apple song was found by ISRC and is a different cut.
func TestHighPrecisionChecksOurOwnDurationToo(t *testing.T) {
	e, mb := editionScenario(t, "2025-11-14", 200000)
	if mb.calls != 1 {
		t.Fatalf("setup: the barcode should have been looked up (%d)", mb.calls)
	}
	var id string
	e.db.QueryRow(`SELECT provider_id FROM external_link WHERE entity_type='recording' AND provider='musicbrainz' LIMIT 1`).Scan(&id)
	if id != "" {
		t.Fatalf("linked MusicBrainz %q although its duration is not our recording's", id)
	}
}

// editionScenario: Spotify holds the listen (ours: 184306 ms, album 2025-11-14)
// but has no barcode; Apple, found by ISRC and so only held on the second run,
// reports an album with albumDate and a song of apDuration; MusicBrainz knows
// that album's barcode and lists the song at the same position and duration.
func editionScenario(t *testing.T, albumDate string, apDuration int) (*idEnv, *barcodeFake) {
	t.Helper()
	const otherUPC = "075679575692"
	ap := appleGorilla("2025-11-14", otherUPC, albumDate)
	apTrack := ap.byID["1800000002"]
	apTrack.DurationMs = apDuration
	ap.byID["1800000002"] = apTrack
	ap.byISRC = map[string]providers.Track{hpISRC: apTrack}
	mb := gorillaMB()
	mb.search = nil // by name MusicBrainz offers nothing: only the album can find it
	mb.releases[otherUPC] = []providers.Release{{ProviderID: "mbOther", Title: "Power Move", Date: albumDate, Barcode: otherUPC,
		Tracks: []providers.ReleaseTrack{{Disc: 1, Position: 2, RecordingID: "mbDupe", Title: "Gorilla Glue", DurationMs: apDuration}}}}
	sp := &albumFake{catalogFake: &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{
		"spGG": {Provider: providers.Spotify, ProviderID: "spGG", Title: "Gorilla Glue", DurationMs: 184306, ISRCs: []string{hpISRC},
			Artists:  []providers.Artist{{ProviderID: "spCheem", Name: "Cheem"}},
			Releases: []providers.ReleaseRef{{ProviderID: hpAlbum, Title: "Power Move", Date: "2025-11-14", Disc: 1, Position: 2}}}}},
		albums: map[string]providers.Album{hpAlbum: {ProviderID: hpAlbum, Title: "Power Move", Date: "2025-11-14"}}}
	e := newIDEnv(t, sp, ap, mb)
	e.m.SetHighPrecision(context.Background(), true)
	e.listen(t, identity.Input{Artist: "Cheem", Title: "Gorilla Glue", Album: "Power Move", SpotifyTrackID: "spGG",
		Credits: []identity.Credit{{Name: "Cheem", SpotifyID: "spCheem"}}})
	e.run(t) // Apple is found by ISRC here
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND provider='apple'`) != 1 {
		t.Fatal("setup: the Apple song was not linked by ISRC")
	}
	if _, err := e.m.RequeueAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.run(t) // now Apple is held, and the album step may use it
	return e, mb
}

// Apple reports a song's own date, not the album's. The normal ladder needs the
// album's, so it asks Apple for the album (once, cached) and chooses between
// MusicBrainz's duplicate recordings by that, and our album stores it.
func TestNormalModeUsesAppleAlbumDateNotTheSongDate(t *testing.T) {
	// The song date equals the date of the *other* edition: using it would pick the duplicate.
	ap := appleGorilla("2026-03-06", hpUPC, "2025-11-14")
	mb := gorillaMB()
	e := newIDEnv(t, ap, mb)
	if err := e.m.SetHighPrecision(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.listen(t, identity.Input{Artist: "Cheem", Title: "Gorilla Glue", Album: "Power Move", AppleSongID: "1800000002",
		Credits: []identity.Credit{{Name: "Cheem"}}})
	e.run(t)
	if id, method, status := mbLink(e, t, rec); id != "mbGG" || status != "auto" || method != "fuzzy" {
		t.Fatalf("want mbGG auto by name + the album's release date; got %q %q %q", id, method, status)
	}
	if ap.calls != 1 {
		t.Fatalf("Apple album lookups %d, want 1", ap.calls)
	}
	var date string
	e.db.QueryRow(`SELECT COALESCE(release_date,'') FROM release r JOIN recording_release rr ON rr.release_id=r.id WHERE rr.recording_id=?`, rec).Scan(&date)
	if date != "2025-11-14" {
		t.Fatalf("our album stored the date %q, want the album's 2025-11-14", date)
	}
}

// An album is fetched once, however many of its recordings are matched.
func TestAnAlbumIsFetchedOncePerAlbum(t *testing.T) {
	e, sp, _ := gorilla(t)
	gorillaListen(t, e) // two recordings on one album
	e.run(t)
	if sp.calls != 1 {
		t.Fatalf("Spotify album lookups %d, want 1", sp.calls)
	}
	if _, err := e.m.RequeueAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if sp.calls != 1 {
		t.Fatalf("a second run asked again (%d): the answer is cached", sp.calls)
	}
}

// When Apple cannot tell us the album's date, a song's own date is not stored
// in its place: an empty date says nothing, a wrong one would mislead.
func TestAppleSongDateIsNeverStoredAsTheAlbumDate(t *testing.T) {
	ap := appleGorilla("2021-01-01", hpUPC, "2025-11-14")
	ap.albums = map[string]providers.Album{} // the album lookup finds nothing
	e := newIDEnv(t, ap, gorillaMB())
	rec, _ := e.listen(t, identity.Input{Artist: "Cheem", Title: "Gorilla Glue", Album: "Power Move", AppleSongID: "1800000002",
		Credits: []identity.Credit{{Name: "Cheem"}}})
	e.run(t)
	var date string
	e.db.QueryRow(`SELECT COALESCE(release_date,'') FROM release r JOIN recording_release rr ON rr.release_id=r.id WHERE rr.recording_id=?`, rec).Scan(&date)
	if date != "" {
		t.Fatalf("stored %q as the album's date", date)
	}
}

// Apple answers cached before DateIsTrack existed carry no flag; reading them
// must still treat the date as the song's.
func TestCachedAppleAnswersWithoutTheFlagAreStillMarked(t *testing.T) {
	ap := appleGorilla("2026-03-06", hpUPC, "2025-11-14")
	e := newIDEnv(t, ap, gorillaMB())
	old := `{"Provider":"apple","ProviderID":"1800000002","Title":"Gorilla Glue","DurationMs":184306,
		"Artists":[{"ProviderID":"apCheem","Name":"Cheem"}],
		"Releases":[{"ProviderID":"` + apAlbum + `","Title":"Power Move","Date":"2026-03-06","Disc":1,"Position":2}]}`
	if err := e.m.snaps.Put(context.Background(), providers.AppleMusic, "1800000002", snapshot.KindRecording, []byte(old), 0); err != nil {
		t.Fatal(err)
	}
	tr, err := e.m.getTrack(context.Background(), ap, "1800000002")
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Releases[0].DateIsTrack {
		t.Fatal("a cached Apple answer's date was taken for the album's")
	}
	if ap.gets != 0 {
		t.Fatal("setup: the answer should have come from the cache")
	}
}

// ---- UPC: Spotify's and Apple Music's albums link when they are the same release

const (
	upcA     = "199891213535"
	upcB     = "075679575692"
	spAlb    = "spAlbumX"
	apAlbum2 = "1900000001"
)

// streamers: a Spotify listen on "Album" (barcode spUPC), and Apple, found by
// ISRC, reporting an album titled apTitle with barcode apUPC. It returns our
// release of the recording.
func streamers(t *testing.T, spUPC, apTitle, apUPC string, tweak ...func(sp *albumFake, ap *appleFake)) (*idEnv, int64) {
	t.Helper()
	sp := &albumFake{
		catalogFake: &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{
			"spS": {Provider: providers.Spotify, ProviderID: "spS", Title: "Song", DurationMs: 200000, ISRCs: []string{"USAAA1111111"},
				Artists:  []providers.Artist{{ProviderID: "spA", Name: "Artist"}},
				Releases: []providers.ReleaseRef{{ProviderID: spAlb, Title: "Album", Date: "2020-01-02", Disc: 1, Position: 1}}}}},
		albums: map[string]providers.Album{spAlb: {ProviderID: spAlb, Title: "Album", Date: "2020-01-02", UPC: spUPC}},
	}
	apSong := providers.Track{Provider: providers.AppleMusic, ProviderID: "apS", Title: "Song", DurationMs: 200000, ISRCs: []string{"USAAA1111111"},
		Artists:  []providers.Artist{{ProviderID: "apA", Name: "Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: apAlbum2, Title: apTitle, Date: "2020-01-02", DateIsTrack: true, Disc: 1, Position: 1}}}
	ap := &appleFake{
		catalogFake: &catalogFake{name: providers.AppleMusic, byID: map[string]providers.Track{"apS": apSong}, byISRC: map[string]providers.Track{"USAAA1111111": apSong}},
		albums:      map[string]providers.Album{apAlbum2: {ProviderID: apAlbum2, Title: apTitle, Date: "2020-01-02", UPC: apUPC}},
	}
	for _, f := range tweak {
		f(sp, ap)
	}
	e := newIDEnv(t, sp, ap)
	_, rel := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Album", SpotifyTrackID: "spS", SpotifyAlbumID: spAlb,
		Credits: []identity.Credit{{Name: "Artist", SpotifyID: "spA"}}})
	return e, rel
}

func relLinks(e *idEnv, t *testing.T, rel int64, provider string) int {
	return e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='release' AND entity_id=? AND provider=? AND status<>'candidate'`, rel, provider)
}

// The same release under two titles ("Album" and "Album - Single") has one
// barcode, and that is what links Apple's album to ours.
func TestSameBarcodeLinksApplesAlbumToOurReleaseWhateverItIsCalled(t *testing.T) {
	e, rel := streamers(t, upcA, "Album - Single", "0"+upcA) // UPC-A on one side, EAN-13 on the other
	e.run(t)
	if relLinks(e, t, rel, "spotify") != 1 || relLinks(e, t, rel, "apple") != 1 {
		t.Fatalf("album links: spotify %d, apple %d", relLinks(e, t, rel, "spotify"), relLinks(e, t, rel, "apple"))
	}
	var upc, method string
	e.db.QueryRow(`SELECT provider_id, match_method FROM external_link WHERE entity_type='release' AND entity_id=? AND provider='upc'`, rel).Scan(&upc, &method)
	if upc != providers.NormalizeBarcode(upcA) || method != "lookup" || relLinks(e, t, rel, "upc") != 1 {
		t.Fatalf("barcode link %q %q (count %d)", upc, method, relLinks(e, t, rel, "upc"))
	}
}

// Equal titles are not enough: a different barcode is a different edition.
func TestDifferentBarcodeKeepsApplesAlbumOffOurRelease(t *testing.T) {
	e, rel := streamers(t, upcA, "Album", upcB)
	e.run(t)
	if relLinks(e, t, rel, "apple") != 0 {
		t.Fatal("Apple's album, another edition by barcode, was attached because its title matched")
	}
	if relLinks(e, t, rel, "spotify") != 1 || relLinks(e, t, rel, "upc") != 1 {
		t.Fatal("our own album's links were lost")
	}
	// The other edition's date and title are not merged into ours.
	if e.n(t, `SELECT COUNT(*) FROM alias WHERE kind='release' AND entity_id=?`, rel) > 1 {
		t.Fatal("the other edition's title became an alias of our release")
	}
}

// A wrong edition link made before barcodes were known is removed once its
// barcode shows it; a link a person confirmed stays.
func TestAWrongEditionLinkIsRepairedButAConfirmedOneIsKept(t *testing.T) {
	for name, c := range map[string]struct {
		status string
		pinned int
		kept   bool
	}{"auto": {"auto", 0, false}, "pinned": {"auto", 1, true}, "confirmed": {"confirmed", 0, true}} {
		e, rel := streamers(t, upcA, "Album", upcB)
		if _, err := e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, status, pinned)
			VALUES ('release', ?, 'apple', ?, 'lookup', ?, ?)`, rel, apAlbum2, c.status, c.pinned); err != nil {
			t.Fatal(err)
		}
		e.run(t)
		if got := relLinks(e, t, rel, "apple") == 1; got != c.kept {
			t.Fatalf("%s: link kept = %v, want %v", name, got, c.kept)
		}
	}
}

// A barcode held by another release of ours means this album is that release.
func TestABarcodeHeldByAnotherReleaseIsNotAttachedToThisOne(t *testing.T) {
	e, rel := streamers(t, upcA, "Album", upcB)
	// Another release of ours already holds Apple's barcode.
	var other int64
	e.db.QueryRow(`INSERT INTO release (title) VALUES ('Compilation') RETURNING id`).Scan(&other)
	e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, status) VALUES ('release', ?, 'upc', ?, 'lookup', 'auto')`, other, providers.NormalizeBarcode(upcB))
	e.run(t)
	if relLinks(e, t, rel, "apple") != 0 || relLinks(e, t, other, "apple") != 0 {
		t.Fatal("Apple's album was attached to a release it is not")
	}
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE kind='release'`) != 0 {
		t.Fatal("a recording on two releases was flagged as a duplicate release")
	}
}

// With no barcode to go by (the catalogue could not be asked), the old rules
// apply: an equal title attaches.
func TestWithoutABarcodeAlbumsLinkTheOldWay(t *testing.T) {
	e, rel := streamers(t, upcA, "Album", "")
	e.run(t)
	if relLinks(e, t, rel, "apple") != 1 {
		t.Fatal("an equal title no longer attaches when Apple has no barcode")
	}
	if relLinks(e, t, rel, "upc") != 1 {
		t.Fatal("Spotify's barcode should still be recorded")
	}
}

func withTracks(sp, ap int, apDate string) func(*albumFake, *appleFake) {
	return func(s *albumFake, a *appleFake) {
		x := s.albums[spAlb]
		x.TotalTracks = sp
		s.albums[spAlb] = x
		y := a.albums[apAlbum2]
		y.TotalTracks = ap
		if apDate != "" {
			y.Date = apDate
		}
		a.albums[apAlbum2] = y
	}
}

// Catalogues sometimes sell one release under two barcodes. The same release
// date and the same track count say it is one release: Apple's album links, and
// its barcode is recorded as our release's second.
func TestAnotherBarcodeWithTheSameDateAndTrackCountIsTheSameRelease(t *testing.T) {
	e, rel := streamers(t, upcA, "Album - Single", upcB, withTracks(10, 10, ""))
	e.run(t)
	if relLinks(e, t, rel, "apple") != 1 {
		t.Fatal("the same release under a second barcode was treated as another edition")
	}
	if relLinks(e, t, rel, "upc") != 2 {
		t.Fatalf("our release should hold both barcodes, has %d", relLinks(e, t, rel, "upc"))
	}
}

// Another edition differs in the date or the track count, whatever the title.
func TestAnotherBarcodeWithADifferentDateOrTrackCountIsAnotherEdition(t *testing.T) {
	for name, tweak := range map[string]func(*albumFake, *appleFake){
		"different track count": withTracks(10, 11, ""),
		"different date":        withTracks(10, 10, "2026-06-05"),
		"no track count":        withTracks(0, 0, ""),
	} {
		e, rel := streamers(t, upcA, "Album", upcB, tweak)
		e.run(t)
		if relLinks(e, t, rel, "apple") != 0 || relLinks(e, t, rel, "upc") != 1 {
			t.Fatalf("%s: Apple's album was taken for the same release (apple %d, barcodes %d)", name, relLinks(e, t, rel, "apple"), relLinks(e, t, rel, "upc"))
		}
	}
}

// ---- an artist's ids are never merged with another artist's

// The shape of recording 38: our artist "SEGA SOUND TEAM" wrongly holds the
// Spotify id of 大谷智哉 (and his name as an alias, from an old wrong pairing),
// and Spotify credits the recording to both people. Two Spotify ids are two
// artists whatever their names, so the credits must stay two artists and each id
// must end up on one artist.
func TestTwoCreditsWithDifferentIDsAreNeverMergedIntoOneArtist(t *testing.T) {
	track := providers.Track{
		Provider: providers.Spotify, ProviderID: "spCyber", Title: "Cyber Space",
		Artists: []providers.Artist{{ProviderID: "spTeam", Name: "SEGA SOUND TEAM"}, {ProviderID: "spOhtani", Name: "大谷智哉"}},
	}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spCyber": track}}
	e := newIDEnv(t, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "SEGA SOUND TEAM", Title: "Cyber Space", SpotifyTrackID: "spCyber",
		Credits: []identity.Credit{{Name: "SEGA SOUND TEAM"}}})
	var poisoned int64
	e.db.QueryRow(`SELECT id FROM artist WHERE name='SEGA SOUND TEAM'`).Scan(&poisoned)
	// The old damage: someone else's id and name on our artist.
	e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, status) VALUES ('artist', ?, 'spotify', 'spOhtani', 'lookup', 'auto')`, poisoned)
	e.db.Exec(`INSERT INTO alias (kind, raw_string, entity_id) VALUES ('artist', '大谷智哉', ?)`, poisoned)
	e.run(t)

	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 2 {
		t.Fatal("the two credits were collapsed into one artist")
	}
	var team int64
	e.db.QueryRow(`SELECT entity_id FROM external_link WHERE entity_type='artist' AND provider='spotify' AND provider_id='spTeam'`).Scan(&team)
	if team == 0 || team == poisoned {
		t.Fatalf("SEGA SOUND TEAM's own Spotify id must attach to an artist of its own, not to artist %d that holds another id", team)
	}
	for _, id := range []string{"spTeam", "spOhtani"} {
		if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND provider='spotify' AND provider_id=?`, id) != 1 {
			t.Fatalf("id %s is not on exactly one artist", id)
		}
	}
}

// An artist listed twice by a provider (same id) is one credit, as before.
func TestTheSameArtistListedTwiceIsOneCredit(t *testing.T) {
	track := providers.Track{Provider: providers.Spotify, ProviderID: "spDup", Title: "Song",
		Artists: []providers.Artist{{ProviderID: "spA", Name: "Alice"}, {ProviderID: "spA", Name: "Alice"}, {ProviderID: "spB", Name: "Bob"}}}
	sp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spDup": track}}
	e := newIDEnv(t, sp)
	rec, _ := e.listen(t, identity.Input{Artist: "Alice", Title: "Song", SpotifyTrackID: "spDup", Credits: []identity.Credit{{Name: "Alice"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec) != 2 {
		t.Fatalf("%d credits, want 2 (Alice once, Bob)", e.n(t, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec))
	}
}

// A provider's artist that conflicts with ours (we hold another id of that
// provider) gives our artist nothing: not the id, not its spelling as an alias,
// not verification. An alias like that is how a wrong pairing becomes permanent.
func TestAConflictingPairGivesOurArtistNoAliasAndNoId(t *testing.T) {
	e := newIDEnv(t)
	rec, _ := e.listen(t, identity.Input{Artist: "Foo", Title: "Song",
		Credits: []identity.Credit{{Name: "Foo", SpotifyID: "spFoo"}}})
	track := providers.Track{Provider: providers.Spotify, ProviderID: "spT3", Title: "Song",
		Artists: []providers.Artist{{ProviderID: "spBar", Name: "Bar"}}}
	tx, err := e.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// exact: the one artist on each side would be paired by position.
	if err := applyTrack(context.Background(), tx, rec, &track, true, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if e.n(t, `SELECT COUNT(*) FROM alias a JOIN artist r ON r.id=a.entity_id WHERE a.kind='artist' AND r.name='Foo' AND a.raw_string='bar'`) != 0 {
		t.Fatal("Bar's name became an alias of Foo")
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND provider='spotify' AND provider_id='spBar'`) != 0 {
		t.Fatal("Bar's Spotify id was put on Foo")
	}
}

// ---- a fuller list from another catalogue adds its extra artists

func extraArtistsEnv(t *testing.T, mbArtists, spArtists []providers.Artist) (*idEnv, int64) {
	t.Helper()
	// Equal durations make the ISRC link automatic whatever the artists look like.
	mb := providers.Track{Provider: providers.MusicBrainz, ProviderID: "mbX", Title: "Song", DurationMs: 200000, ISRCs: []string{"USEEE0000001"}, Artists: mbArtists}
	sp := providers.Track{Provider: providers.Spotify, ProviderID: "spX", Title: "Song", DurationMs: 200000, ISRCs: []string{"USEEE0000001"}, Artists: spArtists}
	mbp := &catalogFake{name: providers.MusicBrainz, byID: map[string]providers.Track{"mbX": mb}}
	spp := &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spX": sp}, byISRC: map[string]providers.Track{"USEEE0000001": sp}}
	e := newIDEnv(t, mbp, spp)
	// The listen names a MusicBrainz recording only: MusicBrainz sets the credits.
	rec, _ := e.listen(t, identity.Input{Artist: "Alice", Title: "Song", RecordingMBID: "mbX", Credits: []identity.Credit{{Name: "Alice"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='spotify' AND status='auto'`, rec) != 1 {
		t.Fatal("setup: the Spotify list was not applied (the ISRC link must be automatic)")
	}
	return e, rec
}

func creditNames(e *idEnv, t *testing.T, rec int64) []string {
	t.Helper()
	rows, err := e.db.Query(`SELECT a.name FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=? ORDER BY ra.position`, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

// MusicBrainz lists the main artist alone and names the recording; Spotify,
// found by ISRC, lists two: the second is added after the first.
func TestAFullerListFromAnotherCatalogueAddsItsExtraArtists(t *testing.T) {
	e, rec := extraArtistsEnv(t,
		[]providers.Artist{{ProviderID: "mbA", Name: "Alice"}},
		[]providers.Artist{{ProviderID: "spA", Name: "Alice"}, {ProviderID: "spB", Name: "Bob"}})
	got := creditNames(e, t, rec)
	if len(got) != 2 || got[0] != "Alice" || got[1] != "Bob" {
		t.Fatalf("credits %v, want [Alice Bob] with ours first", got)
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link l JOIN artist a ON a.id=l.entity_id WHERE l.entity_type='artist' AND a.name='Bob' AND l.provider='spotify' AND l.provider_id='spB'`) != 1 {
		t.Fatal("the added artist did not get its Spotify id")
	}
	// A second run changes nothing.
	if _, err := e.m.RequeueAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if got := creditNames(e, t, rec); len(got) != 2 {
		t.Fatalf("a second run changed the credits: %v", got)
	}
}

// A list that does not contain every artist we hold is not plainly this
// recording's credits (a different recording, or a different artist's): it adds
// nothing, however many artists it names.
func TestAListThatLacksOneOfOursAddsNothing(t *testing.T) {
	e, rec := extraArtistsEnv(t,
		[]providers.Artist{{ProviderID: "mbA", Name: "Alice"}},
		[]providers.Artist{{ProviderID: "spZ", Name: "Zed"}, {ProviderID: "spB", Name: "Bob"}})
	if got := creditNames(e, t, rec); len(got) != 1 || got[0] != "Alice" {
		t.Fatalf("credits %v: a list without Alice added artists", got)
	}
}

// A list that names no more artists than we hold adds nothing.
func TestAListThatIsNotLongerAddsNothing(t *testing.T) {
	e, rec := extraArtistsEnv(t,
		[]providers.Artist{{ProviderID: "mbA", Name: "Alice"}, {ProviderID: "mbB", Name: "Bob"}},
		[]providers.Artist{{ProviderID: "spA", Name: "Alice"}})
	if got := creditNames(e, t, rec); len(got) != 2 {
		t.Fatalf("credits %v", got)
	}
}

// Extras are never paired by position: a different name does not count as ours.
func TestExtraArtistsAreNotAddedWhenOursOnlyMatchesByPosition(t *testing.T) {
	e, rec := extraArtistsEnv(t,
		[]providers.Artist{{ProviderID: "mbA", Name: "Alice"}},
		[]providers.Artist{{ProviderID: "spAl", Name: "Alicia"}, {ProviderID: "spB", Name: "Bob"}})
	if got := creditNames(e, t, rec); len(got) != 1 {
		t.Fatalf("credits %v: Alicia was taken for Alice by position", got)
	}
}

// A recording edited by hand keeps its artists.
func TestHandEditedRecordingGetsNoExtraArtists(t *testing.T) {
	mb := providers.Track{Provider: providers.MusicBrainz, ProviderID: "mbX", Title: "Song", ISRCs: []string{"USEEE0000001"},
		Artists: []providers.Artist{{ProviderID: "mbA", Name: "Alice"}}}
	sp := providers.Track{Provider: providers.Spotify, ProviderID: "spX", Title: "Song", ISRCs: []string{"USEEE0000001"},
		Artists: []providers.Artist{{ProviderID: "spA", Name: "Alice"}, {ProviderID: "spB", Name: "Bob"}}}
	e := newIDEnv(t, &catalogFake{name: providers.MusicBrainz, byID: map[string]providers.Track{"mbX": mb}},
		&catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spX": sp}, byISRC: map[string]providers.Track{"USEEE0000001": sp}})
	rec, _ := e.listen(t, identity.Input{Artist: "Alice", Title: "Song", RecordingMBID: "mbX", Credits: []identity.Credit{{Name: "Alice"}}})
	if _, err := e.db.Exec(`INSERT INTO edit_log (actor, entity_type, entity_id, scope) VALUES ('admin','recording',?, 'entity')`, rec); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if got := creditNames(e, t, rec); len(got) != 1 {
		t.Fatalf("credits %v: a hand-edited recording was extended", got)
	}
}

// ---- gaps are filled: a recording with no release, plays with no release

const (
	gapUPC   = "199891213599"
	gapAlbum = "spAlbumGap"
)

func gapSpotify(recs ...string) *albumFake {
	byID := map[string]providers.Track{}
	for i, id := range recs {
		byID[id] = providers.Track{Provider: providers.Spotify, ProviderID: id, Title: "Gap Song " + id, DurationMs: 200000,
			Artists: []providers.Artist{{ProviderID: "spGapArtist", Name: "Gap Artist"}},
			Releases: []providers.ReleaseRef{{ProviderID: gapAlbum, Title: "Gap Album", Date: "2021-05-05", Disc: 1, Position: i + 1,
				Artists: []providers.Artist{{ProviderID: "spGapArtist", Name: "Gap Artist"}}}}}
	}
	return &albumFake{catalogFake: &catalogFake{name: providers.Spotify, byID: byID},
		albums: map[string]providers.Album{gapAlbum: {ProviderID: gapAlbum, Title: "Gap Album", Date: "2021-05-05", UPC: gapUPC, TotalTracks: 2}}}
}

func play(e *idEnv, t *testing.T, rec int64, album string) int64 {
	t.Helper()
	var alb any
	if album != "" {
		alb = album
	}
	res, err := e.db.Exec(`INSERT INTO scrobble (played_at, recording_id, raw_artist, raw_title, raw_album, source)
		VALUES ('2026-10-17T10:00:00.000Z', ?, 'Gap Artist', 'Gap Song', ?, 'import:applemusic')`, rec, alb)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func relOf(e *idEnv, scrobble int64) (rel sql.NullInt64) {
	e.db.QueryRow(`SELECT release_id FROM scrobble WHERE id=?`, scrobble).Scan(&rel)
	return
}

// The Apple Play Activity case: the listen names a catalogue id but no album.
// The catalogue's album becomes the recording's release, with everything the
// catalogue knows about it, and the plays that had no release get it.
func TestARecordingWithNoReleaseGetsTheAlbumTheCatalogueNames(t *testing.T) {
	e := newIDEnv(t, gapSpotify("spG1"))
	rec, rel := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song spG1", SpotifyTrackID: "spG1", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	if rel != 0 {
		t.Fatal("setup: the listen carries no album")
	}
	bare := play(e, t, rec, "")
	own := play(e, t, rec, "Somewhere Else") // names an album of its own: never reassigned
	e.run(t)

	var relID int64
	var title, date string
	e.db.QueryRow(`SELECT r.id, r.title, COALESCE(r.release_date,'') FROM recording_release rr JOIN release r ON r.id=rr.release_id WHERE rr.recording_id=? AND rr.is_primary=1`, rec).Scan(&relID, &title, &date)
	if title != "Gap Album" || date != "2021-05-05" {
		t.Fatalf("release %q %q: the catalogue's album was not created", title, date)
	}
	if relLinks(e, t, relID, "spotify") != 1 || relLinks(e, t, relID, "upc") != 1 {
		t.Fatal("the album's Spotify id and barcode were not attached")
	}
	if e.n(t, `SELECT COUNT(*) FROM release_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.release_id=? AND a.name='Gap Artist'`, relID) != 1 {
		t.Fatal("the album artist was not filled")
	}
	if got := relOf(e, bare); !got.Valid || got.Int64 != relID {
		t.Fatalf("a play with no album got release %v, want %d", got, relID)
	}
	if got := relOf(e, own); got.Valid {
		t.Fatal("a play that names its own album was reassigned")
	}
	// A second run changes nothing.
	if _, err := e.m.RequeueAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM release`) != 1 {
		t.Fatalf("%d releases after a second run", e.n(t, `SELECT COUNT(*) FROM release`))
	}
}

// Two recordings of one album share one release; the second finds it by barcode.
func TestTwoRecordingsOfOneAlbumShareTheReleaseThatWasCreated(t *testing.T) {
	e := newIDEnv(t, gapSpotify("spG1", "spG2"))
	r1, _ := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song spG1", SpotifyTrackID: "spG1", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	r2, _ := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song spG2", SpotifyTrackID: "spG2", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM release`) != 1 {
		t.Fatalf("%d releases: the album was created twice", e.n(t, `SELECT COUNT(*) FROM release`))
	}
	if e.n(t, `SELECT COUNT(DISTINCT release_id) FROM recording_release WHERE recording_id IN (?,?)`, r1, r2) != 1 {
		t.Fatal("the two recordings are not on the same release")
	}
}

// An album we only found by ISRC still fills the gap (an album is better than
// none), but an id the listen named wins when both name one.
func TestAnAlbumFoundByISRCFillsAGapButAnIDTheListenNamedWinsOverIt(t *testing.T) {
	apSong := providers.Track{Provider: providers.AppleMusic, ProviderID: "apG", Title: "Gap Song", DurationMs: 200000, ISRCs: []string{"USGAP0000001"},
		Artists:  []providers.Artist{{ProviderID: "apGapArtist", Name: "Gap Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "1990000001", Title: "Apple Album - EP", Date: "2021-05-05", DateIsTrack: true, Disc: 1, Position: 1}}}
	ap := &appleFake{catalogFake: &catalogFake{name: providers.AppleMusic, byID: map[string]providers.Track{"apG": apSong}, byISRC: map[string]providers.Track{"USGAP0000001": apSong}},
		albums: map[string]providers.Album{"1990000001": {ProviderID: "1990000001", Title: "Apple Album - EP", Date: "2021-05-05", UPC: "075679575699"}}}
	// Found by ISRC only: the Apple album fills the gap.
	e := newIDEnv(t, ap)
	rec, _ := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song", ISRC: "USGAP0000001", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM recording_release rr JOIN release r ON r.id=rr.release_id WHERE rr.recording_id=? AND r.title='Apple Album - EP'`, rec) != 1 {
		t.Fatal("an album found by ISRC did not fill the gap")
	}
	// Both: Spotify's id was named by the listen, so its album is the one.
	sp := gapSpotify("spG1")
	t2 := sp.byID["spG1"]
	t2.Title, t2.ISRCs = "Gap Song", []string{"USGAP0000001"}
	sp.byID["spG1"] = t2
	e2 := newIDEnv(t, sp, ap)
	rec2, _ := e2.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song", SpotifyTrackID: "spG1", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	e2.run(t)
	if e2.n(t, `SELECT COUNT(*) FROM recording_release rr JOIN release r ON r.id=rr.release_id WHERE rr.recording_id=? AND r.title='Gap Album' AND rr.is_primary=1`, rec2) != 1 {
		t.Fatal("the album of the id the listen named is not the primary release")
	}
	if e2.n(t, `SELECT COUNT(*) FROM release`) != 1 {
		t.Fatalf("%d releases: the other catalogue's edition was added as a second one", e2.n(t, `SELECT COUNT(*) FROM release`))
	}
}

// A recording that already has a release keeps it, and its plays with no album
// get it (Nocturnal's 23 Apple plays).
func TestPlaysWithNoAlbumGetTheRecordingsPrimaryRelease(t *testing.T) {
	e := newIDEnv(t, gapSpotify("spG1"))
	rec, rel := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song spG1", Album: "Gap Album", SpotifyTrackID: "spG1", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	if rel == 0 {
		t.Fatal("setup: the listen names an album")
	}
	bare := play(e, t, rec, "")
	e.run(t)
	if got := relOf(e, bare); !got.Valid || got.Int64 != rel {
		t.Fatalf("play release %v, want %d", got, rel)
	}
	if e.n(t, `SELECT COUNT(*) FROM release`) != 1 {
		t.Fatal("a second release was created for a recording that already had one")
	}
}

// A recording a person edited is left alone: they may have removed a release on purpose.
func TestAHandEditedRecordingGetsNoReleaseCreated(t *testing.T) {
	e := newIDEnv(t, gapSpotify("spG1"))
	rec, _ := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song spG1", SpotifyTrackID: "spG1", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	bare := play(e, t, rec, "")
	if _, err := e.db.Exec(`INSERT INTO edit_log (actor, entity_type, entity_id, scope) VALUES ('admin','recording',?, 'entity')`, rec); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM release`) != 0 || relOf(e, bare).Valid {
		t.Fatal("enrichment put a hand-edited recording back on a release")
	}
}

// ---- the same album under two spellings: merged when the barcode ties them

// Nocturnal: a Spotify listen made release "Nocturnal"; an Apple listen made
// "Nocturnal - Single". The Apple album (same barcode as Spotify's) is attached to
// the first, and the second, which has the album's own title, is marked for the
// automatic merge. Without a merger it stays a flag.
func TestTheSameAlbumUnderAppleSuffixIsMarkedForMerge(t *testing.T) {
	e, rel := streamers(t, upcA, "Album - Single", upcA, withTracks(1, 1, ""))
	_, variant := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Album - Single", Credits: []identity.Credit{{Name: "Artist"}}})
	if variant == 0 || variant == rel {
		t.Fatalf("setup: the Apple spelling should make a second release (%d, %d)", rel, variant)
	}
	e.db.Exec(`INSERT INTO artist (id, name) VALUES (9001, 'Album Artist')`)
	e.db.Exec(`INSERT INTO release_artist (release_id, artist_id, position) VALUES (?, 9001, 0)`, rel)
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM release_artist WHERE release_id=? AND artist_id=9001`, variant) != 1 {
		t.Fatal("the variant did not get the album artist of the release it is a variant of")
	}
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE kind='release' AND reason=? AND ((entity_id=? AND other_id=?) OR (entity_id=? AND other_id=?))`, ReasonBarcodeVariant, rel, variant, variant, rel) != 1 {
		t.Fatal("the two spellings of one album were not marked for merging")
	}
	if e.n(t, `SELECT COUNT(*) FROM release`) != 2 {
		t.Fatal("nothing should merge without a merger")
	}
}

// With the editor's merge wired in, the Apple spelling folds into the release
// holding the barcode: its plays move, and its name becomes an alias so later
// Apple imports land on the same release.
func TestTheSameAlbumUnderAppleSuffixIsMerged(t *testing.T) {
	e, rel := streamers(t, upcA, "Album - Single", upcA, withTracks(1, 1, ""))
	rec, variant := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Album - Single", Credits: []identity.Credit{{Name: "Artist"}}})
	p := play(e, t, rec, "")
	e.db.Exec(`UPDATE scrobble SET release_id=? WHERE id=?`, variant, p)
	e.m.o.Merge = func(ctx context.Context, kind string, from, into int64) error {
		_, err := (&edit.Editor{DB: e.db}).Merge(ctx, "matching", kind, from, into)
		return err
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM release WHERE id=?`, variant) != 0 || e.n(t, `SELECT COUNT(*) FROM release WHERE id=?`, rel) != 1 {
		t.Fatal("the Apple spelling was not merged into the release holding the barcode")
	}
	if got := relOf(e, p); !got.Valid || got.Int64 != rel {
		t.Fatalf("the play's release is %v, want %d", got, rel)
	}
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE kind='release'`) != 0 {
		t.Fatal("the settled pair is still flagged")
	}
	if e.n(t, `SELECT COUNT(*) FROM alias WHERE kind='release' AND entity_id=?`, rel) == 0 {
		t.Fatal("the merged name did not become an alias")
	}
}

// Another edition with the same name holds another barcode: it is not a variant.
func TestAnotherEditionWithTheSameTitleIsNotFlagged(t *testing.T) {
	e := newIDEnv(t)
	tx, _ := e.db.Begin()
	var a, b int64
	tx.QueryRow(`INSERT INTO release (title) VALUES ('X') RETURNING id`).Scan(&a)
	tx.QueryRow(`INSERT INTO release (title) VALUES ('X - Single') RETURNING id`).Scan(&b)
	tx.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, status) VALUES ('release', ?, 'upc', ?, 'lookup','auto')`, b, providers.NormalizeBarcode(upcB))
	albums := map[string]providers.Album{"apple/1": {ProviderID: "1", UPC: upcA, TotalTracks: 3, Date: "2020-01-01"}}
	err := flagSameAlbumVariants(context.Background(), tx, a, []ourRelease{{a, "X"}, {b, "X - Single"}}, "X - Single", providers.NormalizeBarcode(upcA), albums["apple/1"], albums)
	tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE kind='release'`) != 0 {
		t.Fatal("an edition with a different barcode was flagged as a variant")
	}
}

// The album that fills a gap is found by barcode even when its id and its title
// are different: Apple's "Gap Album - EP" is the Spotify release "Gap Album".
func TestAGapIsFilledFromTheReleaseThatHoldsTheAlbumsBarcode(t *testing.T) {
	sp := gapSpotify("spG1")
	apSong := providers.Track{Provider: providers.AppleMusic, ProviderID: "apG2", Title: "Other Song", DurationMs: 190000,
		Artists:  []providers.Artist{{ProviderID: "apGapArtist", Name: "Gap Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "1990000002", Title: "Gap Album - EP", Date: "2021-05-05", DateIsTrack: true, Disc: 1, Position: 2}}}
	ap := &appleFake{catalogFake: &catalogFake{name: providers.AppleMusic, byID: map[string]providers.Track{"apG2": apSong}},
		albums: map[string]providers.Album{"1990000002": {ProviderID: "1990000002", Title: "Gap Album - EP", Date: "2021-05-05", UPC: gapUPC, TotalTracks: 2}}}
	e := newIDEnv(t, sp, ap)
	r1, _ := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song spG1", SpotifyTrackID: "spG1", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	r2, _ := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Other Song", AppleSongID: "apG2", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM release`) != 1 {
		t.Fatalf("%d releases: the same barcode made a second release", e.n(t, `SELECT COUNT(*) FROM release`))
	}
	if e.n(t, `SELECT COUNT(DISTINCT release_id) FROM recording_release WHERE recording_id IN (?,?)`, r1, r2) != 1 {
		t.Fatal("the two recordings are not on the same release")
	}
}

// The id the listen named decides the album even when another catalogue would
// rank first by default: here Apple's song was named, and Spotify's is only found
// by ISRC (and may be on another edition).
func TestTheAlbumOfTheIDTheListenNamedBeatsOneFoundByISRCEvenFromAHigherRankedCatalogue(t *testing.T) {
	apSong := providers.Track{Provider: providers.AppleMusic, ProviderID: "apT", Title: "Gap Song", DurationMs: 200000, ISRCs: []string{"USGAP0000002"},
		Artists:  []providers.Artist{{ProviderID: "apGapArtist", Name: "Gap Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "1990000003", Title: "Named Album", Date: "2021-05-05", DateIsTrack: true, Disc: 1, Position: 1}}}
	ap := &appleFake{catalogFake: &catalogFake{name: providers.AppleMusic, byID: map[string]providers.Track{"apT": apSong}},
		albums: map[string]providers.Album{"1990000003": {ProviderID: "1990000003", Title: "Named Album", Date: "2021-05-05", UPC: "075679575611"}}}
	spSong := providers.Track{Provider: providers.Spotify, ProviderID: "spT", Title: "Gap Song", DurationMs: 200000, ISRCs: []string{"USGAP0000002"},
		Artists:  []providers.Artist{{ProviderID: "spGapArtist", Name: "Gap Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "spOtherAlbum", Title: "Compilation Of Hits", Date: "2023-01-01", Disc: 1, Position: 7}}}
	sp := &albumFake{catalogFake: &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{"spT": spSong}, byISRC: map[string]providers.Track{"USGAP0000002": spSong}},
		albums: map[string]providers.Album{"spOtherAlbum": {ProviderID: "spOtherAlbum", Title: "Compilation Of Hits", Date: "2023-01-01", UPC: "199891219988"}}}
	e := newIDEnv(t, sp, ap)
	rec, _ := e.listen(t, identity.Input{Artist: "Gap Artist", Title: "Gap Song", AppleSongID: "apT", Credits: []identity.Credit{{Name: "Gap Artist"}}})
	e.run(t)
	var title string
	e.db.QueryRow(`SELECT r.title FROM recording_release rr JOIN release r ON r.id=rr.release_id WHERE rr.recording_id=? AND rr.is_primary=1`, rec).Scan(&title)
	if title != "Named Album" {
		t.Fatalf("primary release %q, want the album of the Apple id the listen named", title)
	}
}

// A song's own date is never stored as the album's; the album's own date is, when
// the catalogue gave it (Apple's album lookup does).
func TestTheAlbumsOwnDateFillsTheReleaseWhenTheTrackCarriesOnlyTheSongDate(t *testing.T) {
	ap := appleGorilla("2019-01-01", hpUPC, "2025-11-14") // song date 2019, album date 2025
	e := newIDEnv(t, ap, gorillaMB())
	rec, _ := e.listen(t, identity.Input{Artist: "Cheem", Title: "Gorilla Glue", AppleSongID: "1800000002", Credits: []identity.Credit{{Name: "Cheem"}}})
	e.run(t) // no album on the listen: the release is created from Apple's
	var date string
	e.db.QueryRow(`SELECT COALESCE(r.release_date,'') FROM recording_release rr JOIN release r ON r.id=rr.release_id WHERE rr.recording_id=?`, rec).Scan(&date)
	if date != "2025-11-14" {
		t.Fatalf("release date %q, want the album's 2025-11-14 (not the song's 2019-01-01)", date)
	}
}

// Found by ISRC (not named by the listen), the Apple song carries only its own
// date; the album's, from the album lookup, is what the release stores.
func TestAnAlbumFoundByISRCStoresTheAlbumsDateNotTheSongs(t *testing.T) {
	song := providers.Track{Provider: providers.AppleMusic, ProviderID: "apD", Title: "Date Song", DurationMs: 200000, ISRCs: []string{"USDAT0000001"},
		Artists:  []providers.Artist{{ProviderID: "apDA", Name: "Date Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "1990000009", Title: "Date Album", Date: "2019-01-01", DateIsTrack: true, Disc: 1, Position: 1}}}
	ap := &appleFake{catalogFake: &catalogFake{name: providers.AppleMusic, byID: map[string]providers.Track{"apD": song}, byISRC: map[string]providers.Track{"USDAT0000001": song}},
		albums: map[string]providers.Album{"1990000009": {ProviderID: "1990000009", Title: "Date Album", Date: "2025-11-14", UPC: "075679575633"}}}
	e := newIDEnv(t, ap)
	rec, _ := e.listen(t, identity.Input{Artist: "Date Artist", Title: "Date Song", ISRC: "USDAT0000001", Credits: []identity.Credit{{Name: "Date Artist"}}})
	e.run(t)
	var date string
	e.db.QueryRow(`SELECT COALESCE(r.release_date,'') FROM recording_release rr JOIN release r ON r.id=rr.release_id WHERE rr.recording_id=?`, rec).Scan(&date)
	if date != "2025-11-14" {
		t.Fatalf("release date %q, want the album's 2025-11-14", date)
	}
}

// Wipeout: Apple's "Wipeout - Single" has the barcode of the Spotify release
// "Wipeout". Recording B, from the Apple import, sits on a release made from
// Apple's wording; it is the same album, so it is marked for merging and gets
// the real release's album artists meanwhile.
func TestAReleaseMadeFromAppleWordingIsMarkedAndGetsTheRealReleasesAlbumArtists(t *testing.T) {
	sp := &albumFake{catalogFake: &catalogFake{name: providers.Spotify, byID: map[string]providers.Track{
		"spW": {Provider: providers.Spotify, ProviderID: "spW", Title: "Wipe Song", DurationMs: 200000,
			Artists: []providers.Artist{{ProviderID: "spWA", Name: "Wipe Artist"}},
			Releases: []providers.ReleaseRef{{ProviderID: "spAlbW", Title: "Wipe", Date: "2024-09-26", Disc: 1, Position: 1,
				Artists: []providers.Artist{{ProviderID: "spWA", Name: "Wipe Artist"}}}}}}},
		albums: map[string]providers.Album{"spAlbW": {ProviderID: "spAlbW", Title: "Wipe", Date: "2024-09-26", UPC: upcA, TotalTracks: 1}}}
	ap := &appleFake{catalogFake: &catalogFake{name: providers.AppleMusic, byID: map[string]providers.Track{
		"apW": {Provider: providers.AppleMusic, ProviderID: "apW", Title: "Wipe Single Song", DurationMs: 200000,
			Artists: []providers.Artist{{ProviderID: "apWA", Name: "Wipe Artist"}},
			Releases: []providers.ReleaseRef{{ProviderID: "1960000001", Title: "Wipe - Single", Date: "2024-09-26", DateIsTrack: true, Disc: 1, Position: 1,
				Artists: []providers.Artist{{Name: "Wipe Artist"}}}}}}},
		albums: map[string]providers.Album{"1960000001": {ProviderID: "1960000001", Title: "Wipe - Single", Date: "2024-09-26", UPC: upcA, TotalTracks: 1}}}
	e := newIDEnv(t, sp, ap)
	_, a := e.listen(t, identity.Input{Artist: "Wipe Artist", Title: "Wipe Song", Album: "Wipe", SpotifyTrackID: "spW", SpotifyAlbumID: "spAlbW",
		Credits: []identity.Credit{{Name: "Wipe Artist"}}})
	_, b := e.listen(t, identity.Input{Artist: "Wipe Artist", Title: "Wipe Single Song", Album: "Wipe - Single", AppleSongID: "apW",
		Credits: []identity.Credit{{Name: "Wipe Artist"}}}) // no album artists: Apple Play Activity has none
	if a == 0 || b == 0 || a == b {
		t.Fatalf("setup: two releases expected (%d, %d)", a, b)
	}
	if e.n(t, `SELECT COUNT(*) FROM release_artist WHERE release_id=?`, b) != 0 {
		t.Fatal("setup: the Apple-wording release should start with no album artist")
	}
	// The real release already holds the barcode (an earlier scan), so Apple's
	// album meets an owner that is not one of recording B's releases.
	e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, status) VALUES ('release', ?, 'upc', ?, 'lookup', 'auto')`, a, providers.NormalizeBarcode(upcA))
	e.db.Exec(`INSERT INTO artist (id, name) VALUES (9201, 'Wipe Artist Album')`)
	e.db.Exec(`INSERT INTO release_artist (release_id, artist_id, position) VALUES (?, 9201, 0)`, a)
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM release_artist WHERE release_id=?`, b) == 0 {
		t.Fatal("the stranded release still has no album artist")
	}
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE kind='release' AND reason=? AND ((entity_id=? AND other_id=?) OR (entity_id=? AND other_id=?))`, ReasonBarcodeVariant, a, b, b, a) != 1 {
		t.Fatal("the two releases of one album were not marked for merging")
	}
}

// A release that already has album artists keeps them.
func TestAVariantThatAlreadyHasAlbumArtistsKeepsThem(t *testing.T) {
	e := newIDEnv(t)
	tx, _ := e.db.Begin()
	var a, b int64
	tx.QueryRow(`INSERT INTO release (title) VALUES ('X') RETURNING id`).Scan(&a)
	tx.QueryRow(`INSERT INTO release (title) VALUES ('X - Single') RETURNING id`).Scan(&b)
	tx.Exec(`INSERT INTO artist (id, name) VALUES (9101, 'From Real'), (9102, 'Own')`)
	tx.Exec(`INSERT INTO release_artist (release_id, artist_id, position) VALUES (?, 9101, 0), (?, 9102, 0)`, a, b)
	err := copyAlbumArtists(context.Background(), tx, a, b)
	tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if e.n(t, `SELECT COUNT(*) FROM release_artist WHERE release_id=? AND artist_id=9102`, b) != 1 || e.n(t, `SELECT COUNT(*) FROM release_artist WHERE release_id=?`, b) != 1 {
		t.Fatal("a release's own album artists were replaced")
	}
}

// Koito-shaped listen: no track id, but the album's MusicBrainz id. MusicBrainz
// keeps one recording per master, so three tie on text. The release the listen
// named is on exactly one, so it is linked automatically, even though the
// recording has no consent for name-only matches (a release id is not a name).
func TestReleaseIDOfTheListenPicksTheMusicBrainzRecording(t *testing.T) {
	mk := func(id string, dur int, releases ...string) providers.Track {
		tr := track(id, "Hey Nineteen", "Steely Dan")
		tr.DurationMs = dur
		tr.Releases = nil
		for _, r := range releases {
			tr.Releases = append(tr.Releases, providers.ReleaseRef{ProviderID: r, Title: "Gaucho"})
		}
		return tr
	}
	fz := &catalogFake{name: providers.MusicBrainz, search: []providers.Track{
		mk("mb-2004", 306000, "rel-2004"), mk("mb-1998", 304000, "rel-1998"), mk("mb-1980", 305893, "rel-1980", "rel-comp")}}
	e := newIDEnv(t, fz)
	rec, rel := e.listen(t, identity.Input{Artist: "Steely Dan", Title: "Hey Nineteen", Album: "Gaucho", ReleaseMBID: "rel-1980",
		Credits: []identity.Credit{{Name: "Steely Dan"}}})
	if rel == 0 {
		t.Fatal("setup: no release")
	}
	e.db.Exec(`UPDATE recording SET fuzzy_consent=0 WHERE id=?`, rec)
	e.run(t)
	var id, status string
	if err := e.db.QueryRow(`SELECT provider_id, status FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz' AND status IN ('auto','confirmed')`, rec).Scan(&id, &status); err != nil || id != "mb-1980" {
		t.Fatalf("recording on the named release not linked: %q %v", id, err)
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND status='auto'`, rec) != 1 {
		t.Fatal("more than one automatic link")
	}
}

// Without the release id the same three stay a tie.
func TestWithoutAReleaseIDTheMusicBrainzTieStaysForReview(t *testing.T) {
	mk := func(id string, dur int) providers.Track {
		tr := track(id, "Hey Nineteen", "Steely Dan")
		tr.DurationMs = dur
		return tr
	}
	fz := &catalogFake{name: providers.MusicBrainz, search: []providers.Track{mk("mb-a", 306000), mk("mb-b", 304000), mk("mb-c", 305893)}}
	e := newIDEnv(t, fz)
	rec, _ := e.listen(t, identity.Input{Artist: "Steely Dan", Title: "Hey Nineteen", Album: "Gaucho", Credits: []identity.Credit{{Name: "Steely Dan"}}})
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND status='auto' AND provider='musicbrainz'`, rec) != 0 ||
		e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND status='candidate'`, rec) != 3 {
		t.Fatal("a tie was resolved without evidence")
	}
}

// A scan that finds the evidence for a candidate an earlier scan left promotes
// it, and drops the rival candidates of that provider. Confirmed links stay.
func TestARescanPromotesALeftOverCandidateAndDropsItsRivals(t *testing.T) {
	mk := func(id string, dur int, releases ...string) providers.Track {
		tr := track(id, "Hey Nineteen", "Steely Dan")
		tr.DurationMs = dur
		tr.Releases = nil
		for _, r := range releases {
			tr.Releases = append(tr.Releases, providers.ReleaseRef{ProviderID: r, Title: "Gaucho"})
		}
		return tr
	}
	fz := &catalogFake{name: providers.MusicBrainz, search: []providers.Track{
		mk("mb-2004", 306000, "rel-2004"), mk("mb-1998", 304000, "rel-1998"), mk("mb-1980", 305893, "rel-1980")}}
	e := newIDEnv(t, fz)
	rec, _ := e.listen(t, identity.Input{Artist: "Steely Dan", Title: "Hey Nineteen", Album: "Gaucho", ReleaseMBID: "rel-1980",
		Credits: []identity.Credit{{Name: "Steely Dan"}}})
	for _, id := range []string{"mb-2004", "mb-1998", "mb-1980"} { // left by an earlier scan
		e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status) VALUES ('recording', ?, 'musicbrainz', ?, 'fuzzy', 1.0, 'candidate')`, rec, id)
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz' AND provider_id='mb-1980' AND status='auto'`, rec) != 1 {
		t.Fatal("the left-over candidate was not promoted")
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND status='candidate'`, rec) != 0 {
		t.Fatal("rival candidates were left open")
	}
}

// A link a person confirmed (or a scan already made automatic) is never
// rewritten by a later result for the same id.
func TestStoreNeverRewritesAConfirmedOrAutoLink(t *testing.T) {
	e := newIDEnv(t, &catalogFake{name: providers.MusicBrainz})
	rec, _ := e.listen(t, identity.Input{Artist: "The Band", Title: "Great Song", Credits: []identity.Credit{{Name: "The Band"}}})
	e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status) VALUES
('recording', ?, 'musicbrainz', 'mb-conf', 'manual', 1.0, 'confirmed'), ('recording', ?, 'musicbrainz', 'mb-auto', 'isrc', 0.95, 'auto')`, rec, rec)
	res := match.Result{Links: []match.Link{
		{Provider: providers.MusicBrainz, ProviderID: "mb-conf", Method: match.MethodFuzzy, Confidence: 0.91, Status: match.StatusAuto},
		{Provider: providers.MusicBrainz, ProviderID: "mb-auto", Method: match.MethodFuzzy, Confidence: 0.91, Status: match.StatusAuto},
	}}
	if _, err := e.m.store(context.Background(), rec, nil, res, nil, albumEvidence{}); err != nil {
		t.Fatal(err)
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider_id='mb-conf' AND status='confirmed' AND match_method='manual' AND confidence=1.0`, rec) != 1 ||
		e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider_id='mb-auto' AND status='auto' AND match_method='isrc' AND confidence=0.95`, rec) != 1 {
		t.Fatal("an existing confirmed or auto link was rewritten")
	}
}

// ---- track ids submitted where recording ids are expected ----

const (
	tidTrack = "aaaaaaaa-1111-2222-3333-444444444444" // a MusicBrainz release-track id
	tidRec   = "bbbbbbbb-1111-2222-3333-444444444444" // the recording it belongs to
)

// tidFake is MusicBrainz where tidTrack is a track of recording tidRec, not a
// recording itself (GetTrack says not found for it).
type tidFake struct {
	*catalogFake
	tids    map[string]providers.Track
	lookups int
}

func (f *tidFake) LookupTrackID(_ context.Context, id string) (*providers.Track, error) {
	f.lookups++
	t, ok := f.tids[id]
	if !ok {
		return nil, providers.ErrNotFound
	}
	return &t, nil
}

func newTidFake() *tidFake {
	rec := providers.Track{Provider: providers.MusicBrainz, ProviderID: tidRec, Title: "Song", DurationMs: 200000, ISRCs: []string{"USAAA1111111"},
		Artists:  []providers.Artist{{ProviderID: "mbA", Name: "Artist"}},
		Releases: []providers.ReleaseRef{{ProviderID: "mbRel", Title: "Album", Date: "2001-02-03"}}}
	return &tidFake{catalogFake: &catalogFake{name: providers.MusicBrainz, byID: map[string]providers.Track{tidRec: rec}},
		tids: map[string]providers.Track{tidTrack: rec}}
}

// offFake is a provider with no credentials.
type offFake struct{ name string }

func (f offFake) Name() string { return f.name }
func (f offFake) SearchTrack(context.Context, providers.TrackQuery) ([]providers.Track, error) {
	return nil, providers.ErrNotConfigured
}
func (f offFake) GetTrack(context.Context, string) (*providers.Track, error) {
	return nil, providers.ErrNotConfigured
}
func (f offFake) GetArtwork(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrNotConfigured
}
func (f offFake) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrNotConfigured
}

func (e *idEnv) jobState(t *testing.T, rec int64) string {
	t.Helper()
	var s, o, le sql.NullString
	if err := e.db.QueryRow(`SELECT status, outcome, last_error FROM enrichment_job WHERE recording_id=?`, rec).Scan(&s, &o, &le); err != nil {
		t.Fatal(err)
	}
	return s.String + "/" + o.String + "/" + le.String
}

func TestATrackIDSubmittedAsARecordingIDResolvesToTheRecording(t *testing.T) {
	mb := newTidFake()
	e := newIDEnv(t, mb, offFake{providers.Spotify}, offFake{providers.AppleMusic})
	rec, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", RecordingMBID: tidTrack, Credits: []identity.Credit{{Name: "Artist"}}})
	e.run(t)
	if got := e.jobState(t, rec); got != "done/linked/" {
		t.Fatalf("job = %s", got)
	}
	// The recording's own id is linked beside the track id the listen named.
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz' AND provider_id=? AND status='auto' AND match_method='submitted_mbid'`, rec, tidRec) != 1 {
		t.Fatal("the resolved recording id was not linked")
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider_id=?`, rec, tidTrack) != 1 {
		t.Fatal("the id the listen named was dropped (the same listen would no longer find this recording)")
	}
	// What the recording gave us was applied: its ISRC and its release.
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='isrc' AND provider_id='USAAA1111111'`, rec) != 1 {
		t.Fatal("the resolved recording's ISRC was not attached")
	}
	// The same listen again finds the same recording, not a new one.
	again, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", RecordingMBID: tidTrack, Credits: []identity.Credit{{Name: "Artist"}}})
	if again != rec || e.n(t, `SELECT COUNT(*) FROM recording`) != 1 {
		t.Fatal("the track id no longer resolves to the recording")
	}
}

// The answer is remembered: another scan neither asks again nor duplicates links.
func TestTrackIDAnswersAreCachedAndRescansAreStable(t *testing.T) {
	mb := newTidFake()
	e := newIDEnv(t, mb, offFake{providers.Spotify})
	rec, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", RecordingMBID: tidTrack, Credits: []identity.Credit{{Name: "Artist"}}})
	e.run(t)
	for i := 0; i < 2; i++ {
		if _, err := e.m.RequeueAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.m.ProcessDue(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if mb.lookups != 1 {
		t.Fatalf("track id looked up %d times, want 1", mb.lookups)
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz'`, rec) != 2 {
		t.Fatal("rescans changed the recording's links")
	}
}

// An id that is no recording and no track is dead. With only providers that
// have no credentials left, the job is finished instead of retrying every hour
// for ever; one whose held id could not be asked about yet is still waiting.
func TestADeadIDEndsTheJobButAnUnaskedIDWaits(t *testing.T) {
	mb := newTidFake()
	e := newIDEnv(t, mb, offFake{providers.Spotify}, offFake{providers.AppleMusic})
	dead, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", RecordingMBID: "cccccccc-1111-2222-3333-444444444444", Credits: []identity.Credit{{Name: "Artist"}}})
	e.run(t)
	if got := e.jobState(t, dead); got != "done/no_match/" {
		t.Fatalf("dead id job = %s", got)
	}
	// Only providers without credentials: the Spotify id cannot be asked about.
	e2 := newIDEnv(t, offFake{providers.Spotify}, offFake{providers.AppleMusic})
	waiting, _ := e2.listen(t, identity.Input{Artist: "Other", Title: "Tune", SpotifyTrackID: "spX", Credits: []identity.Credit{{Name: "Other"}}})
	e2.run(t)
	if got := e2.jobState(t, waiting); !strings.HasPrefix(got, "queued/") || !strings.Contains(got, "no provider available") {
		t.Fatalf("a Spotify id waiting for keys = %s", got)
	}
}

// If another recording already holds the recording id, the two are flagged for
// a person to merge; nothing is moved.
func TestAResolvedRecordingIDHeldElsewhereIsFlaggedNotMoved(t *testing.T) {
	mb := newTidFake()
	e := newIDEnv(t, mb, offFake{providers.Spotify})
	a, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", RecordingMBID: tidRec, Credits: []identity.Credit{{Name: "Artist"}}})
	b, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Song (track)", RecordingMBID: tidTrack, Credits: []identity.Credit{{Name: "Artist"}}})
	if a == b {
		t.Fatal("setup: expected two recordings")
	}
	e.run(t)
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND provider_id=? AND entity_id=?`, tidRec, b) != 0 {
		t.Fatal("the id held by another recording was moved or copied")
	}
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE kind='recording'`) < 1 {
		t.Fatal("the duplicate recordings were not flagged")
	}
	if e.n(t, `SELECT COUNT(*) FROM recording`) != 2 {
		t.Fatal("recordings were merged")
	}
}

// relFake is a catalogue that can read a release's tracklist, and finds nothing
// by searching.
type relFake struct {
	catalogFake
	tracklists map[string][]providers.Track
	asked      []string
}

func (f *relFake) LookupReleaseTracks(_ context.Context, id string) ([]providers.Track, error) {
	f.asked = append(f.asked, id)
	ts, ok := f.tracklists[id]
	if !ok {
		return nil, providers.ErrNotFound
	}
	return ts, nil
}

// Koito-shaped listen from a catalogue that spells the title differently: the
// history names the release, and the title carries a "(feat. …)" tag MusicBrainz
// leaves out, so no search by name would find it. The release's tracklist does,
// and since it is the release the listen named the link is automatic even with
// no consent for name-only matches.
func TestAListenThatNamesItsReleaseIsMatchedFromTheTracklist(t *testing.T) {
	mk := func(id, title string) providers.Track {
		tr := track(id, title, "Kikuo")
		tr.Releases = []providers.ReleaseRef{{ProviderID: "rel-orange", Title: "Orange"}}
		return tr
	}
	fz := &relFake{
		catalogFake: catalogFake{name: providers.MusicBrainz},
		tracklists: map[string][]providers.Track{"rel-orange": {
			mk("mb-other", "Another Song"), mk("mb-youkai", "Moon Youkai"), mk("mb-third", "Third Song"),
		}},
	}
	e := newIDEnv(t, fz)
	rec, _ := e.listen(t, identity.Input{Artist: "Kikuo", Title: "Moon Youkai (feat. Hatsune Miku)", Album: "Orange",
		ReleaseMBID: "rel-orange", Credits: []identity.Credit{{Name: "Kikuo"}}})
	e.db.Exec(`UPDATE recording SET fuzzy_consent=0 WHERE id=?`, rec)
	e.run(t)
	var id string
	if err := e.db.QueryRow(`SELECT provider_id FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz' AND status='auto'`, rec).Scan(&id); err != nil || id != "mb-youkai" {
		t.Fatalf("track not linked from the tracklist: %q %v", id, err)
	}
	if e.n(t, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=?`, rec) != 1 {
		t.Fatal("more than the one link")
	}
	if len(fz.asked) == 0 || fz.asked[0] != "rel-orange" {
		t.Fatalf("release not read: %v", fz.asked)
	}
}
