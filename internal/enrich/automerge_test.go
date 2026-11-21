package enrich

import (
	"context"
	"testing"
	"time"

	"younithm/internal/providers"
	"younithm/internal/providers/match"
	"younithm/internal/providers/snapshot"
)

// Recording 1 came in by Spotify id, recording 2 by Apple id; matching
// recording 2 found the Spotify id (or the ISRC) recording 1 holds and
// flagged the pair. Spotify says it runs 3:55.5.
func TestRecordingsSameByIDAndISRCMerge(t *testing.T) {
	type merged struct{ from, into int64 }
	cases := []struct {
		name      string
		reason    string
		appleISRC string
		appleHow  string
		appleMs   string
		extra     string
		want      bool
	}{
		{"shared ISRC", "same spotify id", "JP92Q2601427", "submitted_id", "236200", "", true},
		{"flagged for the ISRC", "same ISRC", "JP92Q2601427", "submitted_id", "234100", "", true},
		{"ISRC case differs", "same spotify id", "jp92q2601427", "submitted_id", "235000", "", true},
		{"lengths 5s apart (an edit)", "same ISRC", "JP92Q2601427", "submitted_id", "230000", "", false},
		{"no length on one side", "same ISRC", "JP92Q2601427", "submitted_id", "0", "", false},
		{"different ISRC", "same spotify id", "USQE92600262", "submitted_id", "235000", "", false},
		{"fuzzy track link", "same spotify id", "JP92Q2601427", "fuzzy", "235000", "", false},
		{"other flag", "similar title", "JP92Q2601427", "submitted_id", "235000", "", false},
		{"different MusicBrainz recordings", "same spotify id", "JP92Q2601427", "submitted_id", "235000",
			`INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
			  ('recording',1,'musicbrainz','mb-a','isrc','auto'), ('recording',2,'musicbrainz','mb-b','isrc','auto')`, false},
		{"same MusicBrainz recording", "same spotify id", "JP92Q2601427", "submitted_id", "235000",
			`INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
			  ('recording',1,'musicbrainz','mb-a','fuzzy','confirmed'), ('recording',2,'musicbrainz','mb-a','fuzzy','auto')`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			var got []merged
			e.m = NewManager(e.db, match.New(providers.NewRegistry(), match.Config{}), snapshot.New(e.db), Options{
				Now: func() time.Time { return e.now }, MaxAttempts: 3,
				Merge: func(_ context.Context, kind string, from, into int64) error {
					if kind != "recording" {
						t.Fatalf("merged a %s", kind)
					}
					got = append(got, merged{from, into})
					return nil
				},
			})
			exec(t, e.db, `INSERT INTO recording(id,title) VALUES (2,'Nothing Ever (feat. Kasane Teto)')`)
			exec(t, e.db, `INSERT INTO external_link(entity_type,entity_id,provider,provider_id,match_method,status) VALUES
			  ('recording',1,'spotify','sp-1','submitted_id','auto'), ('recording',2,'apple','ap-2',?,'auto')`, c.appleHow)
			put := func(provider, id, isrc, ms string) {
				raw := `{"Provider":"` + provider + `","ProviderID":"` + id + `","DurationMs":` + ms + `,"ISRCs":["` + isrc + `"]}`
				if err := snapshot.New(e.db).Put(context.Background(), provider, id, snapshot.KindRecording, []byte(raw), 0); err != nil {
					t.Fatal(err)
				}
			}
			put(providers.Spotify, "sp-1", "JP92Q2601427", "235500")
			put(providers.AppleMusic, "ap-2", c.appleISRC, c.appleMs)
			if c.extra != "" {
				exec(t, e.db, c.extra)
			}
			exec(t, e.db, `INSERT INTO merge_candidate(kind,entity_id,other_id,reason) VALUES ('recording',1,2,?)`, c.reason)

			e.m.autoMerge(context.Background())

			if c.want != (len(got) == 1) {
				t.Fatalf("merges %v, want merged = %v", got, c.want)
			}
			var left int
			_ = e.db.QueryRow(`SELECT COUNT(*) FROM merge_candidate`).Scan(&left)
			if c.want && (got[0] != merged{2, 1} || left != 0) {
				t.Fatalf("merged %v (want 2 into 1), %d flags left", got, left)
			}
			if !c.want && left != 1 {
				t.Fatalf("flag removed without a merge")
			}
		})
	}
}
