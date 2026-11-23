package stats

import (
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/names"
)

func TestLibraryLists(t *testing.T) {
	h := librarySetup(t)
	type out struct {
		Total int64        `json:"total"`
		Items []browseItem `json:"items"`
	}
	var a out
	if c := get(t, h, "/library/artists", &a); c != 200 {
		t.Fatalf("status %d", c)
	}
	// Matched Artist: 2 plays of Song One + 1 of Feature; Guest: 1 of Feature.
	if a.Total != 2 || a.Items[0].Name != "Matched Artist" || a.Items[0].Plays != 3 || a.Items[1].Plays != 1 {
		t.Fatalf("artists %+v", a)
	}
	var al out
	get(t, h, "/library/albums", &al)
	if al.Total != 1 || al.Items[0].Name != "Album One" || al.Items[0].Plays != 2 || *al.Items[0].Artist != "Matched Artist" {
		t.Fatalf("albums %+v", al)
	}
	var tr out
	get(t, h, "/library/tracks?sort=last", &tr)
	if tr.Total != 2 || tr.Items[0].Name != "Feature" || *tr.Items[0].Artist != "Guest feat. Matched Artist" ||
		tr.Items[1].Album == nil || *tr.Items[1].Album != "Album One" {
		t.Fatalf("tracks %+v", tr)
	}
	// Each credited artist comes separately, in order, so each can link.
	if c := tr.Items[0].Artists; len(c) != 2 || c[0].Name != "Guest" || c[1].Name != "Matched Artist" || c[0].ID == 0 {
		t.Fatalf("track artists %+v", c)
	}
	if c := al.Items[0].Artists; len(c) != 1 || c[0].Name != "Matched Artist" {
		t.Fatalf("album artists %+v", c)
	}
	var n out
	get(t, h, "/library/tracks?sort=name&q=guest", &n)
	if n.Total != 1 || n.Items[0].Name != "Feature" {
		t.Fatalf("search by artist %+v", n)
	}
	get(t, h, "/library/tracks?q=100%25", &n)
	if n.Total != 0 {
		t.Fatalf("%% must be literal %+v", n)
	}
	// Aliases match, in any case or spacing: "m. artist" is a spelling
	// Matched Artist's listens came in with.
	var ma out
	get(t, h, "/library/artists?q=M.%20%20Artist", &ma)
	if ma.Total != 1 || ma.Items[0].Name != "Matched Artist" {
		t.Fatalf("artist by alias %+v", ma)
	}
	get(t, h, "/library/albums?q=m.%20artist", &ma)
	if ma.Total != 1 || ma.Items[0].Name != "Album One" {
		t.Fatalf("album by its artist's alias %+v", ma)
	}
	var p out
	get(t, h, "/library/artists?limit=1&offset=1", &p)
	if p.Total != 2 || len(p.Items) != 1 || p.Items[0].Name != "Guest" {
		t.Fatalf("paging %+v", p)
	}
	for _, bad := range []string{"/library/genres", "/library/tracks?sort=x", "/library/tracks?offset=-1", "/library/tracks?limit=0"} {
		if c := get(t, h, bad, nil); c == 200 {
			t.Fatalf("%s: want error", bad)
		}
	}
}

func TestLibraryUsesLanguageNames(t *testing.T) {
	_, d := setup(t)
	seed(t, d)
	exec(t, d, `INSERT INTO artist(id,name) VALUES (2,'25時、ナイトコードで。')`)
	exec(t, d, `INSERT INTO recording(id,title) VALUES (2,'Song Two')`)
	exec(t, d, `INSERT INTO recording_artist(recording_id,artist_id) VALUES (2,2)`)
	exec(t, d, `INSERT INTO scrobble(played_at,recording_id,raw_artist,raw_title) VALUES ('2026-06-15T09:00:00.000Z',2,'x','y')`)
	exec(t, d, `INSERT INTO entity_name(entity_type,entity_id,lang,provider,provider_id,name,authoritative) VALUES
	  ('artist',2,'en','spotify','sp','25時、ナイトコードで。',0),
	  ('artist',2,'en','musicbrainz','mb','Nightcord at 25:00',1),
	  ('artist',2,'ja','musicbrainz','mb','25時、ナイトコードで。',1)`)
	exec(t, d, `INSERT INTO admin_setting(key,value) VALUES ('languages.preferred','en,ja')`)
	router := func() chi.Router {
		h := New(d).WithNames(names.New(d, d))
		h.now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
		r := chi.NewRouter()
		h.MountLibrary(r, nil, nil)
		return r
	}
	type out struct {
		Total int64        `json:"total"`
		Items []browseItem `json:"items"`
	}

	// The source order wins: Spotify first (the default) shows Spotify's
	// wording, even where MusicBrainz has an English name.
	var sp out
	get(t, router(), "/library/artists?sort=name", &sp)
	if len(sp.Items) != 2 || sp.Items[0].Name != "25時、ナイトコードで。" {
		t.Fatalf("Spotify first: %+v", sp.Items)
	}
	// "Also known as" lists the other names whichever one shows.
	type page struct {
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
	}
	var ap page
	get(t, router(), "/artists/2", &ap)
	if ap.Name != "25時、ナイトコードで。" || len(ap.Aliases) != 1 || ap.Aliases[0] != "Nightcord at 25:00" {
		t.Fatalf("aka with Spotify first: %+v", ap)
	}

	exec(t, d, `INSERT INTO admin_setting(key,value) VALUES ('display.naming','musicbrainz')`)
	r := router()
	var a out
	get(t, r, "/library/artists?sort=name", &a)
	if len(a.Items) != 2 || a.Items[0].Name != "Matched Artist" || a.Items[1].Name != "Nightcord at 25:00" {
		t.Fatalf("sorted by shown name: %+v", a.Items)
	}
	var mp page
	get(t, r, "/artists/2", &mp)
	if mp.Name != "Nightcord at 25:00" || len(mp.Aliases) != 1 || mp.Aliases[0] != "25時、ナイトコードで。" {
		t.Fatalf("aka with MusicBrainz first: %+v", mp)
	}
	var s out
	get(t, r, "/library/artists?q=nightcord", &s)
	if s.Total != 1 || s.Items[0].ID != 2 {
		t.Fatalf("search by language name: %+v", s)
	}
	var tr out
	get(t, r, "/library/tracks?q=nightcord", &tr)
	if tr.Total != 1 || tr.Items[0].Name != "Song Two" {
		t.Fatalf("track search by artist's language name: %+v", tr)
	}
}

func TestLibrarySearchesTrackAliases(t *testing.T) {
	_, d := setup(t)
	seed(t, d)
	exec(t, d, `INSERT INTO alias(kind,raw_string,entity_id) VALUES ('recording','matched artist'||char(31)||'song one (remastered)',1)`)
	h := New(d)
	r := chi.NewRouter()
	h.MountLibrary(r, nil, nil)
	var out struct {
		Total int64        `json:"total"`
		Items []browseItem `json:"items"`
	}
	get(t, r, "/library/tracks?q=Remastered", &out)
	if out.Total != 1 || out.Items[0].ID != 1 {
		t.Fatalf("track by alias %+v", out)
	}
}

func TestLibrarySearchesArtistSearchNames(t *testing.T) {
	_, d := setup(t)
	seed(t, d)
	exec(t, d, `INSERT INTO search_name(kind,entity_id,name,provider,provider_id) VALUES ('artist',1,'ニーゴ','musicbrainz','mb-1')`)
	h := New(d)
	r := chi.NewRouter()
	h.MountLibrary(r, nil, nil)
	var out struct {
		Total int64        `json:"total"`
		Items []browseItem `json:"items"`
	}
	get(t, r, "/library/artists?q=%E3%83%8B%E3%83%BC", &out)
	if out.Total != 1 || out.Items[0].ID != 1 {
		t.Fatalf("artist by search name %+v", out)
	}
}

func TestArtistTrackList(t *testing.T) {
	h, d := librarySetupDB(t)
	// An unplayed track of Matched Artist's still lists, last.
	exec(t, d, `INSERT INTO recording(id,title) VALUES (3,'Unplayed')`)
	exec(t, d, `INSERT INTO recording_artist(recording_id,artist_id,position) VALUES (3,1,0)`)
	type out struct {
		Total int64        `json:"total"`
		Items []browseItem `json:"items"`
	}
	var all out
	if c := get(t, h, "/library/tracks?artist=1", &all); c != 200 {
		t.Fatalf("status %d", c)
	}
	if all.Total != 3 || all.Items[0].Name != "Song One" || all.Items[0].Plays != 2 ||
		all.Items[2].Name != "Unplayed" || all.Items[2].Plays != 0 || all.Items[2].Last != nil {
		t.Fatalf("artist tracks %+v", all)
	}
	var recent out
	get(t, h, "/library/tracks?artist=1&sort=last", &recent)
	if recent.Items[0].Name != "Feature" || recent.Items[2].Name != "Unplayed" {
		t.Fatalf("by last played %+v", recent)
	}
	var guest out
	get(t, h, "/library/tracks?artist=2&q=feat", &guest)
	if guest.Total != 1 || guest.Items[0].Name != "Feature" {
		t.Fatalf("guest %+v", guest)
	}
	// The library list itself still holds played tracks only.
	var lib out
	get(t, h, "/library/tracks", &lib)
	if lib.Total != 2 {
		t.Fatalf("library %+v", lib)
	}
	for _, bad := range []string{"/library/albums?artist=1", "/library/tracks?artist=x", "/library/tracks?artist=0"} {
		if c := get(t, h, bad, nil); c != 400 {
			t.Fatalf("%s: status %d", bad, c)
		}
	}
}

func TestLibrarySearchShowsDisplayNames(t *testing.T) {
	_, d := setup(t)
	seed(t, d)
	exec(t, d, `UPDATE artist SET display_name = 'Mine Artist' WHERE id = 1`)
	exec(t, d, `UPDATE recording SET display_name = 'Mine Song' WHERE id = 1`)
	exec(t, d, `UPDATE release SET display_name = 'Mine Album' WHERE id = 1`)
	exec(t, d, `INSERT INTO recording_release(recording_id,release_id,is_primary) VALUES (1,1,1)`)
	exec(t, d, `INSERT INTO release_artist(release_id,artist_id) VALUES (1,1)`)
	h := New(d).WithNames(names.New(d, d))
	r := chi.NewRouter()
	h.MountLibrary(r, nil, nil)
	type out struct {
		Total int64        `json:"total"`
		Items []browseItem `json:"items"`
	}
	for _, q := range []string{"mine", "matched", "song", "album"} {
		var tr out
		get(t, r, "/library/tracks?q="+q, &tr)
		if tr.Total != 1 || tr.Items[0].Name != "Mine Song" || *tr.Items[0].Artist != "Mine Artist" || *tr.Items[0].Album != "Mine Album" {
			t.Fatalf("q=%s: %+v", q, tr.Items)
		}
	}
	var a, al out
	get(t, r, "/library/artists?q=matched", &a)
	get(t, r, "/library/albums?q=mine", &al)
	if a.Total != 1 || a.Items[0].Name != "Mine Artist" || al.Total != 1 || al.Items[0].Name != "Mine Album" {
		t.Fatalf("artists %+v albums %+v", a.Items, al.Items)
	}
}
