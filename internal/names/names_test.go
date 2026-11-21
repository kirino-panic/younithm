package names

import (
	"context"
	"path/filepath"
	"testing"

	"younithm/internal/db"
)

func TestPreferredNamesFromSnapshots(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO artist(id,name) VALUES (1,'Stored Artist')`,
		`INSERT INTO recording(id,title) VALUES (1,'Stored Title')`,
		`INSERT INTO release(id,title) VALUES (1,'Album')`,
		`INSERT INTO recording_artist(recording_id,artist_id) VALUES (1,1)`,
		`INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
		  ('recording',1,'apple','s1','isrc','auto'),('artist',1,'apple','a1','isrc','auto'),('release',1,'apple','al1','isrc','auto'),
		  ('recording',1,'spotify','sp1','isrc','candidate')`,
		`INSERT INTO provider_snapshot(provider,provider_id,kind,fetched_at,raw_json) VALUES
		  ('apple','s1','recording','x','{"Title":"Apple Title","Artists":[{"ProviderID":"a1","Name":"Apple Artist"}],"Releases":[{"ProviderID":"al1","Title":"Album - Single"}]}'),
		  ('spotify','sp1','recording','x','{"Title":"Candidate Title","Artists":[]}')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s := New(d, d)
	if got := s.Display(ctx, Recording, 1, "Stored Title"); got != "Apple Title" {
		t.Fatalf("default order should fall through Spotify (candidate only) to Apple, got %q", got)
	}
	if err := s.setPref(ctx, mustResolve(t, Scrobbled, nil)); err != nil {
		t.Fatal(err)
	}
	if got := s.Display(ctx, Recording, 1, "Stored Title"); got != "Stored Title" {
		t.Fatalf("as scrobbled should show the stored name, got %q", got)
	}
	if err := s.setPref(ctx, mustResolve(t, "apple", nil)); err != nil {
		t.Fatal(err)
	}
	if got := s.Display(ctx, Recording, 1, "x"); got != "Apple Title" {
		t.Fatalf("recording %q", got)
	}
	if got := s.Display(ctx, Release, 1, "x"); got != "Album - Single" {
		t.Fatalf("release %q", got)
	}
	if got := s.CreditLine(ctx, Recording, 1); got != "Apple Artist" {
		t.Fatalf("credit %q", got)
	}
	s.setPref(ctx, mustResolve(t, ModeCustom, []string{"spotify", Scrobbled, "apple", "musicbrainz"}))
	if got := s.Display(ctx, Recording, 1, "Stored Title"); got != "Stored Title" {
		t.Fatalf("a candidate link must not supply names, got %q", got)
	}
	s.setPref(ctx, mustResolve(t, ModeDefault, nil))
	if _, err := d.Exec(`UPDATE recording SET display_name = 'Mine' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	s.Forget()
	if got := s.Display(ctx, Recording, 1, "Stored Title"); got != "Mine" {
		t.Fatalf("a display name set by hand should win, got %q", got)
	}
	// It wins with the stored name first too, and makes names active.
	s.setPref(ctx, mustResolve(t, ModeCustom, []string{Scrobbled, "spotify", "apple", "musicbrainz"}))
	if !s.Active(ctx) || s.Display(ctx, Recording, 1, "Stored Title") != "Mine" {
		t.Fatal("display name ignored with the stored name first")
	}
	if _, err := d.Exec(`UPDATE recording SET display_name = NULL WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	s.Forget()
	if s.Active(ctx) || s.Display(ctx, Recording, 1, "Stored Title") != "Stored Title" {
		t.Fatal("cleared display name still shows")
	}
	s.setPref(ctx, mustResolve(t, ModeDefault, nil))
	if _, err := d.Exec(`UPDATE recording SET display_name = 'Mine' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	s.Forget()
	vs := s.RecordingVariants(ctx, 1, "Stored Title")
	if len(vs) != 2 || vs[0].Source != "stored" || vs[1].Source != "apple" {
		t.Fatalf("variants %+v", vs)
	}
}

func mustResolve(t *testing.T, mode string, order []string) Pref {
	t.Helper()
	p, ok := resolve(mode, order)
	if !ok {
		t.Fatalf("resolve %q %v", mode, order)
	}
	return p
}

func TestResolve(t *testing.T) {
	if p := parse(""); p.Mode != ModeDefault || p.Order[0] != "spotify" || p.Order[3] != Scrobbled {
		t.Fatalf("default %+v", p)
	}
	if p := parse("musicbrainz"); p.Order[0] != "musicbrainz" || p.Order[1] != "spotify" || len(p.Order) != 4 {
		t.Fatalf("single %+v", p)
	}
	if p := parse("custom:scrobbled,apple,spotify,musicbrainz"); p.Mode != ModeCustom || p.Order[0] != Scrobbled {
		t.Fatalf("custom %+v", p)
	}
	if p := parse("custom:apple,apple,spotify,musicbrainz"); p.Mode != ModeDefault {
		t.Fatalf("a bad custom order should fall back to default, got %+v", p)
	}
	if _, ok := resolve(ModeCustom, []string{"apple"}); ok {
		t.Fatal("a partial custom order should be refused")
	}
}
