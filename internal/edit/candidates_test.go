package edit_test

import (
	"testing"

	"younithm/internal/edit"
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
