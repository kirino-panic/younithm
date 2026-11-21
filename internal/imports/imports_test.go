package imports

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/db"
	"younithm/internal/ingest"
)

type env struct {
	t     *testing.T
	pools *db.Pools
	m     *Manager
	h     http.Handler
	ctx   context.Context
}

func newEnv(t *testing.T, opt Options) *env {
	t.Helper()
	pools, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pools.Close() })
	if err := db.Migrate(pools.Write); err != nil {
		t.Fatal(err)
	}
	opt.Dir = filepath.Join(t.TempDir(), "imports")
	m := NewManager(pools.Read, pools.Write, opt)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	id := func(h http.Handler) http.Handler { return h }
	m.MountWith(r, id, id)
	return &env{t: t, pools: pools, m: m, h: r, ctx: ctx}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "importers", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) upload(name string, data []byte, query string) (int, Job) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", "/admin/imports/"+query, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	var j Job
	_ = json.Unmarshal(w.Body.Bytes(), &j)
	return w.Code, j
}

func (e *env) post(path string) (int, Job) {
	req := httptest.NewRequest("POST", path, nil)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	var j Job
	_ = json.Unmarshal(w.Body.Bytes(), &j)
	return w.Code, j
}

func (e *env) wait(id int64, want ...string) Job {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		j, err := e.m.store.Get(e.ctx, id)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, w := range want {
			if j.Status == w {
				return j
			}
		}
		if j.terminal() || time.Now().After(deadline) {
			e.t.Fatalf("job %d: status %s (want %v), error %q", id, j.Status, want, j.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) previewAndApply(name string, data []byte, query string) (Job, Job) {
	e.t.Helper()
	code, j := e.upload(name, data, query)
	if code != 201 {
		e.t.Fatalf("upload: %d", code)
	}
	prev := e.wait(j.ID, StatusAwaiting)
	if code, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm", j.ID)); code != 200 {
		e.t.Fatalf("confirm: %d", code)
	}
	return prev, e.wait(j.ID, StatusDone)
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pools.Read.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func parseSummary(t *testing.T, raw json.RawMessage) Summary {
	t.Helper()
	var s Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Every fixture format goes through auto-detect, preview and apply.
func TestAllFormatsEndToEnd(t *testing.T) {
	cases := []struct {
		file, format string
		wantNew      int64
	}{
		{"listenbrainz.jsonl", "listenbrainz", 2},
		{"lastfm.csv", "lastfm", -1},
		{"lastfm.json", "lastfm", 2},
		{"spotify.json", "spotify", -1},
		{"maloja.json", "maloja", -1},
		{"koito.json", "koito", -1},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			e := newEnv(t, Options{})
			prev, done := e.previewAndApply(c.file, fixture(t, c.file), "")
			if prev.Format != c.format || !prev.FormatDetected {
				t.Fatalf("detected %q (detected=%v), want %q", prev.Format, prev.FormatDetected, c.format)
			}
			s := parseSummary(t, prev.Preview)
			if s.Counts.New == 0 || s.Counts.New != done.Progress.Inserted {
				t.Fatalf("preview new %d vs inserted %d", s.Counts.New, done.Progress.Inserted)
			}
			if c.wantNew >= 0 && s.Counts.New != c.wantNew {
				t.Fatalf("new = %d, want %d", s.Counts.New, c.wantNew)
			}
			if n := e.count(`SELECT COUNT(*) FROM scrobble WHERE import_job_id=?`, done.ID); int64(n) != done.Progress.Inserted {
				t.Fatalf("stored %d, inserted %d", n, done.Progress.Inserted)
			}
			if len(s.Sample) == 0 || s.Dedupe.Explanation == "" {
				t.Fatalf("preview lacks sample/explanation: %+v", s)
			}
		})
	}
}

func TestReimportIsIdempotent(t *testing.T) {
	e := newEnv(t, Options{})
	data := fixture(t, "listenbrainz.jsonl")
	_, first := e.previewAndApply("a.jsonl", data, "")
	n1 := e.count(`SELECT COUNT(*) FROM scrobble`)
	recs := e.count(`SELECT COUNT(*) FROM recording`)
	if n1 != 2 || first.Progress.Inserted != 2 {
		t.Fatalf("first import stored %d", n1)
	}

	prev, second := e.previewAndApply("a-again.jsonl", data, "")
	s := parseSummary(t, prev.Preview)
	if s.Counts.New != 0 || s.Counts.Duplicate != 2 || s.Dups.ExistingExact != 2 {
		t.Fatalf("re-import preview: %+v dups=%+v", s.Counts, s.Dups)
	}
	if second.Progress.Inserted != 0 || e.count(`SELECT COUNT(*) FROM scrobble`) != n1 || e.count(`SELECT COUNT(*) FROM recording`) != recs {
		t.Fatalf("re-import changed the database")
	}
}

func TestImportResolvesThroughIdentity(t *testing.T) {
	e := newEnv(t, Options{})
	prev, _ := e.previewAndApply("a.jsonl", fixture(t, "listenbrainz.jsonl"), "")
	s := parseSummary(t, prev.Preview)
	if s.Matching[matchByID] != 2 || s.Counts.Unmatched != 0 {
		t.Fatalf("matching: %+v", s.Matching)
	}
	if e.count(`SELECT COUNT(*) FROM recording`) != 2 || e.count(`SELECT COUNT(*) FROM artist`) != 2 ||
		e.count(`SELECT COUNT(*) FROM external_link WHERE provider='musicbrainz' AND entity_type='recording'`) != 2 ||
		e.count(`SELECT COUNT(*) FROM recording_release WHERE is_primary=1`) != 1 ||
		e.count(`SELECT COUNT(*) FROM scrobble WHERE recording_id IS NOT NULL AND release_id IS NOT NULL`) != 1 {
		t.Fatal("entities not created as expected")
	}

	// A different file with the same MBID reuses the recording; a listen with
	// the same names and no id matches by the single-match name rule; a listen
	// of an unknown track still gets a recording (every listen maps to one).
	lb := `{"listened_at":1800000000,"track_metadata":{"artist_name":"Test Artist","track_name":"Test Song","additional_info":{"recording_mbid":"aaaaaaaa-0000-0000-0000-000000000001"}}}` + "\n" +
		`{"listened_at":1800000500,"track_metadata":{"artist_name":"test artist","track_name":"TEST SONG"}}` + "\n" +
		`{"listened_at":1800001000,"track_metadata":{"artist_name":"Nobody","track_name":"Nothing"}}` + "\n"
	prev2, _ := e.previewAndApply("b.jsonl", []byte(lb), "")
	s2 := parseSummary(t, prev2.Preview)
	if s2.Matching[matchByID] != 1 || s2.Matching[matchByName] != 2 || s2.Counts.Unmatched != 2 {
		t.Fatalf("matching: %+v counts %+v", s2.Matching, s2.Counts)
	}
	if e.count(`SELECT COUNT(*) FROM recording`) != 3 {
		t.Fatalf("recordings = %d, want 3 (one new for the unknown track)", e.count(`SELECT COUNT(*) FROM recording`))
	}
	if e.count(`SELECT COUNT(*) FROM scrobble WHERE recording_id IS NULL`) != 0 {
		t.Fatal("an imported scrobble was left without a recording")
	}
}

func TestFuzzyConsentIsPerImportAndOnlyForListensWithoutIDs(t *testing.T) {
	e := newEnv(t, Options{})
	names := []byte(`{"listened_at":1800001000,"track_metadata":{"artist_name":"Nobody","track_name":"Nothing"}}` + "\n")
	byID := []byte(`{"listened_at":1800002000,"track_metadata":{"artist_name":"Someone","track_name":"Else","additional_info":{"spotify_id":"https://open.spotify.com/track/abc"}}}` + "\n")
	e.previewAndApply("names.jsonl", names, "")
	e.previewAndApply("id.jsonl", byID, "")
	if e.count(`SELECT COUNT(*) FROM recording WHERE title='Nothing' AND fuzzy_consent=0`) != 1 {
		t.Fatal("names-only recording should start without fuzzy consent")
	}
	if e.count(`SELECT COUNT(*) FROM recording WHERE title='Else' AND fuzzy_consent=1`) != 1 {
		t.Fatal("a listen with an id needs no consent")
	}
	// A later import that consents sets it for the recording it matches.
	again := []byte(`{"listened_at":1800009000,"track_metadata":{"artist_name":"Nobody","track_name":"Nothing"}}` + "\n")
	e.previewAndApply("again.jsonl", again, "?fuzzy_consent=true")
	if e.count(`SELECT COUNT(*) FROM recording WHERE title='Nothing' AND fuzzy_consent=1`) != 1 {
		t.Fatal("consenting import did not set fuzzy consent")
	}
}

func TestDedupeWindow(t *testing.T) {
	// Seed a live scrobble through the same write path ingest uses.
	seed := func(e *env) {
		st := &ingest.Store{DB: e.pools.Write, DedupeTolerance: 2 * time.Second}
		if a, _, err := st.Insert(e.ctx, []ingest.NewScrobble{{PlayedAt: time.Unix(1700000000, 0), Artist: "Test Artist", Title: "Test Song", Source: "live"}}); err != nil || a != 1 {
			t.Fatal(a, err)
		}
	}
	imp := func(at int64) []byte {
		return []byte(fmt.Sprintf(`{"listened_at":%d,"track_metadata":{"artist_name":"TEST  artist","track_name":"test song"}}`+"\n", at))
	}

	e := newEnv(t, Options{ImportWindow: 30 * time.Second})
	seed(e)
	// 10s apart: inside the 30s import window, outside the 2s live window.
	prev, _ := e.previewAndApply("near.jsonl", imp(1700000010), "")
	s := parseSummary(t, prev.Preview)
	if s.Counts.Duplicate != 1 || s.Dups.ExistingWithinWindow != 1 || !strings.Contains(s.Sample[0].Reason, "10s earlier") {
		t.Fatalf("near: %+v %+v %+v", s.Counts, s.Dups, s.Sample)
	}
	if prev.Options.DedupeWindowSec != 30 || !strings.Contains(s.Dedupe.Explanation, "30s") {
		t.Fatalf("window not explained: %+v", s.Dedupe)
	}
	// 2 minutes apart is a different play.
	prev, _ = e.previewAndApply("far.jsonl", imp(1700000120), "")
	if s := parseSummary(t, prev.Preview); s.Counts.New != 1 {
		t.Fatalf("far: %+v", s.Counts)
	}
	// A per-upload window of 5m catches it; the floor is the live window.
	code, j := e.upload("wide.jsonl", imp(1700000200), "?dedupe_window=5m")
	if code != 201 || j.Options.DedupeWindowSec != 300 {
		t.Fatalf("window override: %d %+v", code, j.Options)
	}
	if s := parseSummary(t, e.wait(j.ID, StatusAwaiting).Preview); s.Counts.Duplicate != 1 {
		t.Fatalf("wide: %+v", s.Counts)
	}
	code, j = e.upload("tiny.jsonl", imp(1700000400), "?dedupe_window=1ms")
	if code != 201 || j.Options.DedupeWindowSec != 2 {
		t.Fatalf("floor: %+v", j.Options)
	}
}

func TestInFileDuplicatesAndKeptRepeats(t *testing.T) {
	e := newEnv(t, Options{})
	row := func(at int64) string {
		return fmt.Sprintf(`{"listened_at":%d,"track_metadata":{"artist_name":"A","track_name":"T"}}`+"\n", at)
	}
	// identical x2 collapse; a repeat one second later is a real play.
	prev, _ := e.previewAndApply("f.jsonl", []byte(row(1700000000)+row(1700000000)+row(1700000001)), "")
	s := parseSummary(t, prev.Preview)
	if s.Counts.New != 2 || s.Dups.InFile != 1 {
		t.Fatalf("%+v %+v", s.Counts, s.Dups)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 2 {
		t.Fatal("stored count")
	}
}

func TestPayloadIsScrubbed(t *testing.T) {
	e := newEnv(t, Options{})
	sp := `[{"ts":"2023-11-14T22:17:20Z","platform":"x","ms_played":200000,"conn_country":"NL","ip_addr":"203.0.113.9","ip_addr_decrypted":"203.0.113.9",` +
		`"master_metadata_track_name":"S","master_metadata_album_artist_name":"A","master_metadata_album_album_name":"Al","spotify_track_uri":"spotify:track:ABC"}]`
	e.previewAndApply("s.json", []byte(sp), "")
	var payload string
	if err := e.pools.Read.QueryRow(`SELECT raw_payload FROM scrobble`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "203.0.113.9") || strings.Contains(payload, "ip_addr") || strings.Contains(payload, "conn_country") {
		t.Fatalf("PII kept: %s", payload)
	}
	if !strings.Contains(payload, `"ms_played":200000`) {
		t.Fatalf("payload lost data: %s", payload)
	}
	var src string
	_ = e.pools.Read.QueryRow(`SELECT source FROM scrobble`).Scan(&src)
	if src != "import:spotify" {
		t.Fatalf("source %q", src)
	}
}

func TestSanitizePayload(t *testing.T) {
	in := `{"a":1,"IP_Addr":"x","nested":{"ip_addr_decrypted":"y","keep":[{"user_name":"u","n":1.50}]},"big":12345678901234567890}`
	got := string(SanitizePayload(json.RawMessage(in)))
	for _, bad := range []string{"IP_Addr", "ip_addr", "user_name"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%s in %s", bad, got)
		}
	}
	if !strings.Contains(got, "12345678901234567890") || !strings.Contains(got, `"keep"`) {
		t.Fatalf("lost data: %s", got)
	}
	if SanitizePayload(json.RawMessage("{broken")) != nil {
		t.Fatal("unparseable payload must be dropped")
	}
}

func TestZipUploads(t *testing.T) {
	mkzip := func(files map[string][]byte) []byte {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		for n, d := range files {
			w, _ := zw.Create(n)
			_, _ = w.Write(d)
		}
		zw.Close()
		return b.Bytes()
	}
	e := newEnv(t, Options{})
	lb := mkzip(map[string][]byte{
		"user.json":                 []byte(`{"user":"x"}`),
		"listens/2023/11.jsonl":     fixture(t, "listenbrainz.jsonl"),
		"feedback.jsonl":            []byte(`{"x":1}`),
		"listens/2023/2.jsonl":      []byte(`{"listened_at":1675000000,"track_metadata":{"artist_name":"Z","track_name":"Y"}}` + "\n"),
		"pinned_recording.jsonl":    []byte(``),
		"__MACOSX/listens/junk.txt": []byte(`x`),
	})
	prev, _ := e.previewAndApply("listenbrainz_x.zip", lb, "")
	if prev.Format != "listenbrainz" || parseSummary(t, prev.Preview).Counts.New != 3 {
		t.Fatalf("lb zip: %s %+v", prev.Format, parseSummary(t, prev.Preview).Counts)
	}

	sp := mkzip(map[string][]byte{
		"MyData/Streaming_History_Audio_2023_0.json": fixture(t, "spotify.json"),
		"MyData/Streaming_History_Video_2023.json":   []byte(`[{"junk":true}]`),
		"MyData/ReadMeFirst.pdf":                     []byte(`%PDF`),
	})
	e2 := newEnv(t, Options{})
	prev, _ = e2.previewAndApply("my_spotify_data.zip", sp, "")
	if prev.Format != "spotify" || parseSummary(t, prev.Preview).Counts.New < 2 {
		t.Fatalf("spotify zip: %s %+v", prev.Format, parseSummary(t, prev.Preview).Counts)
	}

	// Apple's privacy export holds many CSVs; only the two play history files are
	// read, and an import starts only because both are in the zip.
	ap := mkzip(map[string][]byte{
		"Apple Music Activity/Apple Music Play Activity.csv":               fixture(t, "applemusic_play_activity.csv"),
		"Apple Music Activity/Apple Music - Play History Daily Tracks.csv": fixture(t, "applemusic_daily_tracks.csv"),
		"Apple Music Activity/Apple Music Library Tracks.csv":              []byte("Title,Artist\nJunk,Junk\n"),
	})
	e3 := newEnv(t, Options{})
	prev, _ = e3.previewAndApply("apple_media_services.zip", ap, "")
	if prev.Format != "applemusic" || parseSummary(t, prev.Preview).Counts.New != 5 {
		t.Fatalf("apple zip: %s %+v", prev.Format, parseSummary(t, prev.Preview).Counts)
	}
}

func TestUploadValidation(t *testing.T) {
	small := newEnv(t, Options{MaxBytes: 1000})
	if code, _ := small.upload("x.csv", bytes.Repeat([]byte("a"), 5000), ""); code != 413 {
		t.Fatalf("too large: %d", code)
	}
	e := newEnv(t, Options{})
	if code, _ := e.upload("x.txt", []byte("hello world"), ""); code != 422 {
		t.Fatalf("unknown format: %d", code)
	}
	if code, _ := e.upload("x.json", []byte("[]"), "?format=nope"); code != 400 {
		t.Fatalf("bad format: %d", code)
	}
	if code, _ := e.upload("x.json", nil, ""); code != 400 {
		t.Fatalf("empty: %d", code)
	}
	if code, _ := e.upload("x.json", []byte("[]"), "?format=listenbrainz&dedupe_window=-3"); code != 400 {
		t.Fatalf("bad window: %d", code)
	}
	// An explicit format that does not match fails with a readable error.
	_, j := e.upload("lb.jsonl", fixture(t, "listenbrainz.jsonl"), "?format=maloja")
	if j.FormatDetected || j.Format != "maloja" {
		t.Fatalf("explicit format: %+v", j)
	}
	if f := e.wait2(j.ID, StatusFailed); f.Error == "" {
		t.Fatal("expected an error message")
	}
	// Nothing left behind in the upload directory except the failed job's file.
	ents, _ := os.ReadDir(e.m.opt.Dir)
	for _, en := range ents {
		if strings.HasSuffix(en.Name(), ".tmp") {
			t.Fatalf("temp file leaked: %s", en.Name())
		}
	}
}

func (e *env) wait2(id int64, want string) Job { return e.wait(id, want) }

func TestCancelAndDelete(t *testing.T) {
	e := newEnv(t, Options{})
	_, j := e.upload("a.jsonl", fixture(t, "listenbrainz.jsonl"), "")
	e.wait(j.ID, StatusAwaiting)
	if code, _ := e.post(fmt.Sprintf("/admin/imports/%d/cancel", j.ID)); code != 200 {
		t.Fatal("cancel")
	}
	got, _ := e.m.store.Get(e.ctx, j.ID)
	if got.Status != StatusCancelled {
		t.Fatalf("status %s", got.Status)
	}
	if got.filePath != "" {
		t.Fatalf("upload not removed: %s", got.filePath)
	}
	if code, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm", j.ID)); code != 409 {
		t.Fatalf("confirm after cancel: %d", code)
	}
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/admin/imports/%d", j.ID), nil)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if code, _ := e.post(fmt.Sprintf("/admin/imports/%d/cancel", 999)); code != 404 {
		t.Fatal("unknown id")
	}
	if code, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm", j.ID)); code != 404 {
		t.Fatal("deleted id")
	}
}

// A cancelled context stops the apply pass and rolls back the open batch.
func TestApplyCancelRollsBackBatch(t *testing.T) {
	e := newEnv(t, Options{})
	_, j := e.upload("a.jsonl", fixture(t, "listenbrainz.jsonl"), "")
	j = e.wait(j.ID, StatusAwaiting)
	j.Phase = PhaseApply
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if _, _, err := newPass(e.m, j, true).run(ctx); err == nil {
		t.Fatal("expected cancellation error")
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 0 {
		t.Fatal("rows leaked from a cancelled batch")
	}
}

// A job found running at startup is failed, and Retry resumes it: rows the
// interrupted apply already wrote are recognised as the job's own.
func TestRestartRecoveryAndResume(t *testing.T) {
	e := newEnv(t, Options{})
	_, j := e.upload("a.jsonl", fixture(t, "listenbrainz.jsonl"), "")
	prev := e.wait(j.ID, StatusAwaiting)

	// Simulate an apply that wrote one row and then crashed.
	_, err := e.pools.Write.Exec(`INSERT INTO scrobble (played_at, raw_artist, raw_title, import_job_id) VALUES ('2023-11-14T22:13:20Z','Test Artist','Test Song',?)`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.pools.Write.Exec(`UPDATE import_job SET status='running', phase='apply' WHERE id=?`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = prev

	m2 := NewManager(e.pools.Read, e.pools.Write, Options{Dir: e.m.opt.Dir})
	if err := m2.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.m = m2
	failed, _ := m2.store.Get(e.ctx, j.ID)
	if failed.Status != StatusFailed || !strings.Contains(failed.Error, "restart") {
		t.Fatalf("after restart: %s %q", failed.Status, failed.Error)
	}
	if err := m2.Retry(e.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	done := e.wait(j.ID, StatusDone)
	if done.Progress.Inserted != 1 || done.Progress.Duplicate != 1 {
		t.Fatalf("resume: %+v", done.Progress)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 2 {
		t.Fatal("expected exactly the 2 listens after resume")
	}
	if err := m2.Retry(e.ctx, j.ID); err == nil {
		t.Fatal("retry of a done job must fail")
	}
}

func TestApplyRefusesChangedFile(t *testing.T) {
	e := newEnv(t, Options{})
	_, j := e.upload("a.jsonl", fixture(t, "listenbrainz.jsonl"), "")
	j = e.wait(j.ID, StatusAwaiting)
	if err := os.WriteFile(j.filePath, []byte(`{"listened_at":1,"track_metadata":{"artist_name":"x","track_name":"y"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Confirm(e.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	f := e.wait(j.ID, StatusFailed)
	if !strings.Contains(f.Error, "changed") || e.count(`SELECT COUNT(*) FROM scrobble`) != 0 {
		t.Fatalf("%q", f.Error)
	}
}

func TestProgressVisibleDuringImport(t *testing.T) {
	// Counters are persisted, not just held in memory.
	e := newEnv(t, Options{})
	_, done := e.previewAndApply("a.jsonl", fixture(t, "listenbrainz.jsonl"), "")
	got, _ := e.m.store.Get(e.ctx, done.ID)
	if got.Progress.Parsed != 2 || got.Progress.New != 2 || got.Progress.Inserted != 2 || got.Progress.Errors != 4 || got.FinishedAt == "" {
		t.Fatalf("%+v", got.Progress)
	}
	s := parseSummary(t, got.Result)
	if s.Drift == nil || s.Drift.PreviewNew != 2 || s.Drift.AppliedNew != 2 || len(s.ErrorsSample) == 0 {
		t.Fatalf("result: %+v", s)
	}
}

func TestDetectSniffing(t *testing.T) {
	cases := map[string]string{
		"listenbrainz.jsonl": "listenbrainz", "lastfm.csv": "lastfm", "lastfm.json": "lastfm",
		"spotify.json": "spotify", "maloja.json": "maloja", "koito.json": "koito",
		"applemusic.csv": "applemusic",
	}
	for f, want := range cases {
		got, err := sniffText(fixture(t, f))
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", f, got, err, want)
		}
	}
	if _, err := sniffText([]byte("   ")); err == nil {
		t.Error("blank file should not detect")
	}
	if g, _ := sniffText([]byte(`{"payload":{"listens":[{"track_metadata":{}}]}}`)); g != "listenbrainz" {
		t.Errorf("envelope: %s", g)
	}
}

// A large file is streamed: memory stays far below the file size, and a
// re-import of the same big file adds nothing.
func TestLargeFileStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("large file")
	}
	const rows = 120_000
	e := newEnv(t, Options{})
	path := filepath.Join(t.TempDir(), "big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 200) // fatten rows: the file is much larger than the heap budget
	for i := 0; i < rows; i++ {
		fmt.Fprintf(f, `{"listened_at":%d,"track_metadata":{"artist_name":"Artist %d","track_name":"Track %d","release_name":"Album","additional_info":{"note":"%s"}}}`+"\n",
			1600000000+int64(i)*240, i%500, i, pad)
	}
	f.Close()
	st, _ := os.Stat(path)
	data, _ := os.ReadFile(path) // test-side only; the server streams from its own copy
	t.Logf("file size %d MiB", st.Size()>>20)

	code, j := e.upload("big.jsonl", data, "")
	if code != 201 {
		t.Fatalf("upload %d", code)
	}
	data = nil // the request body of the test client is garbage now
	runtime.GC()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	stop := make(chan struct{})
	peak := make(chan uint64)
	go func() {
		var max uint64
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				peak <- max
				return
			case <-time.After(25 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > max {
					max = ms.HeapAlloc
				}
			}
		}
	}()
	e.wait(j.ID, StatusAwaiting)
	if code, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm", j.ID)); code != 200 {
		t.Fatalf("confirm %d", code)
	}
	done := e.wait(j.ID, StatusDone)
	close(stop)
	p := <-peak
	if done.Progress.Inserted != rows {
		t.Fatalf("inserted %d", done.Progress.Inserted)
	}
	growth := int64(p) - int64(base.HeapAlloc)
	t.Logf("peak heap growth %d MiB for a %d MiB file (preview + apply of %d rows)", growth>>20, st.Size()>>20, rows)
	if growth > int64(st.Size())/2 {
		t.Fatalf("heap grew %d MiB for a %d MiB file: not streaming", growth>>20, st.Size()>>20)
	}

	prev, again := e.previewAndApply("big2.jsonl", fixtureBytes(t, path), "")
	if s := parseSummary(t, prev.Preview); s.Counts.New != 0 || s.Counts.Duplicate != rows || again.Progress.Inserted != 0 {
		t.Fatalf("re-import: %+v", s.Counts)
	}
}

func fixtureBytes(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- Apple Music: two files, and nothing starts until both are here

func (e *env) addFile(id int64, name string, data []byte, query string) (int, Job) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", fmt.Sprintf("/admin/imports/%d/files%s", id, query), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	var j Job
	_ = json.Unmarshal(w.Body.Bytes(), &j)
	return w.Code, j
}

func appleFixtures(t *testing.T) (play, daily []byte) {
	return fixture(t, "applemusic_play_activity.csv"), fixture(t, "applemusic_daily_tracks.csv")
}

func (e *env) uploadsOnDisk() int {
	ents, _ := os.ReadDir(e.m.opt.Dir)
	n := 0
	for _, en := range ents {
		if strings.HasSuffix(en.Name(), ".upload") {
			n++
		}
	}
	return n
}

// steady asserts a job stays exactly as it is for a while: the worker polls on a
// 30 s tick and is woken on every upload, so a short wait after a wake is enough
// to see whether it would have started.
func (e *env) steady(id int64, status string) Job {
	e.t.Helper()
	e.m.Wake()
	time.Sleep(300 * time.Millisecond)
	j, err := e.m.store.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	if j.Status != status {
		e.t.Fatalf("job %d is %s, want it to stay %s", id, j.Status, status)
	}
	return j
}

func TestAppleImportWaitsForBothFilesBeforeAnythingStarts(t *testing.T) {
	play, daily := appleFixtures(t)
	e := newEnv(t, Options{})

	code, j := e.upload("Apple Music Play Activity.csv", play, "")
	if code != 201 || j.Status != StatusAwaitingFiles || j.Format != "applemusic" || !j.FormatDetected {
		t.Fatalf("first file: %d %s %s", code, j.Status, j.Format)
	}
	if len(j.Files) != 2 || !j.Files[0].Present || j.Files[1].Present || len(j.Missing) != 1 || j.Missing[0] != "daily_tracks" {
		t.Fatalf("files %+v missing %v", j.Files, j.Missing)
	}
	// Nothing is scanned, joined or previewed, whatever wakes the worker.
	w := e.steady(j.ID, StatusAwaitingFiles)
	if w.Progress.RowsSeen != 0 || len(w.Preview) != 0 || w.StartedAt != "" {
		t.Fatalf("the job started on half the data: %+v", w)
	}
	if _, claimed, err := e.m.store.claimNext(e.ctx); err != nil || claimed {
		t.Fatalf("the worker claimed a job that is missing a file (%v, %v)", claimed, err)
	}
	// It cannot be confirmed, retried or applied either.
	if c, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm", j.ID)); c != 409 {
		t.Fatalf("confirm while waiting: %d", c)
	}
	if c, _ := e.post(fmt.Sprintf("/admin/imports/%d/retry", j.ID)); c != 409 {
		t.Fatalf("retry while waiting: %d", c)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 0 || e.count(`SELECT COUNT(*) FROM recording`) != 0 {
		t.Fatal("rows were created before the second file arrived")
	}

	// The second file releases it.
	code, j2 := e.addFile(j.ID, "Apple Music - Play History Daily Tracks.csv", daily, "")
	if code != 200 || len(j2.Missing) != 0 {
		t.Fatalf("second file: %d %+v", code, j2.Missing)
	}
	prev := e.wait(j.ID, StatusAwaiting)
	sum := parseSummary(t, prev.Preview)
	if sum.Apple == nil || sum.Apple.Joined != 5 || sum.Apple.Ambiguous != 1 || sum.Apple.Unjoined != 1 || sum.Counts.New != 5 {
		t.Fatalf("preview: %+v %+v", sum.Apple, sum.Counts)
	}
	if sum.Matching["by_id"] != 5 || sum.Matching["by_name"] != 0 {
		t.Fatalf("every Apple row resolves by catalog id: %+v", sum.Matching)
	}
	if !strings.Contains(prev.Filename, "Play Activity") || !strings.Contains(prev.Filename, "Daily Tracks") {
		t.Fatalf("job name %q", prev.Filename)
	}
	if c, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm", j.ID)); c != 200 {
		t.Fatalf("confirm: %d", c)
	}
	done := e.wait(j.ID, StatusDone)
	if done.Progress.Inserted != 5 || e.uploadsOnDisk() != 0 {
		t.Fatalf("inserted %d, uploads left %d", done.Progress.Inserted, e.uploadsOnDisk())
	}
	// The catalog ids came from Daily Tracks, as the ids the listens named, and the
	// artists are the strings Daily Tracks gives, as written.
	if e.count(`SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND provider='apple' AND match_method='submitted_id'
		AND provider_id IN ('1001','1002','1005','1006','1008')`) != 5 {
		t.Fatal("the Apple song ids were not attached as submitted ids")
	}
	if e.count(`SELECT COUNT(*) FROM artist WHERE name IN ('Artist A','Artist B','Hi - Fi','Echo Artist','Queen')`) != 5 {
		t.Fatal("artists from Daily Tracks were not created as written")
	}
	if e.count(`SELECT COUNT(*) FROM recording WHERE title IN ('Gamma','Delta')`) != 0 {
		t.Fatal("an ambiguous or unjoined play was guessed")
	}
}

func TestAppleImportWorksInEitherOrder(t *testing.T) {
	play, daily := appleFixtures(t)
	e := newEnv(t, Options{})
	_, j := e.upload("Apple Music - Play History Daily Tracks.csv", daily, "")
	if j.Status != StatusAwaitingFiles || len(j.Missing) != 1 || j.Missing[0] != "play_activity" {
		t.Fatalf("%s %v", j.Status, j.Missing)
	}
	e.steady(j.ID, StatusAwaitingFiles)
	if c, _ := e.addFile(j.ID, "Apple Music Play Activity.csv", play, ""); c != 200 {
		t.Fatalf("add: %d", c)
	}
	e.wait(j.ID, StatusAwaiting)
}

func TestAppleZipWithBothFilesIsReadyAtOnceAndWithOneWaits(t *testing.T) {
	play, daily := appleFixtures(t)
	mk := func(files map[string][]byte) []byte {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		for n, d := range files {
			w, _ := zw.Create(n)
			_, _ = w.Write(d)
		}
		zw.Close()
		return b.Bytes()
	}
	// Both entries: no waiting.
	e := newEnv(t, Options{})
	_, j := e.upload("export.zip", mk(map[string][]byte{"A/Apple Music Play Activity.csv": play, "A/Apple Music - Play History Daily Tracks.csv": daily}), "")
	if j.Status == StatusAwaitingFiles {
		t.Fatalf("a zip with both files should not wait: %+v", j.Missing)
	}
	e.wait(j.ID, StatusAwaiting)

	// One entry: it waits for the other, which can come as a plain CSV.
	e2 := newEnv(t, Options{})
	_, j2 := e2.upload("partial.zip", mk(map[string][]byte{"A/Apple Music Play Activity.csv": play, "A/readme.txt": []byte("x")}), "")
	if j2.Status != StatusAwaitingFiles || len(j2.Missing) != 1 || j2.Missing[0] != "daily_tracks" {
		t.Fatalf("%s %v", j2.Status, j2.Missing)
	}
	e2.steady(j2.ID, StatusAwaitingFiles)
	if c, _ := e2.addFile(j2.ID, "daily.csv", daily, ""); c != 200 {
		t.Fatalf("add: %d", c)
	}
	e2.wait(j2.ID, StatusAwaiting)
	if c, _ := e2.post(fmt.Sprintf("/admin/imports/%d/confirm", j2.ID)); c != 200 {
		t.Fatal("confirm")
	}
	if done := e2.wait(j2.ID, StatusDone); done.Progress.Inserted != 5 {
		t.Fatalf("inserted %d", done.Progress.Inserted)
	}
}

func TestAppleUploadValidation(t *testing.T) {
	play, daily := appleFixtures(t)
	e := newEnv(t, Options{})
	if c, _ := e.upload("notes.csv", []byte("Title,Artist\nx,y\n"), "?format=applemusic"); c != 422 {
		t.Fatalf("a non-Apple CSV as Apple Music: %d", c)
	}
	_, j := e.upload("Apple Music Play Activity.csv", play, "")
	if c, _ := e.addFile(j.ID, "again.csv", play, ""); c != 409 {
		t.Fatalf("the same role twice: %d", c)
	}
	if c, _ := e.addFile(j.ID, "notes.csv", []byte("Title,Artist\nx,y\n"), ""); c != 422 {
		t.Fatalf("a non-Apple file: %d", c)
	}
	before := e.uploadsOnDisk()
	if c, w := e.addFile(j.ID, "newer.csv", play, "?replace=true"); c != 200 || len(w.Missing) != 1 {
		t.Fatalf("replace: %d %v", c, w.Missing)
	}
	if e.uploadsOnDisk() != before {
		t.Fatalf("the replaced file was not deleted (%d files, was %d)", e.uploadsOnDisk(), before)
	}
	if c, _ := e.addFile(999, "x.csv", daily, ""); c != 404 {
		t.Fatalf("unknown job: %d", c)
	}
	e.addFile(j.ID, "daily.csv", daily, "")
	e.wait(j.ID, StatusAwaiting)
	if c, _ := e.addFile(j.ID, "extra.csv", daily, ""); c != 409 {
		t.Fatalf("a job that is not waiting: %d", c)
	}
	// A single-file format has nothing to add to.
	_, lb := e.upload("a.jsonl", fixture(t, "listenbrainz.jsonl"), "")
	if c, _ := e.addFile(lb.ID, "b.jsonl", fixture(t, "listenbrainz.jsonl"), ""); c != 409 {
		t.Fatalf("a single-file job: %d", c)
	}
}

func TestAppleImportCancelledWhileWaitingLeavesNothingBehind(t *testing.T) {
	play, _ := appleFixtures(t)
	e := newEnv(t, Options{})
	_, j := e.upload("Apple Music Play Activity.csv", play, "")
	if e.uploadsOnDisk() != 1 {
		t.Fatalf("setup: %d uploads", e.uploadsOnDisk())
	}
	if c, _ := e.post(fmt.Sprintf("/admin/imports/%d/cancel", j.ID)); c != 200 {
		t.Fatal("cancel")
	}
	got, _ := e.m.store.Get(e.ctx, j.ID)
	if got.Status != StatusCancelled || e.uploadsOnDisk() != 0 || got.filesOnDisk() {
		t.Fatalf("status %s, uploads %d", got.Status, e.uploadsOnDisk())
	}
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/admin/imports/%d", j.ID), nil)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	if w.Code != 204 || e.count(`SELECT COUNT(*) FROM import_job_file`) != 0 {
		t.Fatalf("delete: %d, file rows %d", w.Code, e.count(`SELECT COUNT(*) FROM import_job_file`))
	}
}

// A job waiting for its second file is still waiting after a restart.
func TestAppleWaitingJobSurvivesARestart(t *testing.T) {
	play, daily := appleFixtures(t)
	e := newEnv(t, Options{})
	_, j := e.upload("Apple Music Play Activity.csv", play, "")
	m2 := NewManager(e.pools.Read, e.pools.Write, Options{Dir: e.m.opt.Dir})
	if err := m2.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.m = m2
	r := chi.NewRouter()
	id := func(h http.Handler) http.Handler { return h }
	m2.MountWith(r, id, id)
	e.h = r
	e.steady(j.ID, StatusAwaitingFiles)
	if c, _ := e.addFile(j.ID, "daily.csv", daily, ""); c != 200 {
		t.Fatalf("add after restart: %d", c)
	}
	e.wait(j.ID, StatusAwaiting)
}

// The files that were previewed are the files that are applied.
func TestAppleApplyRefusesAChangedFile(t *testing.T) {
	play, daily := appleFixtures(t)
	e := newEnv(t, Options{})
	_, j := e.upload("Apple Music Play Activity.csv", play, "")
	e.addFile(j.ID, "daily.csv", daily, "")
	j = e.wait(j.ID, StatusAwaiting)
	var path string
	for _, f := range j.fileRefs {
		if f.role == "daily_tracks" {
			path = f.path
		}
	}
	if err := os.WriteFile(path, append(daily, []byte("\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	e.post(fmt.Sprintf("/admin/imports/%d/confirm", j.ID))
	if f := e.wait(j.ID, StatusFailed); !strings.Contains(f.Error, "changed since the preview") {
		t.Fatalf("error %q", f.Error)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 0 {
		t.Fatal("a changed file was applied")
	}
}

// Defence in depth: even if a job somehow got released with a file missing, no
// stage runs on half the data.
func TestAMultiFileJobWithAMissingFileFailsInsteadOfRunning(t *testing.T) {
	play, _ := appleFixtures(t)
	e := newEnv(t, Options{})
	_, j := e.upload("Apple Music Play Activity.csv", play, "")
	if _, err := e.pools.Write.Exec(`UPDATE import_job SET awaiting_files=0 WHERE id=?`, j.ID); err != nil { // simulate a bug or a hand edit
		t.Fatal(err)
	}
	e.m.Wake()
	f := e.wait(j.ID, StatusFailed)
	if !strings.Contains(f.Error, "Daily Tracks") || !strings.Contains(f.Error, "missing") {
		t.Fatalf("error %q", f.Error)
	}
	if e.count(`SELECT COUNT(*) FROM scrobble`) != 0 || f.Progress.RowsSeen != 0 {
		t.Fatal("rows were read or written")
	}
}

func TestAMultiFileFormatCannotBeParsedAsASingleFile(t *testing.T) {
	if _, err := parserFor("applemusic", JobOptions{}); err == nil {
		t.Fatal("the Apple Music format parsed from one file")
	}
	if _, err := ParseFile(context.Background(), "/nonexistent", "applemusic", JobOptions{}, nil, nil); err == nil {
		t.Fatal("ParseFile ran the Apple Music format")
	}
}

func TestRequiredFilesAreListedForClients(t *testing.T) {
	rf := RequiredFiles()
	if len(rf["applemusic"]) != 2 || rf["applemusic"][0]["role"] != "play_activity" || rf["applemusic"][1]["role"] != "daily_tracks" || len(rf["spotify"]) != 0 {
		t.Fatalf("%+v", rf)
	}
}

// Onboarding says yes to imports that may still be waiting for a file or
// being read; each applies as soon as its preview is done, never before.
func TestConfirmWhenReady(t *testing.T) {
	e := newEnv(t, Options{})
	// Ready already: applies straight away.
	code, j := e.upload("a.jsonl", fixture(t, "listenbrainz.jsonl"), "")
	if code != 201 {
		t.Fatalf("upload: %d", code)
	}
	e.wait(j.ID, StatusAwaiting)
	if c, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm?when_ready=1", j.ID)); c != 200 {
		t.Fatalf("confirm ready job: %d", c)
	}
	if e.wait(j.ID, StatusDone).Progress.Inserted != 2 {
		t.Fatal("ready job did not apply")
	}

	// Still waiting for its second file: nothing runs until it arrives, then
	// the preview runs and the apply follows on its own.
	play, daily := appleFixtures(t)
	code, a := e.upload("Apple Music Play Activity.csv", play, "")
	if code != 201 || a.Status != StatusAwaitingFiles {
		t.Fatalf("apple first file: %d %s", code, a.Status)
	}
	if c, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm?when_ready=1", a.ID)); c != 200 {
		t.Fatalf("confirm waiting job: %d", c)
	}
	if w := e.steady(a.ID, StatusAwaitingFiles); w.Progress.RowsSeen != 0 {
		t.Fatal("confirming a waiting job started it")
	}
	// Without when_ready the old rule stands.
	if c, _ := e.post(fmt.Sprintf("/admin/imports/%d/confirm", a.ID)); c != 409 {
		t.Fatalf("plain confirm while waiting: %d", c)
	}
	if c, _ := e.addFile(a.ID, "Apple Music - Play History Daily Tracks.csv", daily, ""); c >= 300 {
		t.Fatalf("second file: %d", c)
	}
	if done := e.wait(a.ID, StatusDone); done.Progress.Inserted == 0 {
		t.Fatal("apple job applied nothing")
	}
}
