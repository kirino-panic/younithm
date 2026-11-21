package enrich

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/db"
	"younithm/internal/providers"
	"younithm/internal/providers/match"
	"younithm/internal/providers/snapshot"
)

type fakeProvider struct {
	tracks []providers.Track
	err    error
	calls  int
}

func (f *fakeProvider) Name() string { return providers.MusicBrainz }
func (f *fakeProvider) SearchTrack(context.Context, providers.TrackQuery) ([]providers.Track, error) {
	f.calls++
	return f.tracks, f.err
}
func (f *fakeProvider) GetTrack(context.Context, string) (*providers.Track, error) {
	return nil, providers.ErrUnsupported
}
func (f *fakeProvider) GetArtwork(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}
func (f *fakeProvider) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, providers.ErrUnsupported
}

func track(id, title, artist string) providers.Track {
	return providers.Track{Provider: providers.MusicBrainz, ProviderID: id, Title: title,
		Artists:  []providers.Artist{{ProviderID: "a-" + id, Name: artist}},
		Releases: []providers.ReleaseRef{{ProviderID: "r-" + id, Title: "Album"}}}
}

type env struct {
	db  *sql.DB
	m   *Manager
	fp  *fakeProvider
	now time.Time
	h   http.Handler
}

func setup(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	e := &env{db: d, fp: &fakeProvider{}, now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	snaps := snapshot.New(d)
	e.m = NewManager(d, match.New(providers.NewRegistry(e.fp), match.Config{}), snaps,
		Options{Now: func() time.Time { return e.now }, MaxAttempts: 3})
	r := chi.NewRouter()
	NewReview(d, e.m, snaps).Mount(r, func(h http.Handler) http.Handler { return h })
	e.h = r
	exec(t, d, `INSERT INTO artist(id,name) VALUES (1,'The Band')`)
	exec(t, d, `INSERT INTO recording(id,title) VALUES (1,'Great Song')`)
	exec(t, d, `INSERT INTO recording_artist(recording_id,artist_id) VALUES (1,1)`)
	return e
}

func exec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *env) run(t *testing.T) {
	t.Helper()
	if _, err := e.m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *env) job(t *testing.T) (status string, attempts int, outcome, lastErr sql.NullString, next string) {
	t.Helper()
	err := e.db.QueryRow(`SELECT status, attempts, outcome, last_error, next_attempt_at FROM enrichment_job WHERE recording_id=1`).
		Scan(&status, &attempts, &outcome, &lastErr, &next)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func (e *env) call(t *testing.T, method, path string, out any) int {
	t.Helper()
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code
}

func (e *env) links(t *testing.T, status string) int {
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=1 AND status=?`, status).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConfidentMatchBecomesAutoLinkAndCachesSnapshot(t *testing.T) {
	e := setup(t)
	e.fp.tracks = []providers.Track{track("mb-1", "Great Song", "The Band")}
	e.run(t)
	st, _, out, _, _ := e.job(t)
	if st != StatusDone || out.String != OutcomeLinked || e.links(t, "auto") != 1 {
		t.Fatalf("status=%s outcome=%v auto=%d", st, out, e.links(t, "auto"))
	}
	if s, _ := snapshot.New(e.db).Get(context.Background(), providers.MusicBrainz, "mb-1", snapshot.KindRecording); s == nil {
		t.Fatal("snapshot not cached")
	}
	// A linked recording is not queued again, even past the retry window.
	e.now = e.now.Add(30 * 24 * time.Hour)
	if n, _ := e.m.Sweep(context.Background()); n != 0 {
		t.Fatalf("sweep requeued a linked recording: %d", n)
	}
}

func TestAmbiguousMatchGoesToReviewQueue(t *testing.T) {
	e := setup(t)
	e.fp.tracks = []providers.Track{track("mb-1", "Great Song", "The Band"), track("mb-2", "Great Song", "The Band")}
	e.run(t)
	if _, _, out, _, _ := e.job(t); out.String != OutcomeCandidates || e.links(t, "candidate") != 2 {
		t.Fatalf("outcome=%v candidates=%d", out, e.links(t, "candidate"))
	}
	var q struct {
		Items []reviewItem `json:"items"`
	}
	if c := e.call(t, "GET", "/api/v1/admin/review", &q); c != 200 || len(q.Items) != 1 ||
		len(q.Items[0].Candidates) != 2 || q.Items[0].Candidates[0].Title != "Great Song" ||
		q.Items[0].Artist != "The Band" {
		t.Fatalf("review list %d %+v", c, q)
	}
	var st map[string]int64
	e.call(t, "GET", "/api/v1/admin/enrichment", &st)
	if st["review_pending"] != 1 || st["done"] != 1 {
		t.Fatalf("status %+v", st)
	}

	id := q.Items[0].Candidates[0].LinkID
	if c := e.call(t, "POST", "/api/v1/admin/review/links/"+itoa(id)+"/confirm", nil); c != 200 {
		t.Fatalf("confirm %d", c)
	}
	var pinned int
	e.db.QueryRow(`SELECT pinned FROM external_link WHERE id=?`, id).Scan(&pinned)
	if e.links(t, "confirmed") != 1 || e.links(t, "candidate") != 0 || pinned != 1 {
		t.Fatal("confirm should pin one link and drop its siblings")
	}
	if c := e.call(t, "POST", "/api/v1/admin/review/links/"+itoa(id)+"/confirm", nil); c != 409 {
		t.Fatalf("second confirm: %d", c)
	}
}

func TestRejectedCandidateIsNeverProposedAgain(t *testing.T) {
	e := setup(t)
	e.fp.tracks = []providers.Track{track("mb-1", "Great Song", "The Band"), track("mb-2", "Great Song", "The Band")}
	e.run(t)
	var id int64
	e.db.QueryRow(`SELECT id FROM external_link WHERE provider_id='mb-1'`).Scan(&id)
	if c := e.call(t, "POST", "/api/v1/admin/review/links/"+itoa(id)+"/reject", nil); c != 200 {
		t.Fatalf("reject %d", c)
	}
	if c := e.call(t, "POST", "/api/v1/admin/review/recordings/1/rematch", nil); c != 202 {
		t.Fatalf("rematch %d", c)
	}
	e.now = e.now.Add(time.Second)
	if _, err := e.m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM external_link WHERE provider_id='mb-1'`).Scan(&n)
	if n != 0 {
		t.Fatal("rejected candidate came back")
	}
	if e.call(t, "POST", "/api/v1/admin/review/recordings/99/rematch", nil) != 404 {
		t.Fatal("rematch of unknown recording should 404")
	}
}

func TestRateLimitRequeuesWithoutCountingAnAttempt(t *testing.T) {
	e := setup(t)
	e.fp.err = &providers.RateLimitedError{Provider: "musicbrainz", RetryAfter: 300}
	e.run(t)
	st, attempts, _, _, next := e.job(t)
	want := e.now.Add(5 * time.Minute).Format(tsLayout)
	if st != StatusQueued || attempts != 0 || next != want {
		t.Fatalf("status=%s attempts=%d next=%s want %s", st, attempts, next, want)
	}
	if n, _ := e.m.ProcessDue(context.Background()); n != 0 {
		t.Fatal("job ran before its retry time")
	}
}

func TestErrorsBackOffThenFail(t *testing.T) {
	e := setup(t)
	e.fp.err = errors.New("boom")
	e.run(t)
	for i := 0; i < 3; i++ {
		e.now = e.now.Add(2 * time.Hour)
		if _, err := e.m.ProcessDue(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	st, attempts, _, lastErr, _ := e.job(t)
	if st != StatusFailed || attempts != 3 || lastErr.String != "boom" {
		t.Fatalf("status=%s attempts=%d err=%v", st, attempts, lastErr)
	}
}

func TestNoMatchRetriedOnlyAfterRetryWindow(t *testing.T) {
	e := setup(t)
	e.run(t)
	if _, _, out, _, _ := e.job(t); out.String != OutcomeNoMatch {
		t.Fatalf("outcome %v", out)
	}
	calls := e.fp.calls
	e.now = e.now.Add(24 * time.Hour)
	if n, _ := e.m.Sweep(context.Background()); n != 0 {
		t.Fatalf("requeued inside the retry window: %d", n)
	}
	e.now = e.now.Add(7 * 24 * time.Hour)
	e.run(t)
	if e.fp.calls != calls+1 {
		t.Fatalf("expected one retry, calls %d -> %d", calls, e.fp.calls)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestUnavailableProviderDefersInsteadOfNoMatch(t *testing.T) {
	e := setup(t)
	e.fp.err = providers.ErrNotConfigured
	e.run(t)
	st, attempts, out, _, next := e.job(t)
	if st != StatusQueued || attempts != 0 || out.Valid || next != e.now.Add(time.Hour).Format(tsLayout) {
		t.Fatalf("status=%s attempts=%d outcome=%v next=%s", st, attempts, out, next)
	}
	// Credentials arrive later: the deferred job now matches.
	e.fp.err = nil
	e.fp.tracks = []providers.Track{track("mb-1", "Great Song", "The Band")}
	e.now = e.now.Add(time.Hour)
	if _, err := e.m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, out, _, _ := e.job(t); out.String != OutcomeLinked {
		t.Fatalf("outcome %v", out)
	}
}

// An import finishing while the queue is being worked adds its recordings to
// the run in progress instead of waiting for the queue to drain.
func TestSweepNowJoinsRunningQueue(t *testing.T) {
	e := setup(t)
	if _, err := e.m.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	exec(t, e.db, `INSERT INTO recording(id,title) VALUES (2,'New Import')`)
	exec(t, e.db, `INSERT INTO recording_artist(recording_id,artist_id) VALUES (2,1)`)
	e.m.SweepNow()
	n, err := e.m.ProcessDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var queued int
	e.db.QueryRow(`SELECT COUNT(*) FROM enrichment_job WHERE recording_id=2`).Scan(&queued)
	if n != 2 || queued != 1 {
		t.Fatalf("processed %d, job for new recording: %d", n, queued)
	}
}
