package snapshot

import (
	"context"
	"testing"
	"time"

	"younithm/internal/db"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	return New(d)
}

func TestPutGetFreshness(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return base }

	if e, err := s.Get(ctx, "musicbrainz", "x", KindRecording); e != nil || err != nil {
		t.Fatalf("want miss, got %v %v", e, err)
	}
	if err := s.Put(ctx, "musicbrainz", "x", KindRecording, []byte(`{"a":1}`), time.Hour); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Get(ctx, "musicbrainz", "x", KindRecording)
	if e == nil || string(e.Raw) != `{"a":1}` || e.NotFound || e.FetchCount != 1 {
		t.Fatalf("bad entry %+v", e)
	}
	if !e.Fresh(base.Add(30*time.Minute)) || e.Fresh(base.Add(2*time.Hour)) {
		t.Fatal("freshness wrong")
	}

	// Overwrite with a negative entry bumps fetch_count.
	if err := s.PutNotFound(ctx, "musicbrainz", "x", KindRecording, 0); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Get(ctx, "musicbrainz", "x", KindRecording)
	if !e.NotFound || e.FetchCount != 2 {
		t.Fatalf("bad negative entry %+v", e)
	}

	n, err := s.PurgeExpired(ctx, 0)
	if err != nil || n != 0 {
		t.Fatalf("purge %d %v", n, err)
	}
	s.Now = func() time.Time { return base.Add(30 * 24 * time.Hour) }
	if n, _ = s.PurgeExpired(ctx, 0); n != 1 {
		t.Fatalf("purge = %d", n)
	}
}
