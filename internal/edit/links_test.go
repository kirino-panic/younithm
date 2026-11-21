package edit_test

import (
	"testing"

	"younithm/internal/edit"
)

func TestSetLinkReplacesRejectsAndUndoes(t *testing.T) {
	e := newEnv(t)
	rec := e.recording("Song", e.artist("A"))
	e.exec(`INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES ('recording',?,'spotify','old','isrc','auto')`, rec)
	before := e.snapshot()
	res, err := e.ed.SetLink(ctx, "admin", "recording", rec, "spotify", "new")
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM external_link WHERE entity_id=? AND provider='spotify'`, rec) != 1 ||
		e.str(`SELECT provider_id || status || pinned || match_method FROM external_link WHERE entity_id=? AND provider='spotify'`, rec) != "newconfirmed1manual" {
		t.Fatal("link not replaced by a pinned manual link")
	}
	if e.count(`SELECT COUNT(*) FROM link_rejection WHERE entity_id=? AND provider_id='old'`, rec) != 1 {
		t.Fatal("the replaced id was not rejected")
	}
	if _, err := e.ed.Undo(ctx, "admin", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != before || e.count(`SELECT COUNT(*) FROM link_rejection`) != 0 {
		t.Fatal("undo did not restore the old link")
	}
	if _, err := e.ed.RemoveLink(ctx, "admin", "recording", rec, "spotify"); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM external_link WHERE entity_id=?`, rec) != 0 || e.count(`SELECT COUNT(*) FROM link_rejection`) != 1 {
		t.Fatal("remove did not drop and reject the link")
	}
	wantErr(t, mustErr(e.ed.SetLink(ctx, "admin", "recording", rec, "deezer", "x")), "bad_provider")
}

func TestSetCreditsCreatesReusesAndUndoes(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Main")
	e.artist("Guest")
	rec := e.recording("Song", a)
	before := e.snapshot()
	res, err := e.ed.SetCredits(ctx, "admin", "recording", rec, []edit.CreditIn{
		{ArtistID: a, JoinPhrase: " feat. "}, {Name: "Guest", JoinPhrase: " & "}, {Name: "Brand New"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := e.str(`SELECT group_concat(a.name || COALESCE(ra.join_phrase,''), '') FROM (SELECT * FROM recording_artist WHERE recording_id=? ORDER BY position) ra JOIN artist a ON a.id=ra.artist_id`, rec)
	if got != "Main feat. Guest & Brand New" || e.count(`SELECT COUNT(*) FROM artist`) != 3 {
		t.Fatalf("credits %q, artists %d", got, e.count(`SELECT COUNT(*) FROM artist`))
	}
	if _, err := e.ed.Undo(ctx, "admin", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != before {
		t.Fatal("undo did not restore the credits")
	}
	wantErr(t, mustErr(e.ed.SetCredits(ctx, "admin", "recording", rec, nil)), "bad_value")
	wantErr(t, mustErr(e.ed.SetCredits(ctx, "admin", "recording", rec, []edit.CreditIn{{ArtistID: a}, {ArtistID: a}})), "bad_value")
}

func TestAddToNewReleaseAndSearch(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Nocturne Band")
	rec := e.recording("Song", a)
	if _, err := e.ed.AddToNewRelease(ctx, "admin", rec, "Night Album"); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_release rr JOIN release r ON r.id=rr.release_id WHERE rr.recording_id=? AND r.title='Night Album' AND rr.is_primary=1`, rec) != 1 ||
		e.count(`SELECT COUNT(*) FROM release_artist ra JOIN release r ON r.id=ra.release_id WHERE r.title='Night Album' AND ra.artist_id=?`, a) != 1 {
		t.Fatal("new album not created with the track's artists")
	}
	hits, err := edit.Search(ctx, e.db, "release", "night", 8)
	if err != nil || len(hits) != 1 || hits[0].Name != "Night Album" || hits[0].Sub != "Nocturne Band" {
		t.Fatalf("release search %+v %v", hits, err)
	}
	hits, err = edit.Search(ctx, e.db, "artist", "noct", 8)
	if err != nil || len(hits) != 1 || hits[0].ID != a {
		t.Fatalf("artist search %+v %v", hits, err)
	}
	if hits, _ := edit.Search(ctx, e.db, "artist", "100%", 8); len(hits) != 0 {
		t.Fatal("LIKE wildcards in the query were not escaped")
	}
}
