package edit_test

import (
	"testing"

	"younithm/internal/identity"
)

func TestNoAlbum(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Artist")
	rec := e.recording("Song", a)
	alb := e.release("Album", "2020")
	e.onRelease(rec, alb, 1)
	s := e.scrobble("2026-01-01T00:00:00Z", "Artist", "Song", "Album", rec, alb)
	before := e.snapshot()

	res, err := e.ed.SetNoAlbum(ctx, "a", rec, true)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM recording_release WHERE recording_id=?`, rec).Scan(&n)
	if n != 0 {
		t.Fatalf("still on %d albums", n)
	}
	var rel any
	e.db.QueryRow(`SELECT release_id FROM scrobble WHERE id=?`, s).Scan(&rel)
	if rel != nil {
		t.Fatalf("scrobble kept album %v", rel)
	}

	// A new listen naming the album gets none, and makes no album.
	var albums int
	e.db.QueryRow(`SELECT COUNT(*) FROM release`).Scan(&albums)
	r, relID, err := (&identity.Resolver{}).Resolve(ctx, e.db, identity.Parse(nil, identity.Input{Artist: "Artist", Title: "Song", Album: "Other Album"}))
	if err != nil {
		t.Fatal(err)
	}
	if r != rec || relID != 0 {
		t.Fatalf("resolve = rec %d rel %d, want rec %d rel 0", r, relID, rec)
	}
	var after int
	e.db.QueryRow(`SELECT COUNT(*) FROM release`).Scan(&after)
	if after != albums {
		t.Fatalf("an album was made: %d -> %d", albums, after)
	}

	// Undo puts everything back.
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); got != before {
		t.Fatalf("undo differs:\n%s\nwant\n%s", got, before)
	}

	// Adding to an album by hand lifts the mark.
	if _, err := e.ed.SetNoAlbum(ctx, "a", rec, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ed.AddToRelease(ctx, "a", rec, alb, 1, nil); err != nil {
		t.Fatal(err)
	}
	var flag int
	e.db.QueryRow(`SELECT no_album FROM recording WHERE id=?`, rec).Scan(&flag)
	if flag != 0 {
		t.Fatal("mark kept after adding to an album by hand")
	}
}
