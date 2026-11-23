package identity

import (
	"context"
	"testing"

	"younithm/internal/edit"
)

func TestCompilationDoesNotSplitByTrackArtist(t *testing.T) {
	d := setup(t)
	// No album artist, no album id: three tracks by three artists on one album.
	_, r1, _ := resolve(t, d, sparse("A", "One", "Now 100"))
	_, r2, _ := resolve(t, d, sparse("B", "Two", "Now 100"))
	_, r3, _ := resolve(t, d, sparse("C", "Three", "now 100"))
	if r1 == 0 || r1 != r2 || r1 != r3 {
		t.Fatalf("releases %d %d %d, want one", r1, r2, r3)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM release`); n != 1 {
		t.Fatalf("releases=%d", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM release_artist`); n != 0 {
		t.Fatalf("album artists were copied from track artists: %d", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM recording_release WHERE release_id=?`, r1); n != 3 {
		t.Fatalf("recordings on the compilation=%d", n)
	}
}

func TestAlbumArtistFilledInLaterWithoutSplitting(t *testing.T) {
	d := setup(t)
	_, r1, _ := resolve(t, d, sparse("A", "One", "Now 100"))
	in := sparse("B", "Two", "Now 100")
	in.AlbumArtists = []Credit{{Name: "Various Artists"}}
	_, r2, _ := resolve(t, d, in)
	if r1 != r2 {
		t.Fatalf("album artist arrival split the album: %d %d", r1, r2)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM release_artist WHERE release_id=?`, r1); n != 1 {
		t.Fatalf("album artists=%d", n)
	}
	// Track artists differ from the album artist, and must stay different.
	if n := count(t, d, `SELECT COUNT(*) FROM artist WHERE name='Various Artists'`); n != 1 {
		t.Fatal("Various Artists missing")
	}
	// A later listen with the same album artist lands on the same album.
	in2 := sparse("C", "Three", "Now 100")
	in2.AlbumArtists = []Credit{{Name: "various artists"}}
	if _, r3, _ := resolve(t, d, in2); r3 != r1 {
		t.Fatalf("r3=%d want %d", r3, r1)
	}
}

func TestSameTitleDifferentAlbumArtistsStaySeparate(t *testing.T) {
	d := setup(t)
	a := sparse("A", "x", "Greatest Hits")
	a.AlbumArtists = []Credit{{Name: "A"}}
	b := sparse("B", "y", "Greatest Hits")
	b.AlbumArtists = []Credit{{Name: "B"}}
	_, ra, _ := resolve(t, d, a)
	_, rb, _ := resolve(t, d, b)
	if ra == rb {
		t.Fatal("different album artists were merged")
	}
	// A listen with no album artist cannot choose: new album, flagged against both.
	_, rc, _ := resolve(t, d, sparse("C", "z", "Greatest Hits"))
	if rc == ra || rc == rb {
		t.Fatalf("guessed an album: %d", rc)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM merge_candidate WHERE kind='release'`); n != 2 {
		t.Fatalf("release candidates=%d", n)
	}
	// ...and the next such listen reuses the unattributed one instead of creating more.
	if _, rd, _ := resolve(t, d, sparse("D", "w", "Greatest Hits")); rd != rc {
		t.Fatalf("rd=%d want %d", rd, rc)
	}
}

func TestAppleAlbumIDIsParsedAttachedAndMatched(t *testing.T) {
	d := setup(t)
	raw := lb(t, map[string]any{
		"artist_name": "A", "track_name": "Song", "release_name": "Album",
		"additional_info": map[string]any{"origin_url": "https://music.apple.com/us/album/album/1111?i=2222"},
	})
	in := Parse(raw, Input{Artist: "A", Title: "Song", Album: "Album"})
	if in.AppleAlbumID != "1111" || in.AppleSongID != "2222" {
		t.Fatalf("ids: album=%q song=%q", in.AppleAlbumID, in.AppleSongID)
	}
	rec, rel, _ := resolve(t, d, in)
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE entity_type='release' AND entity_id=? AND provider='apple' AND provider_id='1111'`, rel); n != 1 {
		t.Fatal("apple album id not attached")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='apple' AND provider_id='2222'`, rec); n != 1 {
		t.Fatal("apple song id not attached")
	}
	// Same album id, no title in the second listen: found by id.
	in2 := Input{Artist: "A", Title: "Other", AppleAlbumID: "1111", Credits: in.Credits}
	if _, rel2, _ := resolve(t, d, in2); rel2 != rel {
		t.Fatalf("rel2=%d want %d", rel2, rel)
	}
	// Weak key: same id but a different title does not match by id.
	in3 := Input{Artist: "A", Title: "Third", Album: "Unrelated", AppleAlbumID: "1111", Credits: in.Credits}
	if _, rel3, _ := resolve(t, d, in3); rel3 == rel {
		t.Fatal("apple album id matched despite a different title")
	}
}

func TestAppleURLParsing(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://music.apple.com/us/album/foo/111?i=222": {"222", "111"},
		"https://music.apple.com/us/album/foo/111":       {"", "111"},
		"https://music.apple.com/au/song/12345":          {"12345", ""},
		"https://music.apple.com/us/song/name/67890":     {"67890", ""},
		"https://example.com/x":                          {"", ""},
	} {
		if got := AppleSongFromURL(in); got != want[0] {
			t.Errorf("song(%q)=%q want %q", in, got, want[0])
		}
		if got := AppleAlbumFromURL(in); got != want[1] {
			t.Errorf("album(%q)=%q want %q", in, got, want[1])
		}
	}
}

func TestAliasKeysMatchEditKeys(t *testing.T) {
	if ArtistAliasKey(" Foo  Bar ") != edit.ArtistKey(" Foo  Bar ") ||
		RecordingAliasKey("A  b", " T ") != edit.RecordingKey("A  b", " T ") ||
		ReleaseAliasKey(" X ") != edit.ReleaseKey(" X ") {
		t.Fatal("identity alias keys differ from edit keys")
	}
}

func TestRecordingAliasFindsTranslatedTitle(t *testing.T) {
	d := setup(t)
	rec, _, _ := resolve(t, d, sparse("Utada Hikaru", "First Love", ""))
	ok, err := AddAlias(ctxBg(), d, "recording", rec, RecordingAliasKey("宇多田ヒカル", "First Love (Remaster)"), "First Love (Remaster)")
	if err != nil || !ok {
		t.Fatalf("alias: %v %v", ok, err)
	}
	got, _, _ := resolve(t, d, sparse("宇多田ヒカル", "First Love (Remaster)", ""))
	if got != rec {
		t.Fatalf("alias not used: got %d want %d", got, rec)
	}
	// An alias never repoints an existing one, and never hijacks another entity's own name.
	other, _, _ := resolve(t, d, sparse("Someone", "Else", ""))
	if ok, _ := AddAlias(ctxBg(), d, "recording", other, RecordingAliasKey("宇多田ヒカル", "First Love (Remaster)"), "First Love (Remaster)"); ok {
		t.Fatal("alias was repointed")
	}
	if ok, _ := AddAlias(ctxBg(), d, "recording", other, RecordingAliasKey("Utada Hikaru", "First Love"), "First Love"); ok {
		t.Fatal("alias hijacked another recording's own name")
	}
}

func ctxBg() context.Context { return context.Background() }

func TestEveryTitledListenGetsRecordingAndArtist(t *testing.T) {
	d := setup(t)
	// No artist at all: the recording still has one.
	rec, _, _ := resolve(t, d, sparse("", "Untitled idea", ""))
	if rec == 0 {
		t.Fatal("no recording for a listen with no artist")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM recording_artist ra JOIN artist a ON a.id=ra.artist_id WHERE ra.recording_id=? AND a.name=?`, rec, UnknownArtist); n != 1 {
		t.Fatalf("unknown-artist credit missing: %d", n)
	}
	// An empty title is the one exception.
	if rec, _, _ := resolve(t, d, sparse("A", "   ", "")); rec != 0 {
		t.Fatal("a listen with no title got a recording")
	}
	in, err := CheckIntegrity(ctxBg(), d)
	if err != nil {
		t.Fatal(err)
	}
	if in.RecordingsWithoutArtist != 0 || in.ScrobblesWithoutRecording != 0 || in.ScrobblesOffTheirRecording != 0 {
		t.Fatalf("integrity: %+v", in)
	}
}

func TestIntegrityFlagsBrokenRows(t *testing.T) {
	d := setup(t)
	d.Exec(`INSERT INTO scrobble (played_at, raw_artist, raw_title) VALUES ('2026-01-01T00:00:00Z','A','Orphan')`)
	d.Exec(`INSERT INTO recording (title) VALUES ('No artist')`)
	in, err := CheckIntegrity(ctxBg(), d)
	if err != nil {
		t.Fatal(err)
	}
	if in.ScrobblesWithoutRecording != 1 || in.RecordingsWithoutArtist != 1 {
		t.Fatalf("integrity: %+v", in)
	}
}
