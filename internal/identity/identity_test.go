package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"younithm/internal/db"
)

func setup(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "cfg"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func count(t *testing.T, d *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func lb(t *testing.T, tm map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"listened_at": 1, "track_metadata": tm})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func resolve(t *testing.T, d *sql.DB, in Input) (rec, rel int64, rep Report) {
	t.Helper()
	r := &Resolver{}
	rec, rel, err := r.Resolve(context.Background(), d, in)
	if err != nil {
		t.Fatal(err)
	}
	return rec, rel, r.Rep
}

func sparse(artist, title, album string) Input {
	return Parse(nil, Input{Artist: artist, Title: title, Album: album})
}

func TestMultiArtistPayload(t *testing.T) {
	d := setup(t)
	raw := lb(t, map[string]any{
		"artist_name": "Ninajirachi, Porter Robinson", "track_name": "iPod Touch", "release_name": "I Love My Computer",
		"additional_info": map[string]any{
			"artist_names":       []string{"Ninajirachi", "Porter Robinson"},
			"spotify_artist_ids": []string{"https://open.spotify.com/artist/aaa", "https://open.spotify.com/artist/bbb"},
			"spotify_id":         "https://open.spotify.com/track/ttt",
			"spotify_album_id":   "https://open.spotify.com/album/lll",
			"isrc":               "usabc1234567",
		},
		"mbid_mapping": map[string]any{"artists": []map[string]any{
			{"artist_credit_name": "Ninajirachi", "artist_mbid": "m1", "join_phrase": " & "},
			{"artist_credit_name": "Porter Robinson", "artist_mbid": "m2", "join_phrase": ""},
		}},
	})
	in := Parse(raw, Input{Artist: "Ninajirachi, Porter Robinson", Title: "iPod Touch", Album: "I Love My Computer"})
	if len(in.Credits) != 2 || in.Credits[0].SpotifyID != "aaa" || in.Credits[0].JoinPhrase != " & " {
		t.Fatalf("credits: %+v", in.Credits)
	}
	rec, rel, rep := resolve(t, d, in)
	if rec == 0 || rel == 0 || rep.ArtistsCreated != 2 {
		t.Fatalf("rec=%d rel=%d rep=%+v", rec, rel, rep)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM recording_artist WHERE recording_id=?`, rec); n != 2 {
		t.Fatalf("recording_artist=%d", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM artist WHERE verified=1`); n != 2 {
		t.Fatalf("verified=%d", n)
	}
	var jp string
	if err := d.QueryRow(`SELECT join_phrase FROM recording_artist WHERE recording_id=? AND position=0`, rec).Scan(&jp); err != nil || jp != " & " {
		t.Fatalf("join phrase %q %v", jp, err)
	}
	// Mapping IDs were not corroborated by a client id, so they are candidates, not auto.
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE provider='musicbrainz' AND status='auto'`); n != 0 {
		t.Fatalf("uncorroborated mapping ids became auto: %d", n)
	}

	// Same listen again: nothing new.
	rec2, rel2, rep2 := resolve(t, d, in)
	if rec2 != rec || rel2 != rel || rep2.ArtistsCreated+rep2.RecordingsCreated+rep2.ReleasesCreated != 0 {
		t.Fatalf("second pass created things: %d %d %+v", rec2, rel2, rep2)
	}
}

func TestSparseNamesShareOneUnverifiedArtist(t *testing.T) {
	d := setup(t)
	r1, _, _ := resolve(t, d, sparse("Radiohead", "Creep", "Pablo Honey"))
	r2, _, _ := resolve(t, d, sparse("radiohead", "Creep", "Pablo Honey"))
	if r1 != r2 {
		t.Fatalf("recordings %d %d", r1, r2)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM artist WHERE verified=0`); n != 1 {
		t.Fatalf("unverified=%d", n)
	}
}

// An artist string is taken as it says: one artist with exactly that name, whatever
// separators it contains. Only a catalog lookup (enrichment) ever changes that.
func TestArtistStringIsTakenAsItIs(t *testing.T) {
	d := setup(t)
	for _, name := range []string{"A feat. B", "Coheed and Cambria", "In Love With a Ghost", "Earth, Wind & Fire"} {
		resolve(t, d, sparse(name, "Song", ""))
		if n := count(t, d, `SELECT COUNT(*) FROM artist WHERE name=?`, name); n != 1 {
			t.Fatalf("%q: artists named so = %d", name, n)
		}
	}
	if n := count(t, d, `SELECT COUNT(*) FROM artist`); n != 4 {
		t.Fatalf("artists=%d, want 4 (no splitting)", n)
	}
}

func TestNameOnlyLinksToTheOneIDedArtist(t *testing.T) {
	d := setup(t)
	withID := Input{Artist: "Björk", Title: "Army of Me", Credits: []Credit{{Name: "Björk", MBID: "mb-bjork"}}}
	resolve(t, d, withID)
	resolve(t, d, sparse("Björk", "Hyperballad", ""))
	if n := count(t, d, `SELECT COUNT(*) FROM artist`); n != 1 {
		t.Fatalf("artists=%d", n)
	}
	if n := count(t, d, `SELECT verified FROM artist`); n != 1 {
		t.Fatalf("verified artist was downgraded")
	}
}

func TestSameNameTwiceCreatesNewUnverifiedAndCandidates(t *testing.T) {
	d := setup(t)
	resolve(t, d, Input{Artist: "Nirvana", Title: "a", Credits: []Credit{{Name: "Nirvana", MBID: "m-us"}}})
	resolve(t, d, Input{Artist: "Nirvana", Title: "b", Credits: []Credit{{Name: "Nirvana", MBID: "m-uk"}}})
	if n := count(t, d, `SELECT COUNT(*) FROM artist`); n != 2 {
		t.Fatalf("different MBIDs must be two artists, got %d", n)
	}
	_, _, rep := resolve(t, d, sparse("Nirvana", "c", ""))
	if n := count(t, d, `SELECT COUNT(*) FROM artist`); n != 3 {
		t.Fatalf("ambiguous name must create a new row, got %d", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM merge_candidate WHERE kind='artist'`); n != 2 || rep.Candidates != 2 {
		t.Fatalf("candidates=%d rep=%+v", n, rep)
	}
}

func TestMBIDConflictNeverMerges(t *testing.T) {
	d := setup(t)
	resolve(t, d, Input{Artist: "X", Title: "a", Credits: []Credit{{Name: "X", SpotifyID: "sp1", MBID: "mb1"}}})
	// Same Spotify id, different MBID: two artists.
	resolve(t, d, Input{Artist: "X", Title: "b", Credits: []Credit{{Name: "X", SpotifyID: "sp1", MBID: "mb2"}}})
	if n := count(t, d, `SELECT COUNT(*) FROM artist`); n != 2 {
		t.Fatalf("artists=%d", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND provider='spotify'`); n != 1 {
		t.Fatalf("spotify id was duplicated or stolen")
	}
}

func TestMappingCorroboratedBySubmittedSpotifyIDIsAuto(t *testing.T) {
	d := setup(t)
	raw := lb(t, map[string]any{
		"artist_name": "A", "track_name": "T",
		"additional_info": map[string]any{"artist_names": []string{"A"}, "spotify_id": "https://open.spotify.com/track/ttt"},
		"mbid_mapping": map[string]any{
			"recording_mbid": "rec-mb",
			"artists":        []map[string]any{{"artist_credit_name": "A", "artist_mbid": "art-mb"}},
			"url_rels":       []map[string]any{{"url": "https://open.spotify.com/track/ttt"}},
		},
	})
	rec, _, _ := resolve(t, d, Parse(raw, Input{Artist: "A", Title: "T"}))
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='musicbrainz' AND status='auto'`, rec); n != 1 {
		t.Fatalf("corroborated mapping recording id not auto")
	}
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE entity_type='artist' AND provider='musicbrainz' AND status='auto'`); n != 1 {
		t.Fatalf("corroborated mapping artist id not auto")
	}
}

func TestUncorroboratedMappingIsCandidateOnly(t *testing.T) {
	d := setup(t)
	raw := lb(t, map[string]any{
		"artist_name": "A", "track_name": "T",
		"additional_info": map[string]any{},
		"mbid_mapping": map[string]any{
			"recording_mbid": "rec-mb",
			"artists":        []map[string]any{{"artist_credit_name": "A", "artist_mbid": "art-mb"}},
		},
	})
	rec, _, _ := resolve(t, d, Parse(raw, Input{Artist: "A", Title: "T"}))
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE status='auto'`); n != 0 {
		t.Fatalf("auto links=%d", n)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND status='candidate'`, rec); n != 1 {
		t.Fatalf("recording candidate missing")
	}
	// A candidate does not make the artist verified or a key for later lookups.
	if n := count(t, d, `SELECT COUNT(*) FROM artist WHERE verified=1`); n != 0 {
		t.Fatalf("artist verified by a candidate")
	}
}

func TestSubmittedIDWinsOverMapping(t *testing.T) {
	raw := lb(t, map[string]any{
		"artist_name": "A", "track_name": "T",
		"additional_info": map[string]any{"recording_mbid": "client-mb"},
		"mbid_mapping":    map[string]any{"recording_mbid": "other-mb"},
	})
	in := Parse(raw, Input{Artist: "A", Title: "T"})
	if in.RecordingMBID != "client-mb" || in.CandRecordingMBID != "" {
		t.Fatalf("%+v", in)
	}
}

func TestIDParsing(t *testing.T) {
	if got := SpotifyID("https://open.spotify.com/track/abc123?si=x"); got != "abc123" {
		t.Fatal(got)
	}
	if got := SpotifyID("spotify:artist:zzz"); got != "zzz" {
		t.Fatal(got)
	}
	for in, want := range map[string]string{
		"https://music.apple.com/au/song/12345":           "12345",
		"https://music.apple.com/us/song/some-name/67890": "67890",
		"https://music.apple.com/us/album/foo/111?i=222":  "222",
		"https://music.apple.com/us/album/foo/111":        "111",
		"https://example.com/not-numeric":                 "",
	} {
		if got := AppleID(in); got != want {
			t.Errorf("AppleID(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHintKeepsExistingRecordingAndAttaches(t *testing.T) {
	d := setup(t)
	res, _ := d.Exec(`INSERT INTO artist (name) VALUES ('Old')`)
	aid, _ := res.LastInsertId()
	res, _ = d.Exec(`INSERT INTO recording (title) VALUES ('Song')`)
	rid, _ := res.LastInsertId()
	d.Exec(`INSERT INTO recording_artist (recording_id, artist_id) VALUES (?,?)`, rid, aid)
	in := Input{Artist: "Old", Title: "Song", HintRecordingID: rid, Credits: []Credit{{Name: "Old", SpotifyID: "sp-old"}}, SpotifyTrackID: "tr1"}
	got, _, rep := resolve(t, d, in)
	if got != rid || rep.RecordingsCreated != 0 {
		t.Fatalf("got=%d rep=%+v", got, rep)
	}
	if n := count(t, d, `SELECT COUNT(*) FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider='spotify'`, rid); n != 1 {
		t.Fatal("spotify track id not attached")
	}
	if n := count(t, d, `SELECT verified FROM artist WHERE id=?`, aid); n != 1 {
		t.Fatal("artist not verified by an attached id")
	}
}

// A recording may hold two MusicBrainz ids when one is the track id the listen
// named and the other the recording it resolved to. A listen naming either
// finds it; a listen naming a third, unrelated id is still another recording.
func TestAnEntityHoldingTheIDItselfIsNotInConflictWithIt(t *testing.T) {
	d := setup(t)
	rec, _, _ := resolve(t, d, Input{Artist: "A", Title: "T", RecordingMBID: "mb-track", Credits: []Credit{{Name: "A"}}})
	d.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status) VALUES ('recording', ?, 'musicbrainz', 'mb-rec', 'submitted_mbid', 1.0, 'auto')`, rec)
	for _, id := range []string{"mb-track", "mb-rec"} {
		again, _, _ := resolve(t, d, Input{Artist: "A", Title: "T", RecordingMBID: id, Credits: []Credit{{Name: "A"}}})
		if again != rec {
			t.Fatalf("a listen naming %s made another recording (%d, want %d)", id, again, rec)
		}
	}
	other, _, _ := resolve(t, d, Input{Artist: "A", Title: "T", RecordingMBID: "mb-other", Credits: []Credit{{Name: "A"}}})
	if other == rec {
		t.Fatal("an unrelated MusicBrainz id was merged into the recording")
	}
}
