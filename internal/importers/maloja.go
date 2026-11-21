package importers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Maloja parses the JSON export from a Maloja instance's admin page:
//
//	{"maloja": {...}, "scrobbles": [{"time": 1700000000, "duration": 215,
//	  "track": {"artists": ["A","B"], "title": "T", "length": 230,
//	            "album": {"albumtitle": "Al", "artists": ["A"]}}, "origin": "..."}]}
//
// time is Unix seconds; duration and length are seconds and may be null.
// The "scrobbles" array is streamed; other top-level keys are skipped.
type Maloja struct{}

func (Maloja) Name() string { return SourceMaloja }

type malojaScrobble struct {
	Time     *int64   `json:"time"`
	Duration *float64 `json:"duration"`
	Origin   string   `json:"origin"`
	Track    struct {
		Artists []string `json:"artists"`
		Title   string   `json:"title"`
		Length  *float64 `json:"length"`
		Album   *struct {
			Title   string   `json:"albumtitle"`
			Artists []string `json:"artists"`
		} `json:"album"`
	} `json:"track"`
}

func (Maloja) Parse(ctx context.Context, r io.Reader, emit func(Listen) error, onRowErr func(RowError)) (Result, error) {
	rn := newRun(ctx, emit, onRowErr)
	r, _, err := peekFirstByte(r)
	if err == io.EOF {
		return rn.res, nil
	}
	if err != nil {
		return rn.res, err
	}
	dec := json.NewDecoder(r)
	err = eachKeyedArray(dec, "scrobbles", func(idx int, raw json.RawMessage) error {
		rn.res.Rows++
		var s malojaScrobble
		if err := json.Unmarshal(raw, &s); err != nil {
			rn.rowErr(idx, err, raw)
			return nil
		}
		if s.Time == nil {
			rn.rowErr(idx, fmt.Errorf("missing time"), raw)
			return nil
		}
		l := Listen{
			PlayedAt:         time.Unix(*s.Time, 0).UTC(),
			Artist:           strings.Join(s.Track.Artists, ", "),
			Title:            s.Track.Title,
			DurationPlayedMs: secToMs(s.Duration),
			TrackDurationMs:  secToMs(s.Track.Length),
			Source:           SourceMaloja,
			RawPayload:       append(json.RawMessage(nil), raw...),
		}
		if a := s.Track.Album; a != nil {
			l.Album = a.Title
			l.AlbumArtist = strings.Join(a.Artists, ", ")
		}
		if err := validate(l); err != nil {
			rn.rowErr(idx, err, raw)
			return nil
		}
		return rn.push(l)
	})
	return rn.res, err
}
