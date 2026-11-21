package edit_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"younithm/internal/db"
	"younithm/internal/edit"
	"younithm/internal/identity"
	"younithm/internal/ingest"
)

var ctx = context.Background()

type env struct {
	t  *testing.T
	db *sql.DB
	ed *edit.Editor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pools, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pools.Close() })
	if err := db.Migrate(pools.Write); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, db: pools.Write, ed: &edit.Editor{DB: pools.Write}}
}

func (e *env) exec(q string, args ...any) int64 {
	e.t.Helper()
	res, err := e.db.Exec(q, args...)
	if err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *env) artist(name string) int64 { return e.exec(`INSERT INTO artist(name) VALUES (?)`, name) }
func (e *env) recording(title string, artists ...int64) int64 {
	id := e.exec(`INSERT INTO recording(title) VALUES (?)`, title)
	for i, a := range artists {
		e.exec(`INSERT INTO recording_artist(recording_id, artist_id, position) VALUES (?,?,?)`, id, a, i)
	}
	return id
}
func (e *env) release(title, date string) int64 {
	return e.exec(`INSERT INTO release(title, release_date) VALUES (?,?)`, title, date)
}
func (e *env) onRelease(rec, rel int64, primary int) {
	e.exec(`INSERT INTO recording_release(recording_id, release_id, is_primary) VALUES (?,?,?)`, rec, rel, primary)
}
func (e *env) scrobble(at, artist, title, album string, rec, rel any) int64 {
	return e.exec(`INSERT INTO scrobble(played_at, raw_artist, raw_title, raw_album, recording_id, release_id) VALUES (?,?,?,?,?,?)`,
		at, artist, title, nullStr(album), rec, rel)
}
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// snapshot dumps the editable tables so round trips can be compared exactly.
func (e *env) snapshot() string {
	e.t.Helper()
	var sb strings.Builder
	for _, tb := range []string{"scrobble", "artist", "recording", "release", "recording_artist", "release_artist",
		"recording_release", "alias", "external_link"} {
		rows, err := e.db.Query(`SELECT * FROM ` + tb + ` ORDER BY 1, 2`)
		if err != nil {
			e.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		fmt.Fprintf(&sb, "## %s\n", tb)
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = rows.Scan(ptrs...)
			b, _ := json.Marshal(vals)
			sb.Write(b)
			sb.WriteByte('\n')
		}
		rows.Close()
	}
	return sb.String()
}

func (e *env) str(q string, args ...any) string {
	e.t.Helper()
	var s sql.NullString
	if err := e.db.QueryRow(q, args...).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s.String
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func nint(v int64) edit.NullableInt { return edit.NullableInt{Set: true, Valid: true, V: v} }
func nnull() edit.NullableInt       { return edit.NullableInt{Set: true} }

func wantErr(t *testing.T, err error, code string) *edit.Error {
	t.Helper()
	var ee *edit.Error
	if !errors.As(err, &ee) || ee.Code != code {
		t.Fatalf("want error %q, got %v", code, err)
	}
	return ee
}

func TestNormMatchesIngest(t *testing.T) {
	for _, s := range []string{"  The  Beatles ", "ÉCLAIR\tPlus", "", "a"} {
		if edit.Norm(s) != ingest.Normalize(s) {
			t.Fatalf("Norm(%q) differs from ingest.Normalize", s)
		}
	}
}

func TestEditScrobbleTimeKeepsRawAndUndoes(t *testing.T) {
	e := newEnv(t)
	s := e.scrobble("2024-01-01T10:00:00Z", "Artist", "Song", "Album", nil, nil)
	start := e.snapshot()

	at := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	res, err := e.ed.EditScrobble(ctx, "admin:1", s, edit.ScrobbleEdit{PlayedAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.str(`SELECT played_at FROM scrobble WHERE id=?`, s); got != "2024-01-01T12:00:00Z" {
		t.Fatalf("played_at = %s", got)
	}
	if got := e.str(`SELECT orig_played_at FROM scrobble WHERE id=?`, s); got != "2024-01-01T10:00:00Z" {
		t.Fatalf("orig_played_at = %s", got)
	}
	// A second edit must not overwrite the original.
	at2 := at.Add(time.Hour)
	if _, err := e.ed.EditScrobble(ctx, "admin:1", s, edit.ScrobbleEdit{PlayedAt: &at2}); err != nil {
		t.Fatal(err)
	}
	if got := e.str(`SELECT orig_played_at FROM scrobble WHERE id=?`, s); got != "2024-01-01T10:00:00Z" {
		t.Fatalf("orig_played_at overwritten: %s", got)
	}
	if e.str(`SELECT raw_artist||raw_title||raw_album FROM scrobble WHERE id=?`, s) != "ArtistSongAlbum" {
		t.Fatal("raw values changed")
	}
	// Undo newest first, then the first one: back to the exact start state.
	var latest int64
	_ = e.db.QueryRow(`SELECT max(id) FROM edit_log`).Scan(&latest)
	if _, err := e.ed.Undo(ctx, "admin:1", latest); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ed.Undo(ctx, "admin:1", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); got != start {
		t.Fatalf("round trip differs:\n%s\nvs\n%s", got, start)
	}
}

func TestUndoRefusesWhenLaterEditConflicts(t *testing.T) {
	e := newEnv(t)
	s := e.scrobble("2024-01-01T10:00:00Z", "A", "S", "", nil, nil)
	t1 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)
	r1, _ := e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{PlayedAt: &t1})
	r2, _ := e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{PlayedAt: &t2})

	before := e.snapshot()
	ee := wantErr(t, mustErr(e.ed.Undo(ctx, "a", r1.EditID)), "undo_conflict")
	// edited_at also differs when the edits land in different milliseconds, and
	// it sorts before played_at, so look for played_at among the conflicts.
	hasPlayedAt := false
	for _, c := range ee.Conflicts {
		if c.Column == "played_at" {
			hasPlayedAt = true
		}
	}
	if ee.Status != 409 || !hasPlayedAt {
		t.Fatalf("conflicts: %+v %+v", ee, ee.Conflicts)
	}
	if e.snapshot() != before {
		t.Fatal("a refused undo changed data")
	}
	// Once the later edit is undone, the earlier one can be.
	if _, err := e.ed.Undo(ctx, "a", r2.EditID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ed.Undo(ctx, "a", r1.EditID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, mustErr(e.ed.Undo(ctx, "a", r1.EditID)), "already_undone")
}

func mustErr(_ *edit.Result, err error) error { return err }

func TestUndoOfUndoRedoes(t *testing.T) {
	e := newEnv(t)
	s := e.scrobble("2024-01-01T10:00:00Z", "A", "S", "", nil, nil)
	at := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	r, _ := e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{PlayedAt: &at})
	u, err := e.ed.Undo(ctx, "a", r.EditID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ed.Undo(ctx, "a", u.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.str(`SELECT played_at FROM scrobble WHERE id=?`, s); got != "2024-02-01T00:00:00Z" {
		t.Fatalf("redo: %s", got)
	}
	// The original is live again, so it can be undone again.
	if _, err := e.ed.Undo(ctx, "a", r.EditID); err != nil {
		t.Fatal(err)
	}
}

func TestEditScrobbleLinksAndAlbumRules(t *testing.T) {
	e := newEnv(t)
	a := e.artist("A")
	rec1, rec2 := e.recording("One", a), e.recording("Two", a)
	alb1, alb2 := e.release("Album 1", "2000"), e.release("Album 2", "2005")
	e.onRelease(rec1, alb1, 1)
	s := e.scrobble("2024-01-01T10:00:00Z", "A", "One", "Album 1", rec1, alb1)

	// Moving to an album the recording is not on is refused...
	wantErr(t, mustErr(e.ed.MoveScrobbleToRelease(ctx, "a", s, alb2, false)), "not_on_release")
	// ...unless asked to link it, which adds recording_release in the same edit.
	res, err := e.ed.MoveScrobbleToRelease(ctx, "a", s, alb2, true)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE recording_id=? AND release_id=? AND is_primary=0`, rec1, alb2) != 1 {
		t.Fatal("link not added as non-primary")
	}
	if e.str(`SELECT release_id FROM scrobble WHERE id=?`, s) != fmt.Sprint(alb2) {
		t.Fatal("scrobble not moved")
	}
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE release_id=?`, alb2) != 0 {
		t.Fatal("undo left the link")
	}

	// Re-pointing at another recording drops an album it is not on.
	if _, err := e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{RecordingID: nint(rec2)}); err != nil {
		t.Fatal(err)
	}
	if e.str(`SELECT release_id FROM scrobble WHERE id=?`, s) != "" {
		t.Fatal("stale album kept")
	}
	if e.str(`SELECT raw_title FROM scrobble WHERE id=?`, s) != "One" {
		t.Fatal("raw title changed")
	}
	wantErr(t, mustErr(e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{RecordingID: nint(999)})), "unknown_recording")
	// Unlinking entirely.
	if _, err := e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{RecordingID: nnull()}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, mustErr(e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{RecordingID: nnull()})), "no_change")
}

func TestDeleteScrobbleUndoRestoresRawRow(t *testing.T) {
	e := newEnv(t)
	s := e.scrobble("2024-01-01T10:00:00Z", "A", "S", "Alb", nil, nil)
	e.exec(`UPDATE scrobble SET raw_payload='{"x":1}', source='t' WHERE id=?`, s)
	start := e.snapshot()
	r, err := e.ed.DeleteScrobble(ctx, "a", s)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 0 {
		t.Fatal("not deleted")
	}
	if _, err := e.ed.Undo(ctx, "a", r.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != start {
		t.Fatal("restore differs")
	}
}

func TestDeleteScrobblesIsOneUndoableEdit(t *testing.T) {
	e := newEnv(t)
	a := e.scrobble("2024-01-01T10:00:00Z", "A", "S", "Alb", nil, nil)
	b := e.scrobble("2024-01-02T10:00:00Z", "A", "T", "Alb", nil, nil)
	keep := e.scrobble("2024-01-03T10:00:00Z", "A", "U", "Alb", nil, nil)
	start := e.snapshot()
	r, err := e.ed.DeleteScrobbles(ctx, "a", []int64{a, b, a})
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 1 || e.count(`SELECT COUNT(*) FROM scrobble WHERE id=?`, keep) != 1 {
		t.Fatal("wrong scrobbles deleted")
	}
	if _, err := e.ed.Undo(ctx, "a", r.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != start {
		t.Fatal("one undo did not restore every scrobble")
	}
	// A missing id deletes nothing; an empty list is refused.
	if _, err := e.ed.DeleteScrobbles(ctx, "a", []int64{a, 999999}); err == nil {
		t.Fatal("missing id accepted")
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 3 {
		t.Fatal("partial delete")
	}
	if _, err := e.ed.DeleteScrobbles(ctx, "a", nil); err == nil {
		t.Fatal("empty list accepted")
	}
}

func TestEditEntityEverywhereAndAlias(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Beatls")
	rec := e.recording("Hey Jude", a)
	s1 := e.scrobble("2024-01-01T10:00:00Z", "Beatls", "Hey Jude", "", rec, nil)
	s2 := e.scrobble("2024-01-02T10:00:00Z", "Beatls", "Hey Jude", "", rec, nil)
	start := e.snapshot()

	res, err := e.ed.EditEntity(ctx, "a", "artist", a, edit.EditEntity(map[string]any{"name": "The Beatles"}))
	if err != nil {
		t.Fatal(err)
	}
	// Both scrobbles see the new name through the model; no scrobble row changed.
	for _, s := range []int64{s1, s2} {
		v, err := edit.View(ctx, e.db, s)
		if err != nil {
			t.Fatal(err)
		}
		if v.Effective.Artist != "The Beatles" || v.Raw.Artist != "Beatls" {
			t.Fatalf("view: %+v", v)
		}
	}
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE edited_at IS NOT NULL`) != 0 {
		t.Fatal("entity edit rewrote scrobbles")
	}
	// The old spelling now resolves to the entity.
	if r, _ := e.resolve(t, "beatls", "hey jude", ""); r != rec {
		t.Fatalf("old spelling resolves to %d", r)
	}
	wantErr(t, mustErr(e.ed.EditEntity(ctx, "a", "artist", a, edit.EditEntity(map[string]any{"title": "x"}))), "bad_field")
	wantErr(t, mustErr(e.ed.EditEntity(ctx, "a", "artist", a, edit.EditEntity(map[string]any{"name": "  "}))), "bad_value")
	wantErr(t, mustErr(e.ed.EditEntity(ctx, "a", "artist", 99, edit.EditEntity(map[string]any{"name": "x"}))), "not_found")

	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != start {
		t.Fatal("undo did not restore (including alias removal)")
	}
}

// A display name is set and cleared without touching the stored name, and
// undoes like any edit.
func TestDisplayNameLeavesStoredName(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Artist")
	rec := e.recording("Song", a)
	shown := func() string {
		var v sql.NullString
		if err := e.db.QueryRow(`SELECT display_name FROM recording WHERE id = ?`, rec).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.String
	}
	if _, err := e.ed.EditEntity(ctx, "a", "recording", rec, edit.EditEntity(map[string]any{"display_name": " Song (Live) "})); err != nil {
		t.Fatal(err)
	}
	if shown() != "Song (Live)" || e.count(`SELECT COUNT(*) FROM recording WHERE id = ? AND title = 'Song'`, rec) != 1 {
		t.Fatal("display name should leave the stored title")
	}
	if e.count(`SELECT COUNT(*) FROM alias WHERE entity_id = ? AND raw_string LIKE '%live%'`, rec) != 0 {
		t.Fatal("a display name is not a spelling to match")
	}
	clr, err := e.ed.EditEntity(ctx, "a", "recording", rec, edit.EditEntity(map[string]any{"display_name": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if shown() != "" {
		t.Fatal("blank should clear the display name")
	}
	if _, err := e.ed.Undo(ctx, "a", clr.EditID); err != nil || shown() != "Song (Live)" {
		t.Fatalf("undo clear: %v %q", err, shown())
	}
}

// An image chosen by hand is set and reset on its own column and undoes.
func TestSetImage(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Artist")
	img := func() string {
		var v sql.NullString
		if err := e.db.QueryRow(`SELECT image_override FROM artist WHERE id = ?`, a).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.String
	}
	if _, err := e.ed.SetImage(ctx, "a", "artist", a, "artist-1-uabc.png"); err != nil || img() != "artist-1-uabc.png" {
		t.Fatalf("set: %v %q", err, img())
	}
	res, err := e.ed.SetImage(ctx, "a", "artist", a, "")
	if err != nil || img() != "" {
		t.Fatalf("reset: %v %q", err, img())
	}
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil || img() != "artist-1-uabc.png" {
		t.Fatalf("undo: %v %q", err, img())
	}
	wantErr(t, mustErr(e.ed.SetImage(ctx, "a", "recording", 1, "x.png")), "bad_kind")
	if _, err := e.ed.SetImage(ctx, "a", "artist", a, "artist-1-uabc.png"); err != edit.ErrNoChange {
		t.Fatalf("same image: %v", err)
	}
}

func TestMergeRecordingAliasAndUndo(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Artist")
	rA := e.recording("Song (typo)", a)
	rB := e.recording("Song", a)
	alb1, alb2 := e.release("Alb1", "2000"), e.release("Alb2", "2001")
	e.onRelease(rA, alb1, 1)
	e.onRelease(rA, alb2, 0)
	e.onRelease(rB, alb1, 1) // shared album; B already primary
	sA := e.scrobble("2024-01-01T10:00:00Z", "Artist", "Song (typo)", "Alb2", rA, alb2)
	sB := e.scrobble("2024-01-02T10:00:00Z", "Artist", "Song", "Alb1", rB, alb1)
	e.exec(`INSERT INTO external_link(entity_type, entity_id, provider, provider_id, match_method) VALUES ('recording', ?, 'musicbrainz', 'mbid-1', 'manual')`, rA)
	start := e.snapshot()

	res, err := e.ed.Merge(ctx, "a", "recording", rA, rB)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording WHERE id=?`, rA) != 0 {
		t.Fatal("A not deleted")
	}
	if e.str(`SELECT recording_id FROM scrobble WHERE id=?`, sA) != fmt.Sprint(rB) {
		t.Fatal("scrobble not repointed")
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE recording_id=? AND release_id=? AND is_primary=0`, rB, alb2) != 1 {
		t.Fatal("B did not gain A's album as non-primary")
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE recording_id=? AND is_primary=1`, rB) != 1 {
		t.Fatal("B should keep exactly one primary")
	}
	if e.count(`SELECT COUNT(*) FROM external_link WHERE entity_id=? AND entity_type='recording'`, rB) != 1 {
		t.Fatal("external link not moved")
	}
	// Alias resolution: the old name resolves to B, including album.
	if r, rel := e.resolve(t, "ARTIST", " song (typo) ", "alb2"); r != rB || rel != alb2 {
		t.Fatalf("resolve = %d,%d want %d,%d", r, rel, rB, alb2)
	}
	_ = sB

	// Undo refuses once a later edit touched a row the merge repointed.
	if _, err := e.ed.EditScrobble(ctx, "a", sA, edit.ScrobbleEdit{RecordingID: nnull()}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, mustErr(e.ed.Undo(ctx, "a", res.EditID)), "undo_conflict")
	var last int64
	_ = e.db.QueryRow(`SELECT max(id) FROM edit_log`).Scan(&last)
	if _, err := e.ed.Undo(ctx, "a", last); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if got := e.snapshot(); got != start {
		t.Fatalf("merge round trip differs:\n%s\nvs\n%s", got, start)
	}
}

func TestMergeArtistAndRelease(t *testing.T) {
	e := newEnv(t)
	a1, a2 := e.artist("Old Name"), e.artist("New Name")
	r1 := e.recording("Song", a1, a2) // both credited: the A row must just go
	e.recording("Other", a1)
	alb1, alb2 := e.release("Album", "2000"), e.release("Album (Deluxe)", "2001")
	e.exec(`INSERT INTO release_artist(release_id, artist_id) VALUES (?,?)`, alb1, a1)
	e.onRelease(r1, alb1, 1)
	e.onRelease(r1, alb2, 0)
	s := e.scrobble("2024-01-01T10:00:00Z", "Old Name", "Song", "Album", r1, alb1)
	start := e.snapshot()

	m1, err := e.ed.Merge(ctx, "a", "artist", a1, a2)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_artist WHERE artist_id=?`, a2) != 2 || e.count(`SELECT COUNT(*) FROM artist`) != 1 {
		t.Fatal("artist merge result wrong")
	}
	if e.count(`SELECT COUNT(*) FROM release_artist WHERE artist_id=?`, a2) != 1 {
		t.Fatal("release credit not moved")
	}
	// Ingest-style lookup of the old artist string finds B's recording.
	if r, _ := e.resolve(t, "Old Name", "Song", ""); r != r1 {
		t.Fatalf("old artist resolves to %d", r)
	}
	m2, err := e.ed.Merge(ctx, "a", "release", alb1, alb2)
	if err != nil {
		t.Fatal(err)
	}
	if e.str(`SELECT release_id FROM scrobble WHERE id=?`, s) != fmt.Sprint(alb2) {
		t.Fatal("scrobble album not repointed")
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE recording_id=? AND is_primary=1`, r1) != 1 {
		t.Fatal("exactly one primary expected after release merge")
	}
	if _, rel := e.resolve(t, "Old Name", "Song", "Album"); rel != alb2 {
		t.Fatalf("old album resolves to %d", rel)
	}
	wantErr(t, mustErr(e.ed.Merge(ctx, "a", "release", alb2, alb2)), "same_entity")
	wantErr(t, mustErr(e.ed.Merge(ctx, "a", "release", 77, alb2)), "not_found")

	if _, err := e.ed.Undo(ctx, "a", m2.EditID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ed.Undo(ctx, "a", m1.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != start {
		t.Fatal("artist/release merge round trip differs")
	}
}

func TestBulkDryRunConfirmAndUndo(t *testing.T) {
	e := newEnv(t)
	a := e.artist("A")
	rec := e.recording("Song", a)
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, e.scrobble(fmt.Sprintf("2024-01-0%dT10:00:00Z", i+1), "a ", "song", "", nil, nil))
	}
	e.scrobble("2024-01-09T10:00:00Z", "Other", "Thing", "", nil, nil)
	start := e.snapshot()

	f := edit.Filter{RawArtist: "A ", RawTitle: "SONG", Unmatched: true}
	set := edit.ScrobbleEdit{RecordingID: nint(rec)}
	f.RawArtist = "a "
	pv, err := e.ed.BulkDryRun(ctx, f, set)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Matched != 5 || pv.WouldChange != 5 || len(pv.Sample) != 5 {
		t.Fatalf("preview %+v", pv)
	}
	if e.snapshot() != start {
		t.Fatal("dry run wrote data")
	}
	wantErr(t, mustErr(e.ed.BulkApply(ctx, "a", f, set, 3)), "count_changed")
	wantErr(t, mustErr(e.ed.BulkApply(ctx, "a", edit.Filter{}, set, 0)), "filter_required")
	wantErr(t, mustErr(e.ed.BulkApply(ctx, "a", f, edit.ScrobbleEdit{}, 5)), "bad_set")
	if e.snapshot() != start {
		t.Fatal("refused bulk wrote data")
	}
	res, err := e.ed.BulkApply(ctx, "a", f, set, pv.Matched)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE recording_id=?`, rec) != 5 {
		t.Fatal("bulk not applied")
	}
	// A later edit to one of them blocks the whole undo, atomically.
	if _, err := e.ed.EditScrobble(ctx, "a", ids[0], edit.ScrobbleEdit{RecordingID: nnull()}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, mustErr(e.ed.Undo(ctx, "a", res.EditID)), "undo_conflict")
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE recording_id=?`, rec) != 4 {
		t.Fatal("partial undo happened")
	}
	var last int64
	_ = e.db.QueryRow(`SELECT max(id) FROM edit_log`).Scan(&last)
	_, _ = e.ed.Undo(ctx, "a", last)
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != start {
		t.Fatal("bulk round trip differs")
	}

	// Time shift in bulk.
	shift := int64(3600)
	if _, err := e.ed.BulkApply(ctx, "a", edit.Filter{IDs: ids[:2]}, edit.ScrobbleEdit{ShiftSeconds: &shift}, 2); err != nil {
		t.Fatal(err)
	}
	if e.str(`SELECT played_at FROM scrobble WHERE id=?`, ids[0]) != "2024-01-01T11:00:00Z" {
		t.Fatal("shift not applied")
	}
}

func TestBulkLinkReleaseCreatesOneLink(t *testing.T) {
	e := newEnv(t)
	a := e.artist("A")
	rec := e.recording("Song", a)
	alb := e.release("Album", "2000")
	for i := 0; i < 3; i++ {
		e.scrobble(fmt.Sprintf("2024-01-0%dT10:00:00Z", i+1), "A", "Song", "", rec, nil)
	}
	f := edit.Filter{RecordingID: &rec}
	set := edit.ScrobbleEdit{ReleaseID: nint(alb), LinkRelease: true}
	wantErr(t, mustErr(e.ed.BulkApply(ctx, "a", f, edit.ScrobbleEdit{ReleaseID: nint(alb)}, 3)), "not_on_release")
	if _, err := e.ed.BulkApply(ctx, "a", f, set, 3); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE recording_id=? AND is_primary=1`, rec) != 1 ||
		e.count(`SELECT COUNT(*) FROM scrobble WHERE release_id=?`, alb) != 3 {
		t.Fatal("bulk link wrong")
	}
}

func TestAlbumMembershipAndPrimary(t *testing.T) {
	e := newEnv(t)
	a := e.artist("A")
	rec := e.recording("Song", a)
	alb1, alb2, alb3 := e.release("Old", "1999"), e.release("Newer", "2005"), e.release("Newest", "2010")
	start := e.snapshot()

	r1, err := e.ed.AddToRelease(ctx, "a", rec, alb2, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE release_id=? AND is_primary=1`, alb2) != 1 {
		t.Fatal("first album should be primary")
	}
	wantErr(t, mustErr(e.ed.AddToRelease(ctx, "a", rec, alb2, 1, nil)), "already_on_release")
	if _, err := e.ed.AddToRelease(ctx, "a", rec, alb3, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ed.AddToRelease(ctx, "a", rec, alb1, 2, nil); err != nil {
		t.Fatal(err)
	}
	wantErr(t, mustErr(e.ed.SetPrimaryRelease(ctx, "a", rec, 999)), "not_on_release")
	if _, err := e.ed.SetPrimaryRelease(ctx, "a", rec, alb3); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE recording_id=? AND is_primary=1 AND release_id=?`, rec, alb3) != 1 {
		t.Fatal("primary not moved")
	}
	// Removing the primary promotes the earliest remaining album and detaches scrobbles.
	s := e.scrobble("2024-01-01T10:00:00Z", "A", "Song", "Newest", rec, alb3)
	rm, err := e.ed.RemoveFromRelease(ctx, "a", rec, alb3)
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording_release WHERE recording_id=? AND is_primary=1 AND release_id=?`, rec, alb1) != 1 {
		t.Fatal("earliest album not promoted")
	}
	if e.str(`SELECT release_id FROM scrobble WHERE id=?`, s) != "" {
		t.Fatal("scrobble still on removed album")
	}
	if _, err := e.ed.Undo(ctx, "a", rm.EditID); err != nil {
		t.Fatal(err)
	}
	if e.str(`SELECT release_id FROM scrobble WHERE id=?`, s) != fmt.Sprint(alb3) {
		t.Fatal("undo did not reattach scrobble")
	}
	_ = r1
	_ = start
}

func TestResolverUsedByIngestStore(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Artist")
	rA := e.recording("Sonng", a)
	rB := e.recording("Song", a)
	alb := e.release("Album", "2000")
	e.onRelease(rB, alb, 1)
	if _, err := e.ed.Merge(ctx, "a", "recording", rA, rB); err != nil {
		t.Fatal(err)
	}
	// Unlinked store: nothing resolves.
	plain := &ingest.Store{DB: e.db, DedupeTolerance: time.Second}
	if _, _, err := plain.Insert(ctx, []ingest.NewScrobble{{PlayedAt: time.Unix(1700000000, 0), Artist: "Artist", Title: "Sonng"}}); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE recording_id IS NOT NULL`) != 0 {
		t.Fatal("plain store linked a scrobble")
	}
	st := &ingest.Store{DB: e.db, DedupeTolerance: time.Second,
		Identify: func(ctx context.Context, tx *sql.Tx, l ingest.NewScrobble) (int64, int64, error) {
			return (&identity.Resolver{}).Resolve(ctx, tx, identity.Parse(nil, identity.Input{Artist: l.Artist, Title: l.Title, Album: l.Album}))
		}}
	acc, _, err := st.Insert(ctx, []ingest.NewScrobble{
		{PlayedAt: time.Unix(1700001000, 0), Artist: "artist", Title: "SONNG", Album: "album"},
		{PlayedAt: time.Unix(1700002000, 0), Artist: "Artist", Title: "Song"}, // canonical name, no alias needed
		{PlayedAt: time.Unix(1700003000, 0), Artist: "Nobody", Title: "Unknown"},
	})
	if err != nil || acc != 3 {
		t.Fatal(acc, err)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE recording_id=? AND release_id=? AND raw_title='SONNG'`, rB, alb) != 1 {
		t.Fatal("aliased listen not resolved to B + album")
	}
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE recording_id=? AND raw_title='Song' AND release_id IS NULL`, rB) != 1 {
		t.Fatal("canonical-name listen not resolved")
	}
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE recording_id IS NULL`) != 1 { // only the plain-store one
		t.Fatal("every titled listen resolved through identity should have a recording")
	}
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE raw_title='Unknown' AND recording_id IS NOT NULL AND recording_id<>?`, rB) != 1 {
		t.Fatal("an unknown listen should get a recording of its own, never B")
	}
}

// resolve runs a raw listen through the identity resolver, as ingest does, to
// show that aliases written by edits and merges are used for later listens.
func (e *env) resolve(t *testing.T, artist, title, album string) (int64, int64) {
	t.Helper()
	in := identity.Parse(nil, identity.Input{Artist: artist, Title: title, Album: album})
	rec, rel, err := (&identity.Resolver{}).Resolve(ctx, e.db, in)
	if err != nil {
		t.Fatal(err)
	}
	return rec, rel
}

func TestEditScrobbleToNewTrackAndAlbumUndoes(t *testing.T) {
	e := newEnv(t)
	a := e.artist("Old Artist")
	rec := e.recording("Old Title", a)
	e.artist("Existing")
	s := e.scrobble("2024-01-01T10:00:00Z", "Old Artist", "Old Title", "", rec, nil)
	start := e.snapshot()

	res, err := e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{
		NewRecording: &edit.NewRecording{Title: " New Title ", Artists: []edit.CreditIn{{Name: "Existing", JoinPhrase: " & "}, {Name: "Brand New"}}},
		NewRelease:   "New Album",
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM artist`) != 3 {
		t.Fatal("existing artist should be reused and one created")
	}
	if e.str(`SELECT r.title FROM scrobble s JOIN recording r ON r.id=s.recording_id WHERE s.id=?`, s) != "New Title" {
		t.Fatal("scrobble not pointed at the new track")
	}
	if e.str(`SELECT rel.title FROM scrobble s JOIN release rel ON rel.id=s.release_id WHERE s.id=?`, s) != "New Album" {
		t.Fatal("scrobble not pointed at the new album")
	}
	if e.count(`SELECT COUNT(*) FROM recording_release rr JOIN scrobble s ON s.recording_id=rr.recording_id AND s.release_id=rr.release_id WHERE s.id=? AND rr.is_primary=1`, s) != 1 {
		t.Fatal("new track not primary on the new album")
	}
	if e.count(`SELECT COUNT(*) FROM release_artist ra JOIN scrobble s ON s.release_id=ra.release_id WHERE s.id=?`, s) != 2 {
		t.Fatal("album should carry the track's artists")
	}
	if e.str(`SELECT raw_title FROM scrobble WHERE id=?`, s) != "Old Title" {
		t.Fatal("raw title changed")
	}
	if _, err := e.ed.Undo(ctx, "a", res.EditID); err != nil {
		t.Fatal(err)
	}
	if e.snapshot() != start {
		t.Fatal("undo did not restore")
	}

	wantErr(t, mustErr(e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{NewRecording: &edit.NewRecording{Title: "x"}})), "bad_value")
	wantErr(t, mustErr(e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{RecordingID: nint(rec), NewRecording: &edit.NewRecording{Title: "x", Artists: []edit.CreditIn{{Name: "y"}}}})), "bad_request")
}

func TestEditScrobbleNewTrackReusesExactMatch(t *testing.T) {
	e := newEnv(t)
	a, b := e.artist("A"), e.artist("B")
	rec := e.recording("Song", a, b)
	other := e.recording("Other", a)
	s := e.scrobble("2024-01-01T10:00:00Z", "A", "Other", "", other, nil)
	if _, err := e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{NewRecording: &edit.NewRecording{Title: "song", Artists: []edit.CreditIn{{ArtistID: a}, {Name: "B"}}}}); err != nil {
		t.Fatal(err)
	}
	if e.str(`SELECT recording_id FROM scrobble WHERE id=?`, s) != fmt.Sprint(rec) || e.count(`SELECT COUNT(*) FROM recording`) != 2 {
		t.Fatal("exact match should be reused, not duplicated")
	}
	// Different artist order is a different track.
	if _, err := e.ed.EditScrobble(ctx, "a", s, edit.ScrobbleEdit{NewRecording: &edit.NewRecording{Title: "Song", Artists: []edit.CreditIn{{ArtistID: b}, {ArtistID: a}}}}); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT COUNT(*) FROM recording`) != 3 {
		t.Fatal("different artist order should create a track")
	}
}
