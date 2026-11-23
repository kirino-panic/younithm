package edit_test

import (
	"testing"

	"younithm/internal/edit"
)

func TestReleaseDateTakesAYearMonthOrDay(t *testing.T) {
	e := newEnv(t)
	rel := e.release("Album", "2020")
	for _, ok := range []string{"1999", "1999-07", "1999-07-31"} {
		if _, err := e.ed.EditEntity(ctx, "admin", "release", rel, edit.EditEntity(map[string]any{"release_date": ok})); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"99", "1999-13", "1999-02-30", "July 1999", "1999-7"} {
		wantErr(t, mustErr(e.ed.EditEntity(ctx, "admin", "release", rel, edit.EditEntity(map[string]any{"release_date": bad}))), "bad_value")
	}
	if _, err := e.ed.EditEntity(ctx, "admin", "release", rel, edit.EditEntity(map[string]any{"release_date": nil})); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM release WHERE id=? AND release_date IS NULL`, rel) != 1 {
		t.Fatal("clearing the date did not clear it")
	}
}

func TestArtistSearchFindsSearchNames(t *testing.T) {
	e := newEnv(t)
	a := e.artist("25時、ナイトコードで。")
	e.exec(`INSERT INTO artist_search_name(artist_id,name,provider,provider_id) VALUES (?,'niigo','musicbrainz','mb-1')`, a)
	hits, err := edit.Search(ctx, e.db, "artist", "niig", 8)
	if err != nil || len(hits) != 1 || hits[0].ID != a {
		t.Fatalf("search %+v %v", hits, err)
	}
	cands, err := edit.MergeSearch(ctx, e.db, "artist", 0, "niigo", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ID != a {
		t.Fatalf("merge search %+v", cands)
	}
}
