package edit_test

import (
	"testing"

	"younithm/internal/edit"
	"younithm/internal/names"
)

func TestMergeCandidates(t *testing.T) {
	e := newEnv(t)
	band := e.artist("25時、ナイトコードで。")
	other := e.artist("Someone Else")
	en := e.artist("Nightcord at 25:00") // the same band, imported under its English name
	e.exec(`INSERT INTO entity_name(entity_type,entity_id,lang,provider,provider_id,name,authoritative) VALUES ('artist',?,'en','apple','ap1','Nightcord at 25:00',1)`, band)

	song := e.recording("Song", band)
	twin := e.recording("song", band)   // same name, same artist
	cover := e.recording("Song", other) // same name, other artist
	linked := e.recording("Song (Album Version)", other)
	e.exec(`INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('recording',?,'spotify','sp1','isrc','auto'),('recording',?,'spotify','sp1','isrc','confirmed')`, song, linked)
	e.scrobble("2026-01-01T00:00:00Z", "x", "song", "", twin, nil)
	e.scrobble("2026-01-02T00:00:00Z", "x", "song", "", twin, nil)

	got, err := edit.MergeSuggestions(ctx, e.db, "recording", song, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != linked || got[0].Reason != "Same catalogue entry" || got[1].ID != twin || got[1].Plays != 2 || got[1].Reason != "Same name" {
		t.Fatalf("track suggestions: %+v", got)
	}
	for _, c := range got {
		if c.ID == cover || c.ID == song {
			t.Fatalf("suggested %d", c.ID)
		}
	}

	// An artist stored in Japanese finds its English twin by catalogue name.
	as, err := edit.MergeSuggestions(ctx, e.db, "artist", band, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].ID != en {
		t.Fatalf("artist suggestions: %+v", as)
	}
	// Search matches catalogue names too, and never offers the entity itself.
	hits, err := edit.MergeSearch(ctx, e.db, "artist", en, "nightcord", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != band || hits[0].Sub != "2 tracks" {
		t.Fatalf("search: %+v", hits)
	}
	// With no entity to exclude (re-linking an unmatched scrobble), every match comes back.
	all, err := edit.MergeSearch(ctx, e.db, "recording", 0, "song", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("search without id: %+v", all)
	}
	if _, err := edit.MergeSearch(ctx, e.db, "genre", 1, "x", 5); err == nil {
		t.Fatal("bad kind accepted")
	}
}

func TestMergeSuggestionsSimilarName(t *testing.T) {
	e := newEnv(t)
	band := e.artist("Band")
	other := e.artist("Other")
	plain := e.recording("Ur not really bout it", band)
	bonus := e.recording("Ur not really bout it (bonus track)", band)
	remaster := e.recording("Ur Not Really Bout It - 2011 Remaster", band)
	live := e.recording("Ur not really bout it (Live)", band)          // a different recording
	cover := e.recording("Ur not really bout it (bonus track)", other) // no shared artist
	longer := e.recording("Ur not really bout it at all", band)

	got, err := edit.MergeSuggestions(ctx, e.db, "recording", plain, 20)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]string{}
	for _, c := range got {
		ids[c.ID] = c.Reason
	}
	if len(got) != 2 || ids[bonus] != "Similar name" || ids[remaster] != "Similar name" {
		t.Fatalf("from plain: %+v", got)
	}
	for _, x := range []int64{live, cover, longer} {
		if _, ok := ids[x]; ok {
			t.Fatalf("suggested %d: %+v", x, got)
		}
	}

	// The other way round: the bonus track finds the plain title and the remaster.
	got, err = edit.MergeSuggestions(ctx, e.db, "recording", bonus, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Reason != "Similar name" {
		t.Fatalf("from bonus: %+v", got)
	}
}

func TestMergeSearchIgnoresPunctuation(t *testing.T) {
	e := newEnv(t)
	a := e.artist("The Mamas & the Papas")
	mm := e.recording("Monday Monday", a)
	dont := e.recording("Don't Stop", a)
	for q, want := range map[string]int64{"Monday, Monday": mm, "monday monday": mm, "dont stop": dont, "Don’t": dont} {
		hits, err := edit.MergeSearch(ctx, e.db, "recording", 0, q, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ID != want {
			t.Fatalf("%q: %+v", q, hits)
		}
	}
	// Punctuation alone still searches literally rather than matching everything.
	if hits, _ := edit.MergeSearch(ctx, e.db, "recording", 0, "'", 10); len(hits) != 1 || hits[0].ID != dont {
		t.Fatalf("quote only: %+v", hits)
	}
}

func TestMergeSuggestionsSameAlbumAndReleaseGroup(t *testing.T) {
	e := newEnv(t)
	duo := e.artist("Casiopea")
	stray := e.artist("Casiopea Vs The Square") // a credit string taken as written
	alb := e.release("Casiopea vs. The Square", "2000")
	other := e.release("Elsewhere", "2001")
	song := e.recording("Asayake", duo)
	twin := e.recording("ASAYAKE", stray)
	far := e.recording("Asayake", stray) // same name, no shared artist or album
	e.onRelease(song, alb, 1)
	e.onRelease(twin, alb, 1)
	e.onRelease(far, other, 1)

	got, err := edit.MergeSuggestions(ctx, e.db, "recording", song, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != twin || got[0].Reason != "Same name" {
		t.Fatalf("same album: %+v", got)
	}

	mono := e.release("Pet Sounds (Mono)", "1966")
	stereo := e.release("Pet Sounds", "1966")
	unrelated := e.release("Smiley Smile", "1967")
	e.exec(`UPDATE release SET release_group_mbid='rg-1' WHERE id IN (?, ?)`, mono, stereo)
	e.exec(`UPDATE release SET release_group_mbid='rg-2' WHERE id=?`, unrelated)
	got, err = edit.MergeSuggestions(ctx, e.db, "release", mono, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != stereo || got[0].Reason != "Same release group" {
		t.Fatalf("release group: %+v", got)
	}
}

func TestCreditLineSQLUsesCreditedNamesAndJoinPhrases(t *testing.T) {
	e := newEnv(t)
	c := e.artist("Casiopea")
	s := e.artist("T-SQUARE")
	rel := e.release("Casiopea vs. The Square", "2000")
	e.exec(`INSERT INTO release_artist(release_id, artist_id, position, join_phrase, credited_name) VALUES (?, ?, 0, ' vs. ', NULL), (?, ?, 1, ' ignored', 'The Square')`, rel, c, rel, s)
	line := func() string {
		return e.str(`SELECT `+names.LineSQL(names.Release, "?"), rel)
	}
	if got := line(); got != "Casiopea vs. The Square" {
		t.Fatalf("line = %q", got)
	}
	// A rename by hand applies where no credited name is set; the credited name wins.
	e.exec(`UPDATE artist SET display_name='CASIOPEA' WHERE id=?`, c)
	e.exec(`UPDATE artist SET display_name='T-Square' WHERE id=?`, s)
	if got := line(); got != "CASIOPEA vs. The Square" {
		t.Fatalf("with overrides = %q", got)
	}
	// No join phrase means ", ".
	e.exec(`UPDATE release_artist SET join_phrase=NULL WHERE release_id=?`, rel)
	if got := line(); got != "CASIOPEA, The Square" {
		t.Fatalf("default join = %q", got)
	}
}

func TestSetCreditsCreditedNameAndUndo(t *testing.T) {
	e := newEnv(t)
	c := e.artist("Casiopea")
	s := e.artist("T-SQUARE")
	rec := e.recording("Asayake", c, s)
	res, err := e.ed.SetCredits(ctx, "a", "recording", rec, []edit.CreditIn{
		{ArtistID: c, JoinPhrase: " vs. "}, {ArtistID: s, CreditedName: " The Square "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.str(`SELECT COALESCE(credited_name, '') FROM recording_artist WHERE recording_id=? AND artist_id=?`, rec, s); got != "The Square" {
		t.Fatalf("credited name = %q", got)
	}
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.str(`SELECT COALESCE(credited_name, '') FROM recording_artist WHERE recording_id=? AND artist_id=?`, rec, s); got != "" {
		t.Fatalf("after undo = %q", got)
	}
}

func TestMergeSearchPutsSameArtistFirst(t *testing.T) {
	e := newEnv(t)
	band := e.artist("Band")
	other := e.artist("Other")
	song := e.recording("Song", band)
	exact := e.recording("Song", other) // exact title, other artist, more plays
	mine := e.recording("Song (Live)", band)
	e.scrobble("2026-01-01T00:00:00Z", "x", "song", "", exact, nil)

	hits, err := edit.MergeSearch(ctx, e.db, "recording", song, "song", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != mine || hits[1].ID != exact {
		t.Fatalf("hits: %+v", hits)
	}
}

func TestSearchFindsDisplayNames(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Kyary")
	rec := e.recording("PonPonPon", a)
	e.exec(`UPDATE artist SET display_name = 'きゃりーぱみゅぱみゅ' WHERE id = ?`, a)
	e.exec(`UPDATE recording SET display_name = 'Pon Pon Pon' WHERE id = ?`, rec)

	as, err := edit.Search(ctx, e.db, "artist", "きゃりー", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].ID != a || as[0].Name != "きゃりーぱみゅぱみゅ" {
		t.Fatalf("artists: %+v", as)
	}
	rs, err := edit.Search(ctx, e.db, "recording", "pon pon", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ID != rec || rs[0].Name != "Pon Pon Pon" {
		t.Fatalf("recordings: %+v", rs)
	}
}
