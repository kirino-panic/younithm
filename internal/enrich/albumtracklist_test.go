package enrich

import (
	"context"
	"testing"
	"time"

	"younithm/internal/edit"
	"younithm/internal/identity"
	"younithm/internal/providers"
	"younithm/internal/providers/snapshot"
)

// blueAlbum is a MusicBrainz catalogue holding one album, "rel-blue", whose
// tracklist has the right recording of "Song", and another edition's copy of it.
func blueAlbum() *relFake { return blueAlbumOn(providers.MusicBrainz) }

// blueAlbumOn is blueAlbum in another catalogue (same ids).
func blueAlbumOn(provider string) *relFake {
	on := func(id, title, rel string) providers.Track {
		tr := track(id, title, "Artist")
		tr.Provider = provider
		tr.Releases = []providers.ReleaseRef{{ProviderID: rel, Title: "Blue"}}
		return tr
	}
	right := on("mb-right", "Song", "rel-blue")
	wrong := on("mb-wrong", "Song", "rel-deluxe")
	return &relFake{
		catalogFake: catalogFake{name: provider,
			byID: map[string]providers.Track{"mb-right": right, "mb-wrong": wrong}},
		tracklists: map[string][]providers.Track{"rel-blue": {on("mb-intro", "Intro", "rel-blue"), right}},
	}
}

// linkAlbum links a release to rel-blue the way the editor does by hand.
func (e *idEnv) linkAlbum(t *testing.T, rel int64) { e.linkAlbumOn(t, rel, providers.MusicBrainz) }

func (e *idEnv) linkAlbumOn(t *testing.T, rel int64, provider string) {
	t.Helper()
	if _, err := e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status, pinned)
VALUES ('release', ?, ?, 'rel-blue', 'manual', 1, 'confirmed', 1)`, rel, provider); err != nil {
		t.Fatal(err)
	}
}

func (e *idEnv) mbLinks(t *testing.T, rec int64) map[string]string {
	return e.linksOn(t, rec, providers.MusicBrainz)
}

func (e *idEnv) linksOn(t *testing.T, rec int64, provider string) map[string]string {
	t.Helper()
	rows, err := e.db.Query(`SELECT provider_id, status FROM external_link WHERE entity_type='recording' AND entity_id=? AND provider=?`, rec, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, st string
		rows.Scan(&id, &st)
		out[id] = st
	}
	return out
}

// An album a person linked to MusicBrainz places its tracks from its
// tracklist, as an album the listen named does, even with no consent for
// name-only matches and a title the catalogue spells differently.
func TestAHandLinkedAlbumPlacesItsTracks(t *testing.T) {
	mb := blueAlbum()
	e := newIDEnv(t, mb)
	rec, rel := e.listen(t, identity.Input{Artist: "Artist", Title: "Song (feat. Guest)", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
	e.db.Exec(`UPDATE recording SET fuzzy_consent=0 WHERE id=?`, rec)
	e.linkAlbum(t, rel)
	e.run(t)
	if got := e.mbLinks(t, rec); len(got) != 1 || got["mb-right"] != "auto" {
		t.Fatalf("links = %v, want mb-right auto", got)
	}
}

// A recording matching put on another edition's copy of the song moves to the
// album's own recording once a person links the album.
func TestAHandLinkedAlbumReplacesAnotherEditionsRecording(t *testing.T) {
	mb := blueAlbum()
	e := newIDEnv(t, mb)
	rec, rel := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
	e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status)
VALUES ('recording', ?, 'musicbrainz', 'mb-wrong', 'isrc', 1, 'auto')`, rec)
	e.linkAlbum(t, rel)
	e.run(t)
	if got := e.mbLinks(t, rec); len(got) != 1 || got["mb-right"] != "auto" {
		t.Fatalf("links = %v, want only mb-right", got)
	}
}

// Links a person set or the listen named are never replaced, and a tracklist
// that cannot be read proves nothing.
func TestAHandLinkedAlbumKeepsWhatItCannotDisprove(t *testing.T) {
	for _, c := range []struct {
		name, method, status string
		pinned               int
		dropTracklist        bool
	}{
		{"confirmed", "manual", "confirmed", 1, false},
		{"named by the listen", "submitted_mbid", "auto", 0, false},
		{"tracklist unreadable", "isrc", "auto", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			mb := blueAlbum()
			if c.dropTracklist {
				mb.tracklists = nil
			}
			e := newIDEnv(t, mb)
			rec, rel := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
			e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status, pinned)
VALUES ('recording', ?, 'musicbrainz', 'mb-wrong', ?, 1, ?, ?)`, rec, c.method, c.status, c.pinned)
			e.linkAlbum(t, rel)
			e.run(t)
			if got := e.mbLinks(t, rec); len(got) != 1 || got["mb-wrong"] == "" {
				t.Fatalf("links = %v, want mb-wrong kept alone", got)
			}
		})
	}
}

// Linking an album queues every track on it for matching again.
func TestRequeueReleaseQueuesItsTracks(t *testing.T) {
	e := newIDEnv(t, blueAlbum())
	a, rel := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
	b, _ := e.listen(t, identity.Input{Artist: "Artist", Title: "Intro", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
	e.run(t)
	e.db.Exec(`UPDATE enrichment_job SET status='done'`)
	if err := e.m.RequeueRelease(t.Context(), rel); err != nil {
		t.Fatal(err)
	}
	if n := e.n(t, `SELECT COUNT(*) FROM enrichment_job WHERE status='queued' AND recording_id IN (?, ?)`, a, b); n != 2 {
		t.Fatalf("queued = %d, want 2", n)
	}
}

// Spotify and Apple Music albums work the same way, each by its own album id:
// a hand-linked album places its tracks and replaces another edition's copy.
func TestAHandLinkedCatalogueAlbumPlacesAndReplacesTracks(t *testing.T) {
	for _, prov := range []string{providers.Spotify, providers.AppleMusic} {
		t.Run(prov, func(t *testing.T) {
			e := newIDEnv(t, blueAlbumOn(prov))
			placed, rel := e.listen(t, identity.Input{Artist: "Artist", Title: "Song (feat. Guest)", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
			e.db.Exec(`UPDATE recording SET fuzzy_consent=0 WHERE id=?`, placed)
			e.linkAlbumOn(t, rel, prov)
			e.run(t)
			if got := e.linksOn(t, placed, prov); len(got) != 1 || got["mb-right"] != "auto" {
				t.Fatalf("placed: links = %v", got)
			}

			e2 := newIDEnv(t, blueAlbumOn(prov))
			moved, rel2 := e2.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
			e2.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status)
VALUES ('recording', ?, ?, 'mb-wrong', 'isrc', 1, 'auto')`, moved, prov)
			e2.linkAlbumOn(t, rel2, prov)
			e2.run(t)
			if got := e2.linksOn(t, moved, prov); len(got) != 1 || got["mb-right"] != "auto" {
				t.Fatalf("replaced: links = %v", got)
			}
		})
	}
}

// albumDupes sets up two recordings of "Song" on the album Blue, linked by hand
// to rel-blue: the first already holds the album's recording, the second does
// not yet. Merges go through the real editor.
func albumDupes(t *testing.T) (e *idEnv, kept, dupe int64) {
	t.Helper()
	e = newIDEnv(t, blueAlbum())
	ed := &edit.Editor{DB: e.db}
	e.m = NewManager(e.db, e.m.matcher, snapshot.New(e.db), Options{Now: func() time.Time { return e.now }, MaxAttempts: 3,
		Merge: func(ctx context.Context, kind string, from, into int64) error {
			_, err := ed.Merge(ctx, "matching", kind, from, into)
			return err
		}})
	kept, rel := e.listen(t, identity.Input{Artist: "Artist", Title: "Song", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
	dupe, rel2 := e.listen(t, identity.Input{Artist: "Artist", Title: "Song (feat. Guest)", Album: "Blue", Credits: []identity.Credit{{Name: "Artist"}}})
	if rel != rel2 || kept == dupe {
		t.Fatalf("setup: recordings %d %d on releases %d %d", kept, dupe, rel, rel2)
	}
	e.db.Exec(`INSERT INTO external_link (entity_type, entity_id, provider, provider_id, match_method, confidence, status)
VALUES ('recording', ?, 'musicbrainz', 'mb-right', 'isrc', 1, 'auto')`, kept)
	e.linkAlbum(t, rel)
	return e, kept, dupe
}

func (e *idEnv) exists(t *testing.T, rec int64) bool {
	return e.n(t, `SELECT COUNT(*) FROM recording WHERE id=?`, rec) == 1
}

// Two recordings the album's tracklist names as one track are merged, the
// older one kept, with no ISRC needed.
func TestOneTrackOnAHandLinkedAlbumIsMerged(t *testing.T) {
	e, kept, dupe := albumDupes(t)
	e.run(t)
	e.m.autoMerge(context.Background())
	if e.exists(t, dupe) || !e.exists(t, kept) {
		t.Fatalf("not merged: kept %v, dupe %v", e.exists(t, kept), e.exists(t, dupe))
	}
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate`) != 0 {
		t.Fatal("flag left behind")
	}
}

// A person's undo sticks: the pair is flagged again for a person, not merged.
func TestAnUndoneAlbumMergeIsNotMadeAgain(t *testing.T) {
	e, kept, dupe := albumDupes(t)
	e.run(t)
	e.m.autoMerge(context.Background())
	var editID int64
	if err := e.db.QueryRow(`SELECT id FROM edit_log WHERE scope='merge'`).Scan(&editID); err != nil {
		t.Fatal(err)
	}
	if _, err := (&edit.Editor{DB: e.db}).Undo(context.Background(), "admin", editID); err != nil {
		t.Fatal(err)
	}
	if !e.exists(t, dupe) {
		t.Fatal("undo did not bring the recording back")
	}
	if err := e.m.Requeue(context.Background(), dupe); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	e.m.autoMerge(context.Background())
	if !e.exists(t, dupe) || !e.exists(t, kept) {
		t.Fatal("merged again after a person undid it")
	}
	if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE reason=?`, ReasonSameAlbumTrack) != 1 {
		t.Fatal("pair not left flagged for a person")
	}
}

// No merge without the proof: lengths that disagree, or an owner that is not
// on the album (an ordinary "same id" flag, which still needs an ISRC).
func TestAlbumDuplicatesNeedTheProof(t *testing.T) {
	t.Run("lengths disagree", func(t *testing.T) {
		e, kept, dupe := albumDupes(t)
		e.db.Exec(`UPDATE recording SET duration_ms = CASE id WHEN ? THEN 200000 ELSE 260000 END WHERE id IN (?, ?)`, kept, kept, dupe)
		e.run(t)
		e.m.autoMerge(context.Background())
		if !e.exists(t, dupe) {
			t.Fatal("merged an edit 60s apart")
		}
	})
	t.Run("owner not on the album", func(t *testing.T) {
		e, kept, dupe := albumDupes(t)
		var other int64
		e.db.QueryRow(`INSERT INTO release (title) VALUES ('Other') RETURNING id`).Scan(&other)
		e.db.Exec(`UPDATE recording_release SET release_id=? WHERE recording_id=?`, other, kept)
		e.run(t)
		e.m.autoMerge(context.Background())
		if !e.exists(t, dupe) {
			t.Fatal("merged a recording that is not on the album")
		}
		if e.n(t, `SELECT COUNT(*) FROM merge_candidate WHERE reason='same musicbrainz id'`) != 1 {
			t.Fatal("not flagged as an ordinary same-id pair")
		}
	})
}
