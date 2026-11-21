package identity

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"younithm/internal/db"
)

func waitDone(t *testing.T, b *Backfill, dry bool) *BackfillStatus {
	t.Helper()
	for i := 0; i < 200; i++ {
		s, err := b.Latest(context.Background(), dry)
		if err != nil {
			t.Fatal(err)
		}
		if s != nil && s.Status != "running" {
			return s
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("backfill did not finish")
	return nil
}

func TestBackfillPreviewThenRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	d, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	// An existing, already linked scrobble (and its entities), a rich LB one,
	// and two sparse ones that name the same artist.
	res, _ := d.Exec(`INSERT INTO artist (name) VALUES ('Old')`)
	aid, _ := res.LastInsertId()
	res, _ = d.Exec(`INSERT INTO recording (title) VALUES ('Linked')`)
	rid, _ := res.LastInsertId()
	d.Exec(`INSERT INTO recording_artist (recording_id, artist_id) VALUES (?,?)`, rid, aid)
	ins := func(artist, title, payload string, rec any) {
		t.Helper()
		var p any
		if payload != "" {
			p = payload
		}
		if _, err := d.Exec(`INSERT INTO scrobble (played_at, raw_artist, raw_title, raw_payload, recording_id) VALUES (?,?,?,?,?)`,
			time.Now().UTC().Format(time.RFC3339), artist, title, p, rec); err != nil {
			t.Fatal(err)
		}
	}
	ins("Old", "Linked", "", rid)
	ins("A, B", "Collab", `{"track_metadata":{"artist_name":"A, B","track_name":"Collab","additional_info":{"artist_names":["A","B"],"spotify_id":"https://open.spotify.com/track/t1"}}}`, nil)
	ins("Solo", "One", "", nil)
	ins("solo", "Two", "", nil)

	b := NewBackfill(context.Background(), d, filepath.Join(dir, "younithm.db"))

	if _, err := b.Start(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	prev := waitDone(t, b, true)
	if prev.Status != "done" || prev.Report.Processed != 4 || prev.Report.NewlyLinked != 3 {
		t.Fatalf("preview: %+v", prev)
	}
	// Dry run changes nothing in the real database.
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM artist`).Scan(&n)
	if n != 1 {
		t.Fatalf("dry run created artists in the real db: %d", n)
	}
	d.QueryRow(`SELECT COUNT(*) FROM scrobble WHERE recording_id IS NULL`).Scan(&n)
	if n != 3 {
		t.Fatalf("dry run linked scrobbles: %d unlinked", n)
	}

	if _, err := b.Start(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	run := waitDone(t, b, false)
	if run.Status != "done" || run.Report.NewlyLinked != 3 || run.Report.ArtistsCreated != prev.Report.ArtistsCreated {
		t.Fatalf("run: %+v (preview %+v)", run, prev)
	}
	check := func(q string, want int) {
		t.Helper()
		var got int
		if err := d.QueryRow(q).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	check(`SELECT COUNT(*) FROM scrobble WHERE recording_id IS NULL`, 0)
	check(`SELECT COUNT(*) FROM artist`, 4)                        // Old, A, B, Solo (shared by both sparse scrobbles)
	check(`SELECT COUNT(*) FROM recording WHERE id=`+itoa(rid), 1) // existing recording kept
	check(`SELECT COUNT(*) FROM recording_artist WHERE recording_id=`+itoa(rid), 1)

	// Running again is a no-op for entities.
	if _, err := b.Start(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	again := waitDone(t, b, false)
	if again.Report.ArtistsCreated != 0 || again.Report.RecordingsCreated != 0 {
		t.Fatalf("second run created entities: %+v", again.Report)
	}
}

func TestBackfillRefusesConcurrentStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	d, _ := db.Open(dir)
	defer d.Close()
	db.Migrate(d)
	b := NewBackfill(context.Background(), d, filepath.Join(dir, "younithm.db"))
	b.cancel = func() {}
	if _, err := b.Start(context.Background(), false, false); err != ErrBackfillRunning {
		t.Fatalf("got %v", err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
