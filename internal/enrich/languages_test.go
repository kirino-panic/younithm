package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"younithm/internal/providers"
	"younithm/internal/providers/match"
	"younithm/internal/providers/snapshot"
)

// fakeMB answers alias lookups from a map, counting requests.
type fakeMB struct {
	fakeProvider
	aliases map[string][]providers.LocalName
	nameReq int
}

func (f *fakeMB) MaxArtists() int { return 100 }
func (f *fakeMB) ArtistNames(_ context.Context, ids []string) (map[string][]providers.LocalName, error) {
	f.nameReq++
	out := map[string][]providers.LocalName{}
	for _, id := range ids {
		out[id] = f.aliases[id]
	}
	return out, nil
}

// fakeCatalogue names ids per language; English is the only language
// Spotify-like fakes localize nothing in.
type fakeCatalogue struct {
	fakeProvider
	name  string
	names map[string]map[string]string // lang → id → name
	auth  bool
	reqs  int
}

func (f *fakeCatalogue) Name() string              { return f.name }
func (f *fakeCatalogue) BatchSize(kind string) int { return 2 }
func (f *fakeCatalogue) NamesIn(_ context.Context, kind string, ids []string, lang string) (map[string]string, bool, error) {
	f.reqs++
	out := map[string]string{}
	for _, id := range ids {
		if n, ok := f.names[lang][kind+":"+id]; ok {
			out[id] = n
		}
	}
	return out, f.auth, nil
}

// answer caches one catalogue's track answer crediting the given artist ids.
func answer(t *testing.T, e *env, provider, trackID string, artistIDs ...string) {
	t.Helper()
	tr := providers.Track{Provider: provider, ProviderID: trackID}
	for _, id := range artistIDs {
		tr.Artists = append(tr.Artists, providers.Artist{ProviderID: id, Name: "whatever " + id})
	}
	raw, _ := json.Marshal(tr)
	if err := snapshot.New(e.db).Put(context.Background(), provider, trackID, snapshot.KindRecording, raw, 0); err != nil {
		t.Fatal(err)
	}
}

func appleLink(t *testing.T, e *env, artist int64) string {
	t.Helper()
	var id, method string
	err := e.db.QueryRow(`SELECT provider_id, match_method FROM external_link WHERE entity_type='artist' AND entity_id=? AND provider='apple'`, artist).Scan(&id, &method)
	if err != nil {
		return ""
	}
	if method != "isrc" {
		t.Fatalf("artist %d linked by %q", artist, method)
	}
	return id
}

func TestNamePassLinksByCreditsAndNamesInBatches(t *testing.T) {
	e := setup(t)
	mb := &fakeMB{aliases: map[string][]providers.LocalName{"mb-band": {
		{Lang: "en", Name: "The Band (alias)", Primary: false},
		{Lang: "en", Name: "Nightband", Primary: true},
		{Lang: "ja", Name: "ザ・バンド", Primary: true},
	}}}
	apple := &fakeCatalogue{name: providers.AppleMusic, auth: true, names: map[string]map[string]string{
		"en": {"artist:111": "Band EN", "recording:555": "Song EN"},
		"ja": {"artist:111": "バンド", "recording:555": "曲"},
	}}
	sp := &fakeCatalogue{name: providers.Spotify, names: map[string]map[string]string{}}
	e.m = NewManager(e.db, match.New(providers.NewRegistry(mb, apple, sp), match.Config{}), snapshot.New(e.db),
		Options{Now: func() time.Time { return e.now }, MaxAttempts: 3})
	ctx := context.Background()

	// Artist 1 holds Spotify id sp-band. Its track is answered by Spotify
	// (crediting sp-band) and by Apple (crediting 111, unheld): one credit each.
	exec(t, e.db, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('artist',1,'musicbrainz','mb-band','lookup','auto'),
	  ('artist',1,'spotify','sp-band','lookup','auto'),
	  ('recording',1,'spotify','sp-1','isrc','auto'),
	  ('recording',1,'apple','555','isrc','auto')`)
	answer(t, e, providers.Spotify, "sp-1", "sp-band")
	answer(t, e, providers.AppleMusic, "555", "111")
	exec(t, e.db, `INSERT INTO admin_setting(key,value) VALUES ('languages.preferred','en,ja')`)

	if err := e.m.NamePass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := appleLink(t, e, 1); got != "111" {
		t.Fatalf("apple artist link = %q", got)
	}

	name := func(kind, lang, provider string) string {
		var n string
		_ = e.db.QueryRow(`SELECT name FROM entity_name WHERE entity_type=? AND entity_id=1 AND lang=? AND provider=?`, kind, lang, provider).Scan(&n)
		return n
	}
	if got := name("artist", "en", "musicbrainz"); got != "Nightband" {
		t.Fatalf("MusicBrainz en = %q (primary alias wins)", got)
	}
	if got := name("artist", "ja", "apple"); got != "バンド" {
		t.Fatalf("Apple ja = %q", got)
	}
	if got := name("recording", "ja", "apple"); got != "曲" {
		t.Fatalf("Apple recording ja = %q", got)
	}
	var stored string
	_ = e.db.QueryRow(`SELECT name FROM artist WHERE id=1`).Scan(&stored)
	if stored != "The Band" {
		t.Fatalf("stored name changed to %q", stored)
	}

	// A second pass asks nothing: every gap is filled or recorded as asked.
	names, appleReqs := mb.nameReq, apple.reqs
	if err := e.m.NamePass(ctx); err != nil {
		t.Fatal(err)
	}
	if mb.nameReq != names || apple.reqs != appleReqs {
		t.Fatalf("second pass asked again: names %d→%d apple %d→%d", names, mb.nameReq, appleReqs, apple.reqs)
	}

	// A new language is the only gap.
	exec(t, e.db, `UPDATE admin_setting SET value='en,ja,ko' WHERE key='languages.preferred'`)
	if err := e.m.NamePass(ctx); err != nil {
		t.Fatal(err)
	}
	var ko int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM entity_name WHERE lang='ko'`).Scan(&ko)
	if ko == 0 {
		t.Fatal("no ko rows")
	}
}

// linkEnv has artists 1 (The Band), 2 and 3, with Spotify ids sp-1..sp-3.
func linkEnv(t *testing.T) *env {
	e := setup(t)
	exec(t, e.db, `INSERT INTO artist(id,name) VALUES (2,'Two'),(3,'Three')`)
	exec(t, e.db, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('artist',1,'spotify','sp-1','lookup','auto'),
	  ('artist',2,'spotify','sp-2','lookup','auto'),
	  ('artist',3,'spotify','sp-3','lookup','auto')`)
	return e
}

// creditedTrack adds our recording id crediting our artists, answered by Spotify and
// Apple with the given credits.
func creditedTrack(t *testing.T, e *env, id int64, method string, ours []int64, spotify, apple []string) {
	t.Helper()
	if id != 1 {
		exec(t, e.db, `INSERT INTO recording(id,title) VALUES (?, 'x')`, id)
	} else {
		exec(t, e.db, `DELETE FROM recording_artist WHERE recording_id = 1`)
	}
	for _, a := range ours {
		exec(t, e.db, `INSERT INTO recording_artist(recording_id,artist_id) VALUES (?,?)`, id, a)
	}
	sid, aid := fmt.Sprint("s", id), fmt.Sprint("a", id)
	exec(t, e.db, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('recording',?,'spotify',?,?,'auto'), ('recording',?,'apple',?,?,'auto')`, id, sid, method, id, aid, method)
	answer(t, e, providers.Spotify, sid, spotify...)
	answer(t, e, providers.AppleMusic, aid, apple...)
}

func TestLinkingByElimination(t *testing.T) {
	e := linkEnv(t)
	// Track 1: three artists each side; Apple 101 and 102 are unknown, so nothing is certain.
	creditedTrack(t, e, 1, "isrc", []int64{1, 2, 3}, []string{"sp-1", "sp-2", "sp-3"}, []string{"101", "102", "103"})
	if err := e.m.linkArtists(context.Background()); err != nil {
		t.Fatal(err)
	}
	if appleLink(t, e, 1) != "" || appleLink(t, e, 2) != "" {
		t.Fatal("linked with several unknowns on each side")
	}
	// Track 2: artist 1 alone settles 101; track 3 (artists 1 and 2) then
	// settles 102; back on track 1 only 103 and artist 3 are left.
	creditedTrack(t, e, 2, "isrc", []int64{1}, []string{"sp-1"}, []string{"101"})
	creditedTrack(t, e, 3, "submitted_id", []int64{1, 2}, []string{"sp-2", "sp-1"}, []string{"101", "102"})
	if err := e.m.linkArtists(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a, b, c := appleLink(t, e, 1), appleLink(t, e, 2), appleLink(t, e, 3); a != "101" || b != "102" || c != "103" {
		t.Fatalf("links = %q %q %q", a, b, c)
	}
}

func TestLinkingLeavesUncertainPairsAlone(t *testing.T) {
	ctx := context.Background()
	t.Run("fuzzy track match", func(t *testing.T) {
		e := linkEnv(t)
		creditedTrack(t, e, 1, "fuzzy", []int64{1}, []string{"sp-1"}, []string{"101"})
		_ = e.m.linkArtists(ctx)
		if appleLink(t, e, 1) != "" {
			t.Fatal("linked through a fuzzy track match")
		}
	})
	t.Run("artist not on our track", func(t *testing.T) {
		e := linkEnv(t)
		creditedTrack(t, e, 1, "isrc", []int64{2}, []string{"sp-1"}, []string{"101"})
		_ = e.m.linkArtists(ctx)
		if appleLink(t, e, 1) != "" {
			t.Fatal("linked an artist our track does not credit")
		}
	})
	t.Run("tracks disagree", func(t *testing.T) {
		e := linkEnv(t)
		creditedTrack(t, e, 1, "isrc", []int64{1}, []string{"sp-1"}, []string{"101"})
		creditedTrack(t, e, 2, "isrc", []int64{2}, []string{"sp-2"}, []string{"101"})
		_ = e.m.linkArtists(ctx)
		if appleLink(t, e, 1) != "" || appleLink(t, e, 2) != "" {
			t.Fatal("linked an id two tracks give different artists")
		}
	})
	t.Run("artist already holds another id", func(t *testing.T) {
		e := linkEnv(t)
		exec(t, e.db, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES ('artist',1,'apple','999','lookup','auto')`)
		creditedTrack(t, e, 1, "isrc", []int64{1}, []string{"sp-1"}, []string{"101"})
		_ = e.m.linkArtists(ctx)
		var n int
		_ = e.db.QueryRow(`SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND provider_id='101'`).Scan(&n)
		if n != 0 {
			t.Fatal("gave an artist a second Apple id")
		}
	})
	t.Run("different number of credits", func(t *testing.T) {
		e := linkEnv(t)
		creditedTrack(t, e, 1, "isrc", []int64{1, 2}, []string{"sp-1", "sp-2"}, []string{"101"})
		_ = e.m.linkArtists(ctx)
		if appleLink(t, e, 1) != "" || appleLink(t, e, 2) != "" {
			t.Fatal("linked when the credits do not line up")
		}
	})
}

func TestLangScore(t *testing.T) {
	for _, c := range []struct {
		want, have string
		score      int
	}{{"en", "en", 2}, {"en", "en-US", 1}, {"en-US", "en", 1}, {"zh-Hant", "zh-Hant-TW", 1}, {"zh-Hant", "zh-Hans", 0}, {"ja", "en", 0}} {
		if got := langScore(c.want, c.have); got != c.score {
			t.Errorf("langScore(%q, %q) = %d, want %d", c.want, c.have, got, c.score)
		}
	}
}

// Every MusicBrainz alias of an artist holding a MusicBrainz id becomes an
// alias of ours, once: hints and names with no language too, but never a
// name another artist goes by, and nothing for an id two artists hold.
func TestNamePassImportsMusicBrainzAliases(t *testing.T) {
	e := setup(t)
	mb := &fakeMB{aliases: map[string][]providers.LocalName{
		"mb-band":   {{Lang: "ja", Name: "ザ・バンド"}, {Name: "Band, The"}, {Name: "TheBand", Hint: true}, {Name: "Other Artist"}},
		"mb-shared": {{Name: "Nobody Knows"}},
	}}
	e.m = NewManager(e.db, match.New(providers.NewRegistry(mb), match.Config{}), snapshot.New(e.db),
		Options{Now: func() time.Time { return e.now }, MaxAttempts: 3})
	exec(t, e.db, `INSERT INTO artist(id,name) VALUES (2,'Other Artist'),(3,'Shared A'),(4,'Shared B')`)
	exec(t, e.db, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('artist',1,'musicbrainz','mb-band','lookup','auto'),
	  ('artist',3,'musicbrainz','mb-shared','lookup','auto'),
	  ('artist',4,'musicbrainz','mb-shared','lookup','auto')`)
	ctx := context.Background()
	if err := e.m.NamePass(ctx); err != nil {
		t.Fatal(err)
	}
	var got []string
	rows, _ := e.db.Query(`SELECT raw_string FROM alias WHERE kind='artist' AND entity_id=1 ORDER BY raw_string`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		got = append(got, s)
	}
	rows.Close()
	want := fmt.Sprint([]string{"band, the", "theband", "ザ・バンド"})
	if fmt.Sprint(got) != want {
		t.Fatalf("aliases %v, want %v", got, want)
	}
	var shared int
	e.db.QueryRow(`SELECT COUNT(*) FROM alias WHERE raw_string='nobody knows'`).Scan(&shared)
	if shared != 0 {
		t.Fatal("aliases were given for an id two artists hold")
	}
	reqs := mb.nameReq
	if err := e.m.NamePass(ctx); err != nil {
		t.Fatal(err)
	}
	if mb.nameReq != reqs {
		t.Fatal("a second pass asked for aliases again")
	}
}
