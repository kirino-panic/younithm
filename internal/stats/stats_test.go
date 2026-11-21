package stats

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/db"
)

func setup(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	h := New(d)
	h.now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	r := chi.NewRouter()
	h.Mount(r, Gates{})
	return r, d
}

func exec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, h http.Handler, path string, out any) int {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if out != nil && w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code
}

func seed(t *testing.T, d *sql.DB) {
	exec(t, d, `INSERT INTO artist(id,name) VALUES (1,'Matched Artist')`)
	exec(t, d, `INSERT INTO recording(id,title,duration_ms) VALUES (1,'Song One',1000)`)
	exec(t, d, `INSERT INTO recording_artist(recording_id,artist_id) VALUES (1,1)`)
	exec(t, d, `INSERT INTO release(id,title) VALUES (1,'Album One')`)
	ins := `INSERT INTO scrobble(played_at,recording_id,release_id,raw_artist,raw_title,raw_album,duration_played_ms) VALUES (?,?,?,?,?,?,?)`
	exec(t, d, ins, "2026-06-14T10:00:00.000Z", 1, 1, "raw", "raw", "", 500)
	exec(t, d, ins, "2026-06-13T10:00:00.000Z", 1, 1, "raw", "raw", "", nil)
	exec(t, d, ins, "2026-06-12T10:00:00.000Z", nil, nil, "Loose", "Unmatched", "Loose Album", 200)
	exec(t, d, ins, "2025-01-01T10:00:00.000Z", nil, nil, "Loose", "Unmatched", nil, nil)
}

func TestTopArtistsGroupsMatchedAndRaw(t *testing.T) {
	h, d := setup(t)
	seed(t, d)
	var out struct{ Items []topItem }
	if c := get(t, h, "/top/artists?range=all", &out); c != 200 {
		t.Fatalf("status %d", c)
	}
	if len(out.Items) != 2 || out.Items[0].Name != "Loose" || out.Items[0].Plays != 2 ||
		out.Items[1].Name != "Matched Artist" || out.Items[1].Plays != 2 {
		t.Fatalf("got %+v", out.Items)
	}
	var yr struct{ Items []topItem }
	get(t, h, "/top/artists?range=year", &yr)
	if len(yr.Items) != 2 || yr.Items[1].Name != "Loose" || yr.Items[1].Plays != 1 {
		t.Fatalf("year range should drop 2025 play: %+v", yr.Items)
	}
}

func TestTopTracksAndAlbums(t *testing.T) {
	h, d := setup(t)
	seed(t, d)
	var out struct{ Items []topItem }
	get(t, h, "/top/tracks", &out)
	if len(out.Items) != 2 || out.Items[0].Name != "Song One" || out.Items[0].Plays != 2 ||
		out.Items[0].Artist == nil || *out.Items[0].Artist != "Matched Artist" {
		t.Fatalf("tracks %+v", out.Items)
	}
	get(t, h, "/top/albums", &out)
	if len(out.Items) != 2 || out.Items[0].Name != "Album One" {
		t.Fatalf("albums %+v", out.Items)
	}
}

func TestSummaryAndRecent(t *testing.T) {
	h, d := setup(t)
	seed(t, d)
	var s struct {
		Plays           int64 `json:"plays"`
		DistinctTracks  int64 `json:"distinct_tracks"`
		DistinctArtists int64 `json:"distinct_artists"`
		ListenedMS      int64 `json:"listened_ms"`
		PerDay          []day `json:"per_day"`
	}
	get(t, h, "/stats?range=week", &s)
	if s.Plays != 3 || s.DistinctTracks != 2 || s.DistinctArtists != 2 || s.ListenedMS != 1700 || len(s.PerDay) != 3 {
		t.Fatalf("summary %+v", s)
	}
	var rec struct{ Items []recentItem }
	get(t, h, "/recent?limit=2", &rec)
	if len(rec.Items) != 2 || rec.Items[0].Artist != "Matched Artist" || rec.Items[0].Title != "Song One" {
		t.Fatalf("recent %+v", rec.Items)
	}
}

func TestValidation(t *testing.T) {
	h, _ := setup(t)
	for path, want := range map[string]int{
		"/top/bogus": 404, "/top/artists?range=decade": 400, "/top/artists?limit=0": 400,
		"/stats?range=x": 400, "/recent?limit=101": 400,
	} {
		if c := get(t, h, path, nil); c != want {
			t.Errorf("%s: %d want %d", path, c, want)
		}
	}
}

func TestTopLatestWeek(t *testing.T) {
	h, d := setup(t) // "now" is 2026-06-15
	ins := `INSERT INTO scrobble(played_at,raw_artist,raw_title) VALUES (?,?,?)`
	exec(t, d, ins, "2026-01-10T10:00:00.000Z", "A", "Old Hit")
	exec(t, d, ins, "2026-01-08T10:00:00.000Z", "A", "Old Hit")
	exec(t, d, ins, "2026-01-09T10:00:00.000Z", "B", "Other")
	exec(t, d, ins, "2025-12-01T10:00:00.000Z", "C", "Ancient")
	exec(t, d, ins, "2025-12-02T10:00:00.000Z", "C", "Ancient")
	exec(t, d, ins, "2025-12-03T10:00:00.000Z", "C", "Ancient")
	var out struct{ Items []topItem }
	if get(t, h, "/top/tracks?range=week", &out); len(out.Items) != 0 {
		t.Fatalf("week should be empty: %+v", out.Items)
	}
	if get(t, h, "/top/tracks?range=latest_week", &out); len(out.Items) != 2 || out.Items[0].Name != "Old Hit" || out.Items[0].Plays != 2 {
		t.Fatalf("latest_week: %+v", out.Items)
	}
}
