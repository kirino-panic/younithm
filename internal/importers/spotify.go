package importers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// DefaultSpotifyMinPlayedMs is the cut-off below which a Spotify stream is not
// considered a listen.
const DefaultSpotifyMinPlayedMs = 30_000

// Spotify parses one Spotify "Extended streaming history" file
// (Streaming_History_Audio_*.json, a JSON array). The older account-data
// format (StreamingHistory*.json: endTime/artistName/trackName/msPlayed) is
// accepted too. Feed one file per call; the job layer iterates files of a zip.
//
// Spotify's ts / endTime is when the stream ENDED, so PlayedAt is derived as
// end minus ms_played. Podcast episodes and audiobooks (no track metadata) are
// skipped, as are plays shorter than MinPlayedMs.
type Spotify struct {
	// MinPlayedMs: 0 uses DefaultSpotifyMinPlayedMs, negative disables the filter.
	MinPlayedMs int64
}

func (Spotify) Name() string { return SourceSpotify }

type spotifyExt struct {
	TS         string  `json:"ts"`
	MsPlayed   int64   `json:"ms_played"`
	Track      *string `json:"master_metadata_track_name"`
	Artist     *string `json:"master_metadata_album_artist_name"`
	Album      *string `json:"master_metadata_album_album_name"`
	URI        *string `json:"spotify_track_uri"`
	EpisodeURI *string `json:"spotify_episode_uri"`
	// legacy format
	EndTime    string `json:"endTime"`
	ArtistName string `json:"artistName"`
	TrackName  string `json:"trackName"`
	LegacyMs   *int64 `json:"msPlayed"`
}

func (p Spotify) Parse(ctx context.Context, r io.Reader, emit func(Listen) error, onRowErr func(RowError)) (Result, error) {
	rn := newRun(ctx, emit, onRowErr)
	min := p.MinPlayedMs
	if min == 0 {
		min = DefaultSpotifyMinPlayedMs
	}
	r, _, err := peekFirstByte(r)
	if err == io.EOF {
		return rn.res, nil
	}
	if err != nil {
		return rn.res, err
	}
	dec := json.NewDecoder(r)
	err = eachArrayElem(dec, func(idx int, raw json.RawMessage) error {
		rn.res.Rows++
		var s spotifyExt
		if err := json.Unmarshal(raw, &s); err != nil {
			rn.rowErr(idx, err, raw)
			return nil
		}
		var l Listen
		var played int64
		var end time.Time
		if s.EndTime != "" || s.LegacyMs != nil {
			t, err := time.ParseInLocation("2006-01-02 15:04", s.EndTime, time.UTC)
			if err != nil {
				rn.rowErr(idx, fmt.Errorf("bad endTime %q", s.EndTime), raw)
				return nil
			}
			end = t
			if s.LegacyMs != nil {
				played = *s.LegacyMs
			}
			l.Artist, l.Title = s.ArtistName, s.TrackName
		} else {
			t, err := time.Parse(time.RFC3339, s.TS)
			if err != nil {
				rn.rowErr(idx, fmt.Errorf("bad ts %q", s.TS), raw)
				return nil
			}
			end = t.UTC()
			played = s.MsPlayed
			l.Artist, l.Title, l.Album = ptrStr(s.Artist), ptrStr(s.Track), ptrStr(s.Album)
			if s.URI != nil {
				l.SpotifyTrackID = spotifyIDFromAny(*s.URI)
				l.SourceID = *s.URI
			}
			if l.Title == "" && l.Artist == "" {
				// podcast episode, audiobook, or removed track: not a music listen
				rn.skip()
				return nil
			}
		}
		if played < 0 {
			rn.rowErr(idx, fmt.Errorf("negative ms_played"), raw)
			return nil
		}
		if min > 0 && played < min {
			rn.skip()
			return nil
		}
		l.PlayedAt = end.Add(-time.Duration(played) * time.Millisecond).UTC()
		l.DurationPlayedMs = played
		l.Source = SourceSpotify
		l.RawPayload = append(json.RawMessage(nil), raw...)
		if err := validate(l); err != nil {
			rn.rowErr(idx, err, raw)
			return nil
		}
		return rn.push(l)
	})
	return rn.res, err
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
