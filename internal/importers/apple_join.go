package importers

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Apple's privacy export splits what an import needs over two files:
//
//   - "Apple Music Play Activity.csv" has every play with its exact time, song
//     name, album and durations, but no artist and no catalog id;
//   - "Apple Music - Play History Daily Tracks.csv" has the catalog id (Track
//     Identifier) and "Artist - Title" (Track Description) per track and day, but
//     only the day.
//
// Neither is enough alone, so an Apple import always needs both (RoleOf says
// which is which), and AppleJoin pairs them: a play is joined to the Daily
// Tracks row with the same title on the same day (or the day before or after:
// the time zone of the date is not given). The play's artist and Apple song id
// come from that row. A play that cannot be joined to exactly one song id is
// skipped and counted, never guessed. See docs/imports.md.

// Roles of the two Apple files.
const (
	RolePlayActivity = "play_activity"
	RoleDailyTracks  = "daily_tracks"
)

// RoleOf says which Apple export file a CSV header row belongs to, "" when it
// is neither. Daily Tracks is recognised by its Track Identifier column (it also
// carries play durations, so the Play Activity columns alone cannot tell).
func RoleOf(headerLine string) string {
	l := strings.ToLower(headerLine)
	switch {
	case strings.Contains(l, "track identifier"):
		return RoleDailyTracks
	case strings.Contains(l, "event start timestamp") || strings.Contains(l, "song name"):
		return RolePlayActivity
	}
	return ""
}

// AppleSkip is one play that was not imported, with the reason.
type AppleSkip struct {
	Reason   string `json:"reason"`
	Title    string `json:"title"`
	Album    string `json:"album,omitempty"`
	PlayedAt string `json:"played_at"`
}

// AppleJoinStats says what the join did, for the import preview.
type AppleJoinStats struct {
	DailyRows   int `json:"daily_rows"`
	DailyTracks int `json:"daily_tracks"` // usable: a numeric song id and an "Artist - Title"
	Joined      int `json:"joined"`       // plays matched to exactly one song id: imported
	Unjoined    int `json:"unjoined"`     // no Daily Tracks row for that title and day
	Ambiguous   int `json:"ambiguous"`    // more than one song id for that title and day
	// SkippedSample lists the first plays that were skipped (up to 20).
	SkippedSample []AppleSkip `json:"skipped_sample,omitempty"`
}

const skipSampleMax = 20

type dailyTrack struct{ id, artist string }

// dailyIndex maps a normalised title and a day to the songs played then.
type dailyIndex map[string]map[string]dailyTrack // key -> id -> track

func indexKey(title string, day time.Time) string {
	return appleTitleKey(title) + "|" + day.Format("2006-01-02")
}

// appleTitleKey normalises a title for the join: case, spacing and typographic
// quotes do not matter.
func appleTitleKey(s string) string {
	s = strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`, " ", " ").Replace(s)
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func numeric(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// buildDailyIndex reads Daily Tracks. "Artist - Title" has no marked boundary,
// and either side can contain " - ", so a row is indexed under every possible
// split; the play's own song name decides which one is right.
func buildDailyIndex(ctx context.Context, streams []io.Reader, st *AppleJoinStats) (dailyIndex, error) {
	idx := dailyIndex{}
	for _, r := range streams {
		r, _, err := peekFirstByte(r)
		if err == io.EOF {
			continue
		}
		if err != nil {
			return nil, err
		}
		cr := csv.NewReader(r)
		cr.FieldsPerRecord = -1
		cr.LazyQuotes = true
		var iID, iDesc, iDate = -1, -1, -1
		first := true
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			rec, err := cr.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				var pe *csv.ParseError
				if errors.As(err, &pe) {
					continue
				}
				return nil, err
			}
			if first {
				first = false
				for i, h := range rec {
					switch strings.ToLower(strings.TrimSpace(h)) {
					case "track identifier":
						iID = i
					case "track description":
						iDesc = i
					case "date played", "date":
						iDate = i
					}
				}
				if iID < 0 || iDesc < 0 || iDate < 0 {
					return nil, errors.New("Daily Tracks: needs the Track Identifier, Track Description and Date Played columns")
				}
				continue
			}
			st.DailyRows++
			at := func(i int) string {
				if i < len(rec) {
					return strings.TrimSpace(rec[i])
				}
				return ""
			}
			id, desc := at(iID), at(iDesc)
			day, derr := parseAppleTime(at(iDate))
			if !numeric(id) || desc == "" || derr != nil { // radio stations and bookkeeping rows
				continue
			}
			usable := false
			rest := desc
			offset := 0
			for {
				i := strings.Index(rest, " - ")
				if i < 0 {
					break
				}
				artist := strings.TrimSpace(desc[:offset+i])
				title := strings.TrimSpace(desc[offset+i+3:])
				if artist != "" && title != "" {
					k := indexKey(title, day)
					if idx[k] == nil {
						idx[k] = map[string]dailyTrack{}
					}
					if _, dup := idx[k][id]; !dup {
						idx[k][id] = dailyTrack{id: id, artist: artist}
					}
					usable = true
				}
				offset += i + 3
				rest = desc[offset:]
			}
			if usable {
				st.DailyTracks++
			}
		}
	}
	return idx, nil
}

// lookup finds the one song played with that title on that day: the exact day
// first, and only when it has none, the neighbouring days.
func (idx dailyIndex) lookup(title string, at time.Time) (dailyTrack, string) {
	day := at.UTC().Truncate(24 * time.Hour)
	if m := idx[indexKey(title, day)]; len(m) > 0 {
		return only(m)
	}
	merged := map[string]dailyTrack{}
	for _, d := range []int{-1, 1} {
		for id, t := range idx[indexKey(title, day.AddDate(0, 0, d))] {
			merged[id] = t
		}
	}
	return only(merged)
}

// only returns the single track of m, or says why there is not exactly one.
func only(m map[string]dailyTrack) (dailyTrack, string) {
	switch len(m) {
	case 0:
		return dailyTrack{}, "unjoined"
	case 1:
		for _, t := range m {
			return t, ""
		}
	}
	return dailyTrack{}, "ambiguous"
}

// AppleJoin streams the Play Activity files, joins each play to Daily Tracks and
// emits the joined listens (Artist, Title, Album, time, durations and
// AppleSongID). Plays that do not join to exactly one song id are skipped and
// counted in the stats. Result counts what was emitted (Parsed) and everything
// skipped for any reason (Skipped). Both lists must be non-empty: an import
// never starts on half the data.
func AppleJoin(ctx context.Context, play, daily []io.Reader, minPlayedMs int64,
	emit func(Listen) error, onRowErr func(RowError)) (Result, AppleJoinStats, error) {
	var st AppleJoinStats
	var res Result
	if len(play) == 0 || len(daily) == 0 {
		return res, st, errors.New("an Apple Music import needs both Apple Music Play Activity.csv and Apple Music - Play History Daily Tracks.csv")
	}
	idx, err := buildDailyIndex(ctx, daily, &st)
	if err != nil {
		return res, st, err
	}
	skip := func(l Listen, reason, why string) {
		if len(st.SkippedSample) < skipSampleMax {
			st.SkippedSample = append(st.SkippedSample, AppleSkip{Reason: why, Title: l.Title, Album: l.Album,
				PlayedAt: l.PlayedAt.UTC().Format(time.RFC3339)})
		}
	}
	for _, p := range play {
		inner := AppleMusic{MinPlayedMs: minPlayedMs, NoArtist: true}
		r, err := inner.Parse(ctx, p, func(l Listen) error {
			t, why := idx.lookup(l.Title, l.PlayedAt)
			switch why {
			case "unjoined":
				st.Unjoined++
				skip(l, why, "no Daily Tracks row for this title on that day (or the days either side)")
				return nil
			case "ambiguous":
				st.Ambiguous++
				skip(l, why, "more than one Apple song id for this title on that day")
				return nil
			}
			st.Joined++
			l.Artist, l.AppleSongID = t.artist, t.id
			res.Parsed++
			if res.Earliest.IsZero() || l.PlayedAt.Before(res.Earliest) {
				res.Earliest = l.PlayedAt
			}
			if l.PlayedAt.After(res.Latest) {
				res.Latest = l.PlayedAt
			}
			return emit(l)
		}, onRowErr)
		res.Rows += r.Rows
		res.Skipped += r.Skipped
		res.Errors += r.Errors
		if err != nil {
			return res, st, fmt.Errorf("Play Activity: %w", err)
		}
	}
	res.Skipped += st.Unjoined + st.Ambiguous
	return res, st, nil
}
