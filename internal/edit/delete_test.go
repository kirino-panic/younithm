package edit_test

import "testing"

func TestDeleteTrackTakesItsScrobbles(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Artist")
	r1, r2 := e.recording("Gone", a), e.recording("Kept", a)
	alb := e.release("Album", "2024")
	e.onRelease(r1, alb, 1)
	e.onRelease(r2, alb, 1)
	e.scrobble("2024-01-01T10:00:00Z", "Artist", "Gone", "Album", r1, alb)
	e.scrobble("2024-01-02T10:00:00Z", "Artist", "Gone", "", r1, nil)
	keep := e.scrobble("2024-01-03T10:00:00Z", "Artist", "Kept", "Album", r2, alb)
	e.exec(`INSERT INTO alias(kind, raw_string, entity_id) VALUES ('recording', 'artist'||char(31)||'gone (live)', ?)`, r1)
	start := e.snapshot()

	res, err := e.ed.DeleteEntity(ctx, "a", "recording", r1)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 1 || e.count(`SELECT COUNT(*) FROM scrobble WHERE id=?`, keep) != 1 {
		t.Fatal("only the track's scrobbles should go")
	}
	if e.count(`SELECT COUNT(*) FROM recording WHERE id=?`, r1)+e.count(`SELECT COUNT(*) FROM alias WHERE kind='recording'`) != 0 {
		t.Fatal("track and its alias should be gone")
	}
	if e.count(`SELECT COUNT(*) FROM release WHERE id=?`, alb) != 1 {
		t.Fatal("album should stay")
	}
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); got != start {
		t.Fatalf("round trip differs:\n%s\nvs\n%s", got, start)
	}
	wantErr(t, mustErr(e.ed.DeleteEntity(ctx, "a", "recording", 999)), "not_found")
	wantErr(t, mustErr(e.ed.DeleteEntity(ctx, "a", "song", r1)), "bad_kind")
}

func TestDeleteAlbumTakesItsScrobblesAndLoneTracks(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Artist")
	only, shared := e.recording("Only Here", a), e.recording("Shared", a)
	alb, other := e.release("Album", "2024"), e.release("Best Of", "2025")
	e.onRelease(only, alb, 1)
	e.onRelease(shared, alb, 1)
	e.onRelease(shared, other, 0)
	e.scrobble("2024-01-01T10:00:00Z", "Artist", "Only Here", "Album", only, alb)
	e.scrobble("2024-01-02T10:00:00Z", "Artist", "Shared", "Album", shared, alb)
	keep := e.scrobble("2024-01-03T10:00:00Z", "Artist", "Shared", "Best Of", shared, other)
	e.exec(`INSERT INTO release_artist(release_id, artist_id) VALUES (?,?)`, alb, a)
	start := e.snapshot()

	res, err := e.ed.DeleteEntity(ctx, "a", "release", alb)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 1 || e.count(`SELECT COUNT(*) FROM scrobble WHERE id=?`, keep) != 1 {
		t.Fatal("only scrobbles played from the album should go")
	}
	if e.count(`SELECT COUNT(*) FROM recording WHERE id=?`, only) != 0 || e.count(`SELECT COUNT(*) FROM recording WHERE id=?`, shared) != 1 {
		t.Fatal("the album-only track should go, the shared one stay")
	}
	if e.count(`SELECT COUNT(*) FROM artist WHERE id=?`, a) != 1 {
		t.Fatal("artist should stay")
	}
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); got != start {
		t.Fatalf("round trip differs:\n%s\nvs\n%s", got, start)
	}
}

func TestDeleteArtistTakesScrobblesOfEveryTrack(t *testing.T) {
	e := newEnv(t)
	a, b := e.artist("Gone"), e.artist("Other")
	solo, collab, theirs := e.recording("Solo", a), e.recording("Collab", b, a), e.recording("Theirs", b)
	soloAlb, comp := e.release("Solo Album", "2024"), e.release("Compilation", "2024")
	e.exec(`INSERT INTO release_artist(release_id, artist_id) VALUES (?,?)`, soloAlb, a)
	e.exec(`INSERT INTO release_artist(release_id, artist_id) VALUES (?,?)`, comp, a)
	e.exec(`INSERT INTO release_artist(release_id, artist_id) VALUES (?,?)`, comp, b)
	e.onRelease(solo, soloAlb, 1)
	e.onRelease(theirs, soloAlb, 1) // someone else's track on the deleted album
	e.onRelease(collab, comp, 1)
	e.scrobble("2024-01-01T10:00:00Z", "Gone", "Solo", "Solo Album", solo, soloAlb)
	e.scrobble("2024-01-02T10:00:00Z", "Other, Gone", "Collab", "Compilation", collab, comp)
	kept := e.scrobble("2024-01-03T10:00:00Z", "Other", "Theirs", "Solo Album", theirs, soloAlb)
	e.exec(`INSERT INTO alias(kind, raw_string, entity_id) VALUES ('artist', 'gone!', ?)`, a)
	start := e.snapshot()

	res, err := e.ed.DeleteEntity(ctx, "a", "artist", a)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 1 || e.str(`SELECT release_id FROM scrobble WHERE id=?`, kept) != "" {
		t.Fatal("other artist's scrobble should stay, without the deleted album")
	}
	if e.count(`SELECT COUNT(*) FROM recording WHERE id=?`, solo) != 0 {
		t.Fatal("solo track should go")
	}
	if e.count(`SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, collab) != 1 {
		t.Fatal("collab track should keep only the other artist")
	}
	if e.count(`SELECT COUNT(*) FROM release WHERE id=?`, soloAlb) != 0 || e.count(`SELECT COUNT(*) FROM release WHERE id=?`, comp) != 1 {
		t.Fatal("solo album should go, compilation stay")
	}
	if e.count(`SELECT COUNT(*) FROM artist WHERE id=?`, a)+e.count(`SELECT COUNT(*) FROM alias WHERE kind='artist'`) != 0 {
		t.Fatal("artist and alias should be gone")
	}
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); got != start {
		t.Fatalf("round trip differs:\n%s\nvs\n%s", got, start)
	}
}
