package enrich

import (
	"context"
	"errors"
	"testing"
	"time"

	"younithm/internal/identity"
	"younithm/internal/providers"
	"younithm/internal/providers/match"
)

func TestFillCreditedNamesPairsByMusicBrainzID(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	exec(t, e.db, `INSERT INTO artist(id, name) VALUES (101, 'Casiopea'), (102, 'T-SQUARE'), (103, 'The Square')`)
	exec(t, e.db, `INSERT INTO external_link(entity_type, entity_id, provider, provider_id, match_method) VALUES
	  ('artist', 101, 'musicbrainz', 'mb-c', 'manual'), ('artist', 102, 'musicbrainz', 'mb-s', 'manual')`)
	exec(t, e.db, `INSERT INTO release(id, title) VALUES (101, 'Casiopea vs. The Square')`)
	exec(t, e.db, `INSERT INTO release_artist(release_id, artist_id, position, join_phrase) VALUES (101, 101, 0, ' vs. '), (101, 102, 1, NULL)`)
	credits := []providers.Artist{
		{ProviderID: "mb-c", Name: "Casiopea", JoinPhrase: " vs. "},
		{ProviderID: "mb-s", Name: "T-SQUARE", CreditedName: "The Square"},
		{ProviderID: "mb-other", Name: "Nobody", CreditedName: "Someone"}, // not ours: nothing happens
	}
	run := func(provider string) {
		tx, err := e.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := fillCreditedNames(ctx, tx, identity.KindRelease, 101, provider, credits); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	got := func(artist int) string {
		var s string
		if err := e.db.QueryRow(`SELECT COALESCE(credited_name, '') FROM release_artist WHERE release_id=101 AND artist_id=?`, artist).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	run(providers.Spotify) // only MusicBrainz says how an artist is credited
	if got(102) != "" {
		t.Fatal("non-MusicBrainz provider filled a credited name")
	}
	run(providers.MusicBrainz)
	if got(101) != "" || got(102) != "The Square" {
		t.Fatalf("credited names: %q %q", got(101), got(102))
	}
	// Never overwrites one already set (by a person or an earlier lookup).
	exec(t, e.db, `UPDATE release_artist SET credited_name='Square' WHERE release_id=101 AND artist_id=102`)
	run(providers.MusicBrainz)
	if got(102) != "Square" {
		t.Fatal("overwrote a credited name")
	}
	// A credit spelled like our own artist name is not stored (MusicBrainz
	// just names the artist differently from us).
	exec(t, e.db, `UPDATE release_artist SET credited_name=NULL WHERE release_id=101`)
	credits[0].CreditedName = "Casiopea" // MusicBrainz might call the artist "CASIOPEA"
	run(providers.MusicBrainz)
	if got(101) != "" {
		t.Fatalf("stored our own name as credited: %q", got(101))
	}
	// The same artist credited twice, once under another name: which of our
	// credits the name belongs to is unknown, so nothing is stored.
	credits = []providers.Artist{
		{ProviderID: "mb-c", Name: "Casiopea", JoinPhrase: " / "},
		{ProviderID: "mb-s", Name: "T-SQUARE", JoinPhrase: " / "},
		{ProviderID: "mb-s", Name: "T-SQUARE", CreditedName: "The Square"},
	}
	exec(t, e.db, `UPDATE release_artist SET credited_name=NULL WHERE release_id=101`)
	run(providers.MusicBrainz)
	if got(102) != "" {
		t.Fatalf("guessed which credit a repeated artist's name belongs to: %q", got(102))
	}
	if n := 0; e.db.QueryRow(`SELECT COUNT(*) FROM release_artist WHERE artist_id=103`).Scan(&n) == nil && n != 0 {
		t.Fatal("paired by name")
	}
}

// relTracksFake is a MusicBrainz provider that only answers release tracklists.
type relTracksFake struct {
	tracks map[string][]providers.Track
	err    error
	calls  int
}

func (f *relTracksFake) Name() string { return providers.MusicBrainz }
func (f *relTracksFake) SearchTrack(context.Context, providers.TrackQuery) ([]providers.Track, error) {
	return nil, nil
}
func (f *relTracksFake) GetTrack(context.Context, string) (*providers.Track, error) {
	return nil, providers.ErrNotFound
}
func (f *relTracksFake) GetArtwork(context.Context, string) ([]providers.ImageRef, error) {
	return nil, nil
}
func (f *relTracksFake) GetArtistImage(context.Context, string) ([]providers.ImageRef, error) {
	return nil, nil
}
func (f *relTracksFake) LookupReleaseTracks(_ context.Context, id string) ([]providers.Track, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if ts, ok := f.tracks[id]; ok {
		return ts, nil
	}
	return nil, providers.ErrNotFound
}

func TestCreditedNamesBackfill(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	fake := &relTracksFake{}
	e.m = NewManager(e.db, match.New(providers.NewRegistry(fake), match.Config{}), nil,
		Options{Now: func() time.Time { return e.now }})
	exec(t, e.db, `INSERT INTO artist(id, name) VALUES (101, 'Casiopea'), (102, 'T-SQUARE')`)
	exec(t, e.db, `INSERT INTO external_link(entity_type, entity_id, provider, provider_id, match_method) VALUES
	  ('artist', 101, 'musicbrainz', 'mb-c', 'manual'), ('artist', 102, 'musicbrainz', 'mb-s', 'manual')`)
	exec(t, e.db, `INSERT INTO release(id, title) VALUES (101, 'Casiopea vs. The Square'), (102, 'Gone From MB')`)
	exec(t, e.db, `INSERT INTO external_link(entity_type, entity_id, provider, provider_id, match_method) VALUES
	  ('release', 101, 'musicbrainz', 'rel-1', 'manual'), ('release', 102, 'musicbrainz', 'rel-gone', 'manual')`)
	exec(t, e.db, `INSERT INTO release_artist(release_id, artist_id, position, join_phrase) VALUES (101, 101, 0, ' vs. '), (101, 102, 1, NULL)`)
	exec(t, e.db, `INSERT INTO recording(id, title) VALUES (101, 'Asayake')`)
	exec(t, e.db, `INSERT INTO recording_artist(recording_id, artist_id, position) VALUES (101, 101, 0), (101, 102, 1)`)
	exec(t, e.db, `INSERT INTO external_link(entity_type, entity_id, provider, provider_id, match_method) VALUES ('recording', 101, 'musicbrainz', 'rec-1', 'manual')`)
	credits := []providers.Artist{{ProviderID: "mb-c", Name: "Casiopea", JoinPhrase: " vs. "}, {ProviderID: "mb-s", Name: "T-SQUARE", CreditedName: "The Square"}}
	fake.tracks = map[string][]providers.Track{"rel-1": {{
		Provider: providers.MusicBrainz, ProviderID: "rec-1", Title: "Asayake", Artists: credits,
		Releases: []providers.ReleaseRef{{ProviderID: "rel-1", Title: "Casiopea vs. The Square", Artists: credits}},
	}}}
	credited := func(table, col string, id int64) string {
		var s string
		if err := e.db.QueryRow(`SELECT COALESCE(credited_name, '') FROM `+table+` WHERE `+col+`=? AND artist_id=102`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	cursor := func() string {
		var s string
		_ = e.db.QueryRow(`SELECT value FROM admin_setting WHERE key=?`, settingCreditedBackfill).Scan(&s)
		return s
	}

	// MusicBrainz down: nothing changes, the cursor stays, and it waits before retrying.
	fake.err = errors.New("unreachable")
	if _, err := e.m.creditedNamesPass(ctx); err == nil {
		t.Fatal("error swallowed")
	}
	if cursor() != "" || credited("release_artist", "release_id", 101) != "" {
		t.Fatal("progress recorded on failure")
	}
	fake.err = nil
	if more, err := e.m.creditedNamesPass(ctx); err != nil || more || fake.calls != 1 {
		t.Fatalf("retried during the pause: more=%v err=%v calls=%d", more, err, fake.calls)
	}
	e.now = e.now.Add(creditedRetry + time.Second)

	// One pass fills the album and its track; a release MusicBrainz lost is skipped.
	if more, err := e.m.creditedNamesPass(ctx); err != nil || !more {
		t.Fatalf("pass: more=%v err=%v", more, err)
	}
	if got := credited("release_artist", "release_id", 101); got != "The Square" {
		t.Fatalf("album credit = %q", got)
	}
	if got := credited("recording_artist", "recording_id", 101); got != "The Square" {
		t.Fatalf("track credit = %q", got)
	}
	if cursor() != "102" {
		t.Fatalf("cursor = %q", cursor())
	}
	// The next pass finds nothing left and stops for good.
	if more, err := e.m.creditedNamesPass(ctx); err != nil || more || cursor() != "done" {
		t.Fatalf("finish: more=%v err=%v cursor=%q", more, err, cursor())
	}
	calls := fake.calls
	if _, err := e.m.creditedNamesPass(ctx); err != nil || fake.calls != calls {
		t.Fatal("ran again after done")
	}
}
