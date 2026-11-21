package importers

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func collect(t *testing.T, p Parser, r io.Reader) ([]Listen, []RowError, Result, error) {
	t.Helper()
	var ls []Listen
	var es []RowError
	res, err := p.Parse(context.Background(), r, func(l Listen) error { ls = append(ls, l); return nil }, func(e RowError) { es = append(es, e) })
	return ls, es, res, err
}

func open(t *testing.T, name string) io.Reader {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func TestParsers(t *testing.T) {
	tests := []struct {
		name                          string
		p                             Parser
		file                          string
		rows, parsed, skipped, errors int
		check                         func(t *testing.T, ls []Listen)
	}{
		{"listenbrainz", ListenBrainz{}, "listenbrainz.jsonl", 6, 2, 0, 4, func(t *testing.T, ls []Listen) {
			a, b := ls[0], ls[1]
			eq(t, a.PlayedAt.Unix(), int64(1700000000))
			eq(t, a.Artist, "Test Artist")
			eq(t, a.Album, "Test Album")
			eq(t, a.RecordingMBID, "aaaaaaaa-0000-0000-0000-000000000001")
			eq(t, a.ISRC, "XXTEST0000001")
			eq(t, a.TrackDurationMs, int64(215000))
			eq(t, a.DurationPlayedMs, int64(120000))
			eq(t, a.SpotifyTrackID, "ABCDEF123456")
			eq(t, a.SourceID, "11111111-1111-1111-1111-111111111111")
			eq(t, len(a.ArtistMBIDs), 1)
			// fallbacks to mbid_mapping and duration in seconds
			eq(t, b.RecordingMBID, "aaaaaaaa-0000-0000-0000-000000000002")
			eq(t, b.TrackDurationMs, int64(180000))
			eq(t, b.Source, SourceListenBrainz)
			if len(a.RawPayload) == 0 {
				t.Error("raw payload missing")
			}
		}},
		{"lastfm", LastFM{}, "lastfm.csv", 5, 2, 0, 3, func(t *testing.T, ls []Listen) {
			eq(t, ls[0].Title, "Song, with comma")
			eq(t, ls[0].PlayedAt.Unix(), int64(1700000000))
			eq(t, ls[0].RecordingMBID, "aaaaaaaa-0000-0000-0000-000000000001")
			eq(t, ls[0].ArtistMBIDs[0], "cccccccc-0000-0000-0000-000000000001")
			eq(t, ls[1].Album, "")
		}},
		{"spotify", Spotify{}, "spotify.json", 6, 2, 2, 2, func(t *testing.T, ls []Listen) {
			// ts is the END of the stream; start = end - ms_played
			eq(t, ls[0].PlayedAt, time.Date(2023, 11, 14, 22, 14, 0, 0, time.UTC))
			eq(t, ls[0].DurationPlayedMs, int64(200000))
			eq(t, ls[0].SpotifyTrackID, "ABCDEF123456")
			eq(t, ls[0].SourceID, "spotify:track:ABCDEF123456")
			eq(t, ls[1].Artist, "Legacy Artist")
			eq(t, ls[1].PlayedAt, time.Date(2023, 11, 15, 8, 1, 0, 0, time.UTC))
		}},
		{"maloja", Maloja{}, "maloja.json", 5, 2, 0, 3, func(t *testing.T, ls []Listen) {
			eq(t, ls[0].Artist, "Artist One, Artist Two")
			eq(t, ls[0].AlbumArtist, "Artist One")
			eq(t, ls[0].DurationPlayedMs, int64(200000))
			eq(t, ls[0].TrackDurationMs, int64(230000))
			eq(t, ls[1].DurationPlayedMs, int64(0))
			eq(t, ls[1].Album, "")
		}},
		{"koito", Koito{}, "koito.json", 3, 1, 0, 2, func(t *testing.T, ls []Listen) {
			eq(t, ls[0].Title, "Song K")
			eq(t, ls[0].Artist, "Artist K, Guest")
			eq(t, ls[0].Album, "Album K")
			eq(t, ls[0].TrackDurationMs, int64(215000))
			eq(t, ls[0].RecordingMBID, "aaaaaaaa-0000-0000-0000-000000000001")
			eq(t, len(ls[0].ArtistMBIDs), 1)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ls, es, res, err := collect(t, tc.p, open(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			eq(t, res.Rows, tc.rows)
			eq(t, res.Parsed, tc.parsed)
			eq(t, res.Skipped, tc.skipped)
			eq(t, res.Errors, tc.errors)
			eq(t, len(es), tc.errors)
			eq(t, len(ls), tc.parsed)
			for _, l := range ls {
				eq(t, l.Source, tc.p.Name())
				if l.PlayedAt.Location() != time.UTC {
					t.Errorf("PlayedAt not UTC")
				}
			}
			if res.Earliest.IsZero() || res.Latest.Before(res.Earliest) {
				t.Errorf("bad range %v..%v", res.Earliest, res.Latest)
			}
			tc.check(t, ls)
		})
	}
}

func eq[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSpotifyMinPlayed(t *testing.T) {
	ls, _, res, _ := collect(t, Spotify{MinPlayedMs: -1}, open(t, "spotify.json"))
	eq(t, len(ls), 3) // short play kept when the filter is disabled
	eq(t, res.Skipped, 1)
}

func TestListenBrainzArrayAndEnvelope(t *testing.T) {
	arr := `[{"listened_at":1,"track_metadata":{"artist_name":"A","track_name":"T"}},{"listened_at":2,"track_metadata":{"artist_name":"A","track_name":"U"}}]`
	// listened_at 1 is valid (>0)
	ls, _, _, err := collect(t, ListenBrainz{}, strings.NewReader(arr))
	if err != nil || len(ls) != 2 {
		t.Fatalf("array: %v %d", err, len(ls))
	}
	env := `{"payload":{"count":1,"listens":[{"listened_at":5,"track_metadata":{"artist_name":"A","track_name":"T"}}]}}`
	ls, _, _, err = collect(t, ListenBrainz{}, strings.NewReader(env))
	if err != nil || len(ls) != 1 {
		t.Fatalf("envelope: %v %d", err, len(ls))
	}
}

func TestListenBrainzZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	line := func(ts int) string {
		return fmt.Sprintf(`{"listened_at":%d,"track_metadata":{"artist_name":"A","track_name":"T"}}`+"\n", ts)
	}
	for name, body := range map[string]string{
		"user.json":             `{"user_name":"x"}`,
		"feedback.jsonl":        `{"x":1}` + "\n",
		"listens/2023/12.jsonl": line(300),
		"listens/2023/11.jsonl": line(100) + line(200),
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	zw.Close()
	var got []int64
	res, err := ListenBrainzZip(context.Background(), bytes.NewReader(buf.Bytes()), int64(buf.Len()),
		func(l Listen) error { got = append(got, l.PlayedAt.Unix()); return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Parsed, 3)
	if fmt.Sprint(got) != "[100 200 300]" {
		t.Errorf("order: %v", got)
	}
	if _, err := ListenBrainzZip(context.Background(), bytes.NewReader([]byte("nope")), 4, nil, nil); err == nil {
		t.Error("want error for non-zip")
	}
}

func TestFatalAndEmpty(t *testing.T) {
	for _, p := range Registry() {
		// empty input is not an error
		if _, _, res, err := collect(t, p, strings.NewReader("")); err != nil || res.Rows != 0 {
			t.Errorf("%s empty: %v %v", p.Name(), res, err)
		}
	}
	// syntactically broken JSON is fatal for array-based parsers, with earlier rows kept
	broken := `[{"ts":"2023-11-14T22:17:20Z","ms_played":100000,"master_metadata_track_name":"A","master_metadata_album_artist_name":"B"}, {"ts":`
	ls, _, _, err := collect(t, Spotify{}, strings.NewReader(broken))
	if err == nil || len(ls) != 1 {
		t.Errorf("broken: err=%v n=%d", err, len(ls))
	}
	if _, _, _, err := collect(t, Maloja{}, strings.NewReader(`{"other":[]}`)); err == nil {
		t.Error("maloja without scrobbles should fail")
	}
	if _, _, _, err := collect(t, LastFM{}, strings.NewReader("a,b,c\n1,2,3\n")); err == nil {
		t.Error("non-lastfm csv should fail")
	}
}

func TestEmitErrorAndCancel(t *testing.T) {
	boom := errors.New("boom")
	_, err := LastFM{}.Parse(context.Background(), open(t, "lastfm.csv"), func(Listen) error { return boom }, nil)
	if !errors.Is(err, boom) {
		t.Errorf("got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Maloja{}.Parse(ctx, open(t, "maloja.json"), func(Listen) error { return nil }, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}

// Large input streams without being held in memory: the reader generates data
// on the fly, and we only count.
func TestStreamingLarge(t *testing.T) {
	const n = 200000
	pr, pw := io.Pipe()
	go func() {
		bw := newBufWriter(pw)
		for i := 0; i < n; i++ {
			fmt.Fprintf(bw, `{"listened_at":%d,"track_metadata":{"artist_name":"A","track_name":"T%d"}}`+"\n", 1700000000+i, i)
		}
		bw.Flush()
		pw.Close()
	}()
	count := 0
	res, err := ListenBrainz{}.Parse(context.Background(), pr, func(Listen) error { count++; return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, count, n)
	eq(t, res.Parsed, n)
}

func TestLastFMJSON(t *testing.T) {
	ls, es, res, err := collect(t, LastFM{}, open(t, "lastfm.json"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(ls), 2)
	eq(t, len(es), 1)
	eq(t, res.Skipped, 1)
	eq(t, ls[0].Artist, "JSON Artist")
	eq(t, ls[0].RecordingMBID, "aaaaaaaa-0000-0000-0000-000000000009")
	eq(t, ls[1].Title, "Another")

	env := `{"recenttracks":{"track":[{"artist":{"#text":"A"},"name":"T","album":{"#text":""},"date":{"uts":"1700000000"}}],"@attr":{"page":"1"}}}`
	ls, _, _, err = collect(t, LastFM{}, strings.NewReader(env))
	if err != nil || len(ls) != 1 || ls[0].Artist != "A" {
		t.Fatalf("envelope: %v %v", ls, err)
	}
}

func TestLessNatural(t *testing.T) {
	if !lessNatural("listens/2023/2.jsonl", "listens/2023/10.jsonl") || lessNatural("listens/2024/1.jsonl", "listens/2023/12.jsonl") {
		t.Fatal("natural order wrong")
	}
}

func TestAppleMusic(t *testing.T) {
	ls, es, res, err := collect(t, AppleMusic{}, open(t, "applemusic.csv"))
	if err != nil {
		t.Fatal(err)
	}
	// 8 data rows: Alpha, End Only, Unknown Dur emitted; Too Short, video and
	// blank-title skipped; bad time and missing artist are row errors.
	eq(t, res.Rows, 8)
	eq(t, res.Parsed, 3)
	eq(t, res.Skipped, 3)
	eq(t, res.Errors, 2)
	eq(t, len(es), 2)
	eq(t, ls[0].Title, "Alpha")
	eq(t, ls[0].Artist, "Apple Artist")
	eq(t, ls[0].Album, "Apple Album")
	eq(t, ls[0].PlayedAt.Unix(), int64(1700000000))
	eq(t, ls[0].DurationPlayedMs, int64(210000))
	eq(t, ls[0].TrackDurationMs, int64(215000))
	eq(t, ls[0].Source, SourceAppleMusic)
	// Start derived from end minus play duration; -1 media length is unknown.
	eq(t, ls[1].PlayedAt.UTC().Format(time.RFC3339), "2023-11-14T22:57:30Z")
	eq(t, ls[1].TrackDurationMs, int64(0))
	// Unknown play duration is kept, not filtered.
	eq(t, ls[2].Title, "Unknown Dur")
	eq(t, ls[2].DurationPlayedMs, int64(0))
}

func TestAppleMusicTrackDescription(t *testing.T) {
	in := "Track Description,Play Duration Milliseconds,Event Start Timestamp\n" +
		"Some Band - Some Song,60000,2023-11-14T22:13:20Z\n"
	ls, es, _, err := collect(t, AppleMusic{}, strings.NewReader(in))
	if err != nil || len(es) != 0 || len(ls) != 1 {
		t.Fatalf("err=%v es=%v ls=%v", err, es, ls)
	}
	eq(t, ls[0].Artist, "Some Band")
	eq(t, ls[0].Title, "Some Song")
	if _, _, _, err := collect(t, AppleMusic{}, strings.NewReader("a,b\n1,2\n")); err == nil {
		t.Fatal("want error for non-Apple CSV")
	}
}

// ---- Apple Music: Play Activity joined to Daily Tracks

func joinApple(t *testing.T, play, daily string) ([]Listen, []RowError, Result, AppleJoinStats, error) {
	t.Helper()
	var ls []Listen
	var es []RowError
	res, st, err := AppleJoin(context.Background(), []io.Reader{open(t, play)}, []io.Reader{open(t, daily)}, 0,
		func(l Listen) error { ls = append(ls, l); return nil }, func(e RowError) { es = append(es, e) })
	return ls, es, res, st, err
}

func TestAppleJoinFillsArtistAndCatalogIDFromDailyTracks(t *testing.T) {
	ls, es, res, st, err := joinApple(t, "applemusic_play_activity.csv", "applemusic_daily_tracks.csv")
	if err != nil || len(es) != 0 {
		t.Fatalf("%v %v", err, es)
	}
	got := map[string]Listen{}
	for _, l := range ls {
		got[l.Title] = l
	}
	for title, want := range map[string][2]string{
		"Alpha":      {"Artist A", "1001"},
		"Beta":       {"Artist B", "1002"}, // played 00:30 UTC on the 15th, Daily Tracks says the 14th
		"Song":       {"Hi - Fi", "1005"},  // an artist with " - " in its name: the play's own title decides the split
		"Echo":       {"Echo Artist", "1006"},
		"Don’t Stop": {"Queen", "1008"}, // curly and straight apostrophes are the same
	} {
		l, ok := got[title]
		if !ok || l.Artist != want[0] || l.AppleSongID != want[1] || l.Source != SourceAppleMusic {
			t.Fatalf("%s: %+v", title, l)
		}
	}
	if len(ls) != 5 {
		t.Fatalf("%d listens, want 5", len(ls))
	}
	if got["Alpha"].Album != "Alpha Album" || got["Alpha"].TrackDurationMs != 215000 || got["Alpha"].DurationPlayedMs != 210000 {
		t.Fatalf("Play Activity's own fields must survive: %+v", got["Alpha"])
	}
	// Nothing is guessed: Gamma has two song ids that day, Delta has none.
	if st.Joined != 5 || st.Ambiguous != 1 || st.Unjoined != 1 {
		t.Fatalf("stats %+v", st)
	}
	if st.DailyRows != 10 || st.DailyTracks != 9 {
		t.Fatalf("daily rows %d usable %d (the radio row is not a song)", st.DailyRows, st.DailyTracks)
	}
	if res.Parsed != 5 || res.Rows != 10 || res.Skipped != 5 { // short play, no song name, video, + the two above
		t.Fatalf("%+v", res)
	}
	if len(st.SkippedSample) != 2 || st.SkippedSample[0].Title == "" {
		t.Fatalf("sample %+v", st.SkippedSample)
	}
}

func TestAppleJoinPrefersTheExactDayOverNeighbours(t *testing.T) {
	// Echo has song 1006 that day and 1007 the next day: the exact day is one id, not an ambiguity.
	ls, _, _, st, _ := joinApple(t, "applemusic_play_activity.csv", "applemusic_daily_tracks.csv")
	for _, l := range ls {
		if l.Title == "Echo" && l.AppleSongID != "1006" {
			t.Fatalf("picked %s", l.AppleSongID)
		}
	}
	if st.Ambiguous != 1 {
		t.Fatalf("only Gamma is ambiguous: %+v", st)
	}
}

func TestAppleJoinNeedsBothFiles(t *testing.T) {
	noop := func(Listen) error { return nil }
	if _, _, err := AppleJoin(context.Background(), nil, []io.Reader{open(t, "applemusic_daily_tracks.csv")}, 0, noop, nil); err == nil {
		t.Fatal("ran with no Play Activity")
	}
	if _, _, err := AppleJoin(context.Background(), []io.Reader{open(t, "applemusic_play_activity.csv")}, nil, 0, noop, nil); err == nil {
		t.Fatal("ran with no Daily Tracks")
	}
	if _, _, err := AppleJoin(context.Background(), []io.Reader{open(t, "applemusic_play_activity.csv")},
		[]io.Reader{strings.NewReader("Title,Artist\nx,y\n")}, 0, noop, nil); err == nil {
		t.Fatal("accepted a file without Track Identifier as Daily Tracks")
	}
}

func TestRoleOfTellsTheTwoFilesApart(t *testing.T) {
	for in, want := range map[string]string{
		`"Apple Id Number","Event Start Timestamp","Song Name","Album Name"`:                           RolePlayActivity,
		`Country,Track Identifier,Media type,Date Played,Play Duration Milliseconds,Track Description`: RoleDailyTracks, // also has durations
		`Title,Artist,Album`:                         "",
		`"Event Start Timestamp","Track Identifier"`: RoleDailyTracks,
	} {
		if got := RoleOf(in); got != want {
			t.Fatalf("%q: %q want %q", in, got, want)
		}
	}
}

func TestAppleTitleKeyIgnoresCaseSpacingAndTypographicQuotes(t *testing.T) {
	for _, in := range []string{"Don't Stop", "DON\u2019T   STOP", "  don\u2019t stop ", "Don\u00a0Don\u2019t"[:0] + "Don't\u00a0Stop"} {
		if got := appleTitleKey(in); got != "don't stop" {
			t.Fatalf("%q -> %q", in, got)
		}
	}
	if appleTitleKey("Alpha") == appleTitleKey("Alpha 2") {
		t.Fatal("different titles must stay different")
	}
}
