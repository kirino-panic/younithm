package imports

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"younithm/internal/importers"
)

func TestNewestN(t *testing.T) {
	k := &newestN{n: 3}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, d := range []int{5, 1, 9, 3, 7, 2} {
		k.add(importers.Listen{PlayedAt: base.AddDate(0, 0, d)})
	}
	got := k.sorted()
	if len(got) != 3 || got[0].PlayedAt != base.AddDate(0, 0, 5) || got[2].PlayedAt != base.AddDate(0, 0, 9) {
		t.Fatalf("got %v", got)
	}
}

// The per-import debug limit keeps only the newest N listens of the file, is
// frozen on the job and shown in the preview, and leaves other imports alone.
func TestDebugLimitPerImport(t *testing.T) {
	e := newEnv(t, Options{})
	var sb strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&sb, `{"listened_at":%d,"track_metadata":{"artist_name":"A","track_name":"Song %d"}}`+"\n", 1800000000+i*600, i)
	}
	prev, done := e.previewAndApply("lb.jsonl", []byte(sb.String()), "?debug_limit=5")
	ps := parseSummary(t, prev.Preview)
	if ps.DebugLimit != 5 || ps.Counts.Parsed != 5 || prev.Options.DebugLimit != 5 {
		t.Fatalf("preview should show the limit and 5 listens: %+v options=%+v", ps.Counts, prev.Options)
	}
	if done.Progress.Inserted != 5 || e.count(`SELECT COUNT(*) FROM scrobble`) != 5 {
		t.Fatalf("inserted %d", done.Progress.Inserted)
	}
	// They are the five newest: Song 26..30.
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE raw_title IN ('Song 26','Song 27','Song 28','Song 29','Song 30')`) != 5 {
		t.Fatal("kept listens are not the newest five")
	}
	// A normal import of the same file afterwards takes the rest (the limit is per job).
	_, full := e.previewAndApply("lb2.jsonl", []byte(sb.String()), "")
	if full.Progress.Inserted != 25 || full.Options.DebugLimit != 0 {
		t.Fatalf("unlimited import inserted %d, options %+v", full.Progress.Inserted, full.Options)
	}
	if code, _ := e.upload("x.jsonl", []byte(sb.String()), "?debug_limit=abc"); code != 400 {
		t.Fatalf("bad debug_limit: %d", code)
	}
}
