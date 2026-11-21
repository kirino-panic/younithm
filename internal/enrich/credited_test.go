package enrich

import (
	"context"
	"testing"

	"younithm/internal/identity"
	"younithm/internal/providers"
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
	if n := 0; e.db.QueryRow(`SELECT COUNT(*) FROM release_artist WHERE artist_id=103`).Scan(&n) == nil && n != 0 {
		t.Fatal("paired by name")
	}
}
