package identity

import (
	"context"
	"testing"
)

func lookup(t *testing.T, q DB, in Input) (int64, int64) {
	t.Helper()
	rec, rel, err := Lookup(context.Background(), q, in)
	if err != nil {
		t.Fatal(err)
	}
	return rec, rel
}

func TestLookupFindsByIDAndNameWithoutWriting(t *testing.T) {
	d := setup(t)
	raw := lb(t, map[string]any{"artist_name": "Fishmans", "track_name": "Long Season", "release_name": "Long Season",
		"additional_info": map[string]any{"spotify_id": "https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC", "spotify_album_id": "https://open.spotify.com/album/1ATL5GLyefJaxhQzSPVrLX"}})
	rec, rel, _ := resolve(t, d, Parse(raw, Input{Artist: "Fishmans", Title: "Long Season", Album: "Long Season"}))
	before := count(t, d, `SELECT (SELECT COUNT(*) FROM recording)+(SELECT COUNT(*) FROM release)+(SELECT COUNT(*) FROM artist)+(SELECT COUNT(*) FROM external_link)`)

	// By Spotify id, whatever the spelling.
	byID := lb(t, map[string]any{"artist_name": "フィッシュマンズ", "track_name": "LONG SEASON",
		"additional_info": map[string]any{"spotify_id": "spotify:track:4uLU6hMCjMI75M1A2tKUQC"}})
	if r, l := lookup(t, d, Parse(byID, Input{Artist: "フィッシュマンズ", Title: "LONG SEASON"})); r != rec || l != rel {
		t.Fatalf("by id: %d %d, want %d %d", r, l, rec, rel)
	}
	// By name with no ids; the album is the track's own.
	if r, l := lookup(t, d, sparse("fishmans", "long season", "")); r != rec || l != rel {
		t.Fatalf("by name: %d %d, want %d %d", r, l, rec, rel)
	}
	// An unknown track finds nothing and creates nothing.
	if r, l := lookup(t, d, sparse("Fishmans", "Night Cruising", "Night Cruising")); r != 0 || l != 0 {
		t.Fatalf("unknown: %d %d", r, l)
	}
	after := count(t, d, `SELECT (SELECT COUNT(*) FROM recording)+(SELECT COUNT(*) FROM release)+(SELECT COUNT(*) FROM artist)+(SELECT COUNT(*) FROM external_link)`)
	if before != after {
		t.Fatalf("lookup wrote rows: %d -> %d", before, after)
	}
}

func TestLookupGivesUpOnAmbiguousNamesAndConflicts(t *testing.T) {
	d := setup(t)
	a, _, _ := resolve(t, d, Parse(lb(t, map[string]any{"artist_name": "X", "track_name": "Song",
		"additional_info": map[string]any{"recording_mbid": "11111111-1111-1111-1111-111111111111"}}), Input{Artist: "X", Title: "Song"}))
	b, _, _ := resolve(t, d, Parse(lb(t, map[string]any{"artist_name": "X", "track_name": "Song",
		"additional_info": map[string]any{"recording_mbid": "22222222-2222-2222-2222-222222222222"}}), Input{Artist: "X", Title: "Song"}))
	if a == b {
		t.Fatal("setup: want two recordings")
	}
	if r, _ := lookup(t, d, sparse("X", "Song", "")); r != 0 {
		t.Fatalf("two tracks share the name, got %d", r)
	}
	// A third MusicBrainz id is a third track, not either of these.
	other := Parse(lb(t, map[string]any{"artist_name": "X", "track_name": "Song",
		"additional_info": map[string]any{"recording_mbid": "33333333-3333-3333-3333-333333333333"}}), Input{Artist: "X", Title: "Song"})
	if r, _ := lookup(t, d, other); r != 0 {
		t.Fatalf("conflicting id, got %d", r)
	}
}
