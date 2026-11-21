package importers

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// DefaultAppleMinPlayedMs is the cut-off below which an Apple Music play is
// not considered a listen (same ~30s rule as Spotify).
const DefaultAppleMinPlayedMs = 30_000

// AppleMusic parses the CSVs from Apple's privacy data export (Data & Privacy
// request → "Apple Media Services information"): "Apple Music Play
// Activity.csv" and "Apple Music - Play History Daily Tracks.csv".
//
// Columns are matched by header name, case-insensitively, in any order; the
// files differ between export vintages, so several aliases are accepted:
//
//	title    Song Name | Title | Track Name | Original Title
//	artist   Artist Name | Artist   (or "Track Description" as "Artist - Title")
//	album    Album Name | Container Album Name | Album
//	start    Event Start Timestamp | Event End Timestamp (minus play duration)
//	         | Event Received Timestamp | Date Played
//	played   Play Duration Milliseconds   (-1 or blank = unknown)
//	length   Media Duration In Milliseconds
//
// Durations are unreliable in these exports, so a play is skipped only when a
// duration is present and under MinPlayedMs. Video rows and rows with no title
// at all (bookkeeping events) are skipped. This format is implemented from
// documentation and user reports, not from a live export.
type AppleMusic struct {
	// MinPlayedMs: 0 uses DefaultAppleMinPlayedMs, negative disables the filter.
	MinPlayedMs int64
	// NoArtist reads Play Activity rows, which have no artist: a row needs only
	// a title and a time here, and AppleJoin supplies the artist and the catalog
	// id from Daily Tracks.
	NoArtist bool
}

func (AppleMusic) Name() string { return SourceAppleMusic }

var appleTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.000Z",
	"2006-01-02T15:04:05Z",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"20060102",
	"2006-01-02",
}

func parseAppleTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range appleTimeLayouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", s)
}

// IsAppleMusicHeader reports whether a CSV header row looks like an Apple
// Music play-history file. Used for format detection.
func IsAppleMusicHeader(line string) bool {
	l := strings.ToLower(line)
	return strings.Contains(l, "event start timestamp") ||
		strings.Contains(l, "play duration milliseconds") ||
		strings.Contains(l, "track description")
}

func (p AppleMusic) Parse(ctx context.Context, r io.Reader, emit func(Listen) error, onRowErr func(RowError)) (Result, error) {
	rn := newRun(ctx, emit, onRowErr)
	min := p.MinPlayedMs
	if min == 0 {
		min = DefaultAppleMinPlayedMs
	}
	r, _, err := peekFirstByte(r) // strips BOM
	if err == io.EOF {
		return rn.res, nil
	}
	if err != nil {
		return rn.res, err
	}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true

	col := map[string]int{}
	idx := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		idx++
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				if idx > 1 {
					rn.res.Rows++
					rn.rowErr(idx, err, nil)
				}
				continue
			}
			return rn.res, err
		}
		if idx == 1 {
			for i, h := range rec {
				col[strings.ToLower(strings.TrimSpace(h))] = i
			}
			if !IsAppleMusicHeader(strings.Join(rec, ",")) {
				return rn.res, errors.New("not an Apple Music play history CSV: no Event Start Timestamp / Play Duration Milliseconds / Track Description column")
			}
			continue
		}
		rn.res.Rows++
		get := func(names ...string) string {
			for _, n := range names {
				if i, ok := col[n]; ok && i < len(rec) {
					if v := strings.TrimSpace(rec[i]); v != "" {
						return v
					}
				}
			}
			return ""
		}
		raw := strings.Join(rec, ",")

		title := get("song name", "title", "track name", "original title")
		artist := get("artist name", "artist")
		if desc := get("track description"); desc != "" && (title == "" || artist == "") {
			if a, t, ok := strings.Cut(desc, " - "); ok {
				if artist == "" {
					artist = strings.TrimSpace(a)
				}
				if title == "" {
					title = strings.TrimSpace(t)
				}
			} else if title == "" {
				title = desc
			}
		}
		if title == "" || strings.EqualFold(get("media type"), "video") {
			rn.skip()
			continue
		}

		played := appleMs(get("play duration milliseconds"))
		if played < 0 {
			if s, e := appleMs(get("start position in milliseconds")), appleMs(get("end position in milliseconds")); s >= 0 && e > s {
				played = e - s
			}
		}
		if min > 0 && played >= 0 && played < min {
			rn.skip()
			continue
		}

		l := Listen{
			Artist:          artist,
			Title:           title,
			Album:           get("album name", "container album name", "album"),
			TrackDurationMs: max(appleMs(get("media duration in milliseconds")), 0),
			Source:          SourceAppleMusic,
		}
		if played > 0 {
			l.DurationPlayedMs = played
		}
		var terr error
		if ts := get("event start timestamp", "date played"); ts != "" {
			l.PlayedAt, terr = parseAppleTime(ts)
		} else if ts := get("event end timestamp"); ts != "" {
			l.PlayedAt, terr = parseAppleTime(ts)
			if terr == nil && played > 0 {
				l.PlayedAt = l.PlayedAt.Add(-time.Duration(played) * time.Millisecond)
			}
		} else if ts := get("event received timestamp"); ts != "" {
			l.PlayedAt, terr = parseAppleTime(ts)
		}
		if terr != nil {
			rn.rowErr(idx, terr, []byte(raw))
			continue
		}
		var verr error
		if p.NoArtist {
			switch {
			case l.PlayedAt.IsZero() || l.PlayedAt.Unix() <= 0:
				verr = errors.New("missing or invalid timestamp")
			case l.Title == "":
				verr = errors.New("missing track title")
			}
		} else {
			verr = validate(l)
		}
		if verr != nil {
			rn.rowErr(idx, verr, []byte(raw))
			continue
		}
		if err := rn.push(l); err != nil {
			return rn.res, err
		}
	}
	return rn.res, nil
}

// appleMs parses a millisecond column; blank, junk and negative values
// (Apple uses -1) are "unknown" and return -1.
func appleMs(s string) int64 {
	if s == "" {
		return -1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil || f < 0 {
			return -1
		}
		return int64(f)
	}
	if n < 0 {
		return -1
	}
	return n
}
