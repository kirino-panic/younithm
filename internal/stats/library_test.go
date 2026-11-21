package stats

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func librarySetup(t *testing.T) http.Handler {
	_, d := setup(t)
	seed(t, d)
	exec(t, d, `INSERT INTO artist(id,name) VALUES (2,'Guest')`)
	exec(t, d, `INSERT INTO recording(id,title) VALUES (2,'Feature')`)
	exec(t, d, `INSERT INTO recording_artist(recording_id,artist_id,position,join_phrase) VALUES (2,2,0,' feat. '),(2,1,1,NULL)`)
	exec(t, d, `INSERT INTO recording_release(recording_id,release_id,disc,position,is_primary) VALUES (1,1,1,1,1)`)
	exec(t, d, `INSERT INTO release_artist(release_id,artist_id) VALUES (1,1)`)
	exec(t, d, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
	  ('artist',1,'musicbrainz','mb-a','isrc','auto'),('artist',1,'spotify','sp-cand','name','candidate')`)
	exec(t, d, `INSERT INTO alias(kind,raw_string,entity_id) VALUES ('artist','matched artist',1),('artist','m. artist',1)`)
	exec(t, d, `INSERT INTO scrobble(played_at,recording_id,raw_artist,raw_title) VALUES ('2026-06-15T09:00:00.000Z',2,'x','y')`)
	h := New(d)
	h.now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	r := chi.NewRouter()
	h.MountLibrary(r, nil, nil)
	return r
}

func TestArtistPage(t *testing.T) {
	h := librarySetup(t)
	var a struct {
		Name      string            `json:"name"`
		Plays     int64             `json:"plays"`
		Rank      int64             `json:"rank"`
		Aliases   []string          `json:"aliases"`
		Links     map[string]string `json:"links"`
		TopTracks []artistTrack     `json:"top_tracks"`
		Albums    []artistRelease   `json:"albums"`
		AppearsOn []artistTrack     `json:"appears_on"`
		Chart     chart             `json:"chart"`
	}
	if c := get(t, h, "/artists/1", &a); c != 200 {
		t.Fatalf("status %d", c)
	}
	if a.Name != "Matched Artist" || a.Plays != 3 || a.Rank != 1 {
		t.Fatalf("artist %+v", a)
	}
	if len(a.Links) != 1 || a.Links["musicbrainz"] != "mb-a" {
		t.Fatalf("candidates must not show as links: %v", a.Links)
	}
	if len(a.Aliases) != 1 || a.Aliases[0] != "m. artist" || len(a.Albums) != 1 || a.Albums[0].Plays != 2 {
		t.Fatalf("aliases %v albums %+v", a.Aliases, a.Albums)
	}
	if len(a.TopTracks) != 2 || a.TopTracks[0].Title != "Song One" || a.TopTracks[0].Plays != 2 {
		t.Fatalf("top tracks %+v", a.TopTracks)
	}
	if len(a.AppearsOn) != 1 || a.AppearsOn[0].Title != "Feature" {
		t.Fatalf("appears on %+v", a.AppearsOn)
	}
	if len(a.Chart.Month) != 30 || len(a.Chart.Year) != 12 || len(a.Chart.All) != 1 || a.Chart.Month[29].Plays != 1 {
		t.Fatalf("chart %+v", a.Chart)
	}
}

func TestReleaseAndRecordingPages(t *testing.T) {
	h := librarySetup(t)
	var rel struct {
		Title    string         `json:"title"`
		Plays    int64          `json:"plays"`
		PlaysAny int64          `json:"plays_any"`
		Tracks   []releaseTrack `json:"tracks"`
		Artists  []credit       `json:"artists"`
	}
	if c := get(t, h, "/releases/1", &rel); c != 200 {
		t.Fatalf("status %d", c)
	}
	if rel.Title != "Album One" || rel.Plays != 2 || rel.PlaysAny != 2 || len(rel.Tracks) != 1 || rel.Tracks[0].Plays != 2 || len(rel.Artists) != 1 {
		t.Fatalf("release %+v", rel)
	}
	var rec struct {
		Artists  []credit           `json:"artists"`
		Releases []recordingRelease `json:"releases"`
		Plays    int64              `json:"plays"`
	}
	get(t, h, "/recordings/2", &rec)
	if len(rec.Artists) != 2 || rec.Artists[0].Name != "Guest" || rec.Artists[0].JoinPhrase != " feat. " || rec.Plays != 1 {
		t.Fatalf("recording %+v", rec)
	}
	var hist struct{ Items []historyItem }
	get(t, h, "/recordings/1/history", &hist)
	if len(hist.Items) != 2 || hist.Items[0].Album == nil || *hist.Items[0].Album != "Album One" {
		t.Fatalf("history %+v", hist.Items)
	}
	for path, want := range map[string]int{"/artists/99": 404, "/releases/x": 400, "/recordings/0": 400} {
		if c := get(t, h, path, nil); c != want {
			t.Errorf("%s: %d want %d", path, c, want)
		}
	}
}

func TestImagesInResponses(t *testing.T) {
	_, d := setup(t)
	seed(t, d)
	exec(t, d, `INSERT INTO recording_release(recording_id,release_id,disc,position,is_primary) VALUES (1,1,1,1,1)`)
	exec(t, d, `INSERT INTO release_artist(release_id,artist_id) VALUES (1,1)`)
	h := New(d)
	r := chi.NewRouter()
	h.Mount(r, Gates{})
	h.MountLibrary(r, nil, nil)

	var before struct {
		Image string `json:"image"`
	}
	get(t, r, "/artists/1", &before)
	if before.Image != "" {
		t.Fatalf("no image yet, got %q", before.Image)
	}
	exec(t, d, `UPDATE release SET artwork_path='release-1-aabbccdd.jpg' WHERE id=1`)
	exec(t, d, `UPDATE artist SET image_path='artist-1-11223344.png' WHERE id=1`)

	var a struct {
		Image     string          `json:"image"`
		Albums    []artistRelease `json:"albums"`
		TopTracks []artistTrack   `json:"top_tracks"`
	}
	get(t, r, "/artists/1", &a)
	if a.Image != "/api/v1/art/artist/1?v=11223344" || a.Albums[0].Image != "/api/v1/art/release/1?v=aabbccdd" || a.TopTracks[0].Image != a.Albums[0].Image {
		t.Fatalf("artist page: %+v", a)
	}
	var rel struct {
		Image string `json:"image"`
	}
	get(t, r, "/releases/1", &rel)
	if rel.Image != a.Albums[0].Image {
		t.Fatalf("release page: %q", rel.Image)
	}
	for kind, want := range map[string]string{"albums": a.Albums[0].Image, "tracks": a.Albums[0].Image, "artists": a.Image} {
		var top struct {
			Items []topItem `json:"items"`
		}
		get(t, r, "/top/"+kind+"?range=all", &top)
		var got string
		for _, it := range top.Items {
			if it.ID != nil && *it.ID == 1 {
				got = it.Image
			}
		}
		if got != want {
			t.Fatalf("top %s: want %q, items %+v", kind, want, top.Items)
		}
	}
	var rc struct {
		Items []recentItem `json:"items"`
	}
	get(t, r, "/recent", &rc)
	if len(rc.Items) == 0 || rc.Items[0].Image != a.Albums[0].Image {
		t.Fatalf("recent: %+v", rc.Items)
	}
}
