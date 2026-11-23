package edit_test

import (
	"fmt"
	"testing"
)

func TestApplyReleaseArtistsIsOneUndoableEdit(t *testing.T) {
	e := newEnv(t)
	band, singer, other := e.artist("Band"), e.artist("Singer"), e.artist("Someone Else")
	rel := e.release("Album", "2020")
	e.exec(`INSERT INTO release_artist(release_id,artist_id,position,join_phrase) VALUES (?,?,0,' & '),(?,?,1,NULL)`, rel, band, rel, singer)
	wrong := e.recording("One", other)
	right := e.recording("Two", band, singer)
	e.exec(`UPDATE recording_artist SET join_phrase=' & ' WHERE recording_id=? AND position=0`, right)
	e.onRelease(wrong, rel, 1)
	e.onRelease(right, rel, 1)
	before := e.snapshot()

	res, err := e.ed.ApplyReleaseArtists(ctx, "admin", rel)
	if err != nil {
		t.Fatal(err)
	}
	credit := `SELECT group_concat(a.name || COALESCE(ra.join_phrase,''), '') FROM (SELECT * FROM recording_artist WHERE recording_id=? ORDER BY position) ra JOIN artist a ON a.id=ra.artist_id`
	if got := e.str(credit, wrong); got != "Band & Singer" {
		t.Fatalf("credits %q", got)
	}
	if res.Summary != fmt.Sprintf("Apply artists of release %d to 1 tracks", rel) {
		t.Fatalf("a track already credited this way was changed: %q", res.Summary)
	}
	if _, err := e.ed.Undo(ctx, "admin", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != before {
		t.Fatal("undo did not restore every track")
	}

	e.exec(`DELETE FROM recording_artist WHERE recording_id=?`, wrong)
	e.exec(`INSERT INTO recording_artist(recording_id,artist_id,role,position,join_phrase) VALUES (?,?,'primary',0,' & '),(?,?,'primary',1,NULL)`, wrong, band, wrong, singer)
	wantErr(t, mustErr(e.ed.ApplyReleaseArtists(ctx, "admin", rel)), "no_change")
	bare := e.release("Bare", "2021")
	wantErr(t, mustErr(e.ed.ApplyReleaseArtists(ctx, "admin", bare)), "no_artists")
}
