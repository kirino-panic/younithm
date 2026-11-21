package edit_test

import (
	"testing"

	"younithm/internal/edit"
)

func TestSplitArtistRecreditsAndSticks(t *testing.T) {
	e := newEnv(t)
	combo := e.artist("Danny Brown & Underscores")
	danny := e.artist("Danny Brown")
	feat := e.artist("Guest")
	r1 := e.recording("Song", combo, feat) // the split artist first, a guest after
	e.exec(`UPDATE recording_artist SET join_phrase=' feat. ' WHERE recording_id=? AND artist_id=?`, r1, combo)
	r2 := e.recording("Other", danny, combo) // one part already credited
	alb := e.release("Album", "2024")
	e.exec(`INSERT INTO release_artist(release_id, artist_id) VALUES (?,?)`, alb, combo)
	e.exec(`INSERT INTO alias(kind, raw_string, entity_id) VALUES ('artist', 'danny brown and underscores', ?)`, combo)
	start := e.snapshot()

	res, err := e.ed.Split(ctx, "a", combo, []edit.CreditIn{{ArtistID: danny, JoinPhrase: " & "}, {Name: "Underscores"}})
	if err != nil {
		t.Fatal(err)
	}
	under := int64(0)
	_ = e.db.QueryRow(`SELECT id FROM artist WHERE name='Underscores'`).Scan(&under)
	if under == 0 || e.count(`SELECT COUNT(*) FROM artist WHERE id=?`, combo) != 0 {
		t.Fatal("split artist should be gone and Underscores created")
	}
	line := func(rec int64) string {
		return e.str(`SELECT group_concat(a.name || COALESCE(ra.join_phrase, '|'), '') FROM (SELECT * FROM recording_artist WHERE recording_id=? ORDER BY position) ra JOIN artist a ON a.id=ra.artist_id`, rec)
	}
	if got := line(r1); got != "Danny Brown & Underscores feat. Guest|" {
		t.Fatalf("r1 credits = %q", got)
	}
	if got := line(r2); got != "Danny Brown|Underscores|" { // Danny keeps his own row
		t.Fatalf("r2 credits = %q", got)
	}
	if e.count(`SELECT COUNT(*) FROM release_artist WHERE release_id=?`, alb) != 2 {
		t.Fatal("album credit not split")
	}
	// Later listens with the old string, or its other spelling, get both artists.
	if r, _ := e.resolve(t, "Danny Brown & Underscores", "Song", ""); r != r1 {
		t.Fatalf("old string resolves to %d, want %d", r, r1)
	}
	newRec, _ := e.resolve(t, "danny brown and underscores", "New Song", "")
	if got := line(newRec); got != "Danny Brown & Underscores|" {
		t.Fatalf("new listen credits = %q", got)
	}
	if e.count(`SELECT COUNT(*) FROM artist WHERE lower(name) LIKE '%&%'`) != 0 {
		t.Fatal("the joined artist came back")
	}

	wantErr(t, mustErr(e.ed.Split(ctx, "a", danny, []edit.CreditIn{{ArtistID: danny}, {Name: "X"}})), "bad_value")
	wantErr(t, mustErr(e.ed.Split(ctx, "a", danny, []edit.CreditIn{{Name: "X"}})), "bad_value")

	// Undo once the new listen is gone again.
	e.exec(`DELETE FROM recording_artist WHERE recording_id=?`, newRec)
	e.exec(`DELETE FROM recording WHERE id=?`, newRec)
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); got != start {
		t.Fatalf("split round trip differs:\n%s\nvs\n%s", got, start)
	}
}

func TestMergeOnceForgetsNames(t *testing.T) {
	e := newEnv(t)
	a1, a2 := e.artist("Typo Name"), e.artist("Right Name")
	r1 := e.recording("Song", a1)
	e.exec(`INSERT INTO alias(kind, raw_string, entity_id) VALUES ('artist', 'typo name 2', ?)`, a1)
	e.exec(`INSERT INTO external_link(entity_type, entity_id, provider, provider_id, match_method) VALUES ('artist', ?, 'spotify', 'sp1', 'manual')`, a1)
	start := e.snapshot()

	res, err := e.ed.MergeWith(ctx, "a", "artist", a1, a2, edit.MergeOptions{Once: true})
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_artist WHERE recording_id=? AND artist_id=?`, r1, a2) != 1 {
		t.Fatal("credit not moved")
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE entity_id=?`, a2)+e.count(`SELECT COUNT(*) FROM external_link WHERE entity_id=?`, a2) != 0 {
		t.Fatal("a one-time merge must not leave aliases or links on the kept artist")
	}
	// A later listen under the old name makes a new artist, not Right Name.
	rec, _ := e.resolve(t, "Typo Name", "Song", "")
	if rec == r1 {
		t.Fatal("old name still redirects to the kept artist")
	}
	if e.count(`SELECT COUNT(*) FROM artist WHERE name='Typo Name'`) != 1 {
		t.Fatal("old name should be a new artist")
	}
	e.exec(`DELETE FROM recording_artist WHERE recording_id=?`, rec)
	e.exec(`DELETE FROM recording WHERE id=?`, rec)
	e.exec(`DELETE FROM artist WHERE name='Typo Name'`)
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); got != start {
		t.Fatalf("one-time merge round trip differs:\n%s\nvs\n%s", got, start)
	}
}
