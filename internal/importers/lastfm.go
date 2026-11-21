package importers

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// LastFM parses Last.fm scrobble CSV exports as produced by the common
// third-party exporters (e.g. lastfm.ghan.nl/export, "recenttracks-*.csv"):
//
//	uts,utc_time,artist,artist_mbid,album,album_mbid,track,track_mbid
//
// JSON is accepted too: the user.getRecentTracks page dumps that other tools
// and Koito's importer use, either an array of pages ([{"track":[...]}]) or a
// single API response ({"recenttracks":{"track":[...]}}). Now-playing entries
// (no date) are skipped.
//
// Columns are matched by header name, case-insensitively, in any order.
// Headerless four-column files (artist, album, title, time) are also accepted.
// Last.fm exports carry no durations.
type LastFM struct{}

func (LastFM) Name() string { return SourceLastFM }

var lastfmTimeLayouts = []string{
	"02 Jan 2006, 15:04",
	"02 Jan 2006 15:04",
	"2 Jan 2006, 15:04",
	"2 Jan 2006 15:04",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05Z07:00",
}

func parseLastfmTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range lastfmTimeLayouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", s)
}

func (LastFM) Parse(ctx context.Context, r io.Reader, emit func(Listen) error, onRowErr func(RowError)) (Result, error) {
	rn := newRun(ctx, emit, onRowErr)
	r, first, err := peekFirstByte(r) // strips BOM
	if err == io.EOF {
		return rn.res, nil
	}
	if err != nil {
		return rn.res, err
	}
	if first == '[' || first == '{' {
		err := parseLastfmJSON(rn, r)
		return rn.res, err
	}
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = false

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
				rn.res.Rows++
				rn.rowErr(idx, err, nil)
				continue
			}
			return rn.res, err
		}
		if idx == 1 {
			for i, h := range rec {
				col[strings.ToLower(strings.TrimSpace(h))] = i
			}
			_, hasArtist := col["artist"]
			_, hasTrack := col["track"]
			if _, ok := col["title"]; ok {
				hasTrack = true
				col["track"] = col["title"]
			}
			if hasArtist && hasTrack {
				continue // header row
			}
			if len(rec) != 4 {
				return rn.res, errors.New("not a Last.fm CSV: no header with artist/track columns and not 4 columns wide")
			}
			col = map[string]int{"artist": 0, "album": 1, "track": 2, "time": 3}
		}
		rn.res.Rows++
		get := func(name string) string {
			if i, ok := col[name]; ok && i < len(rec) {
				return rec[i]
			}
			return ""
		}
		raw := strings.Join(rec, ",")
		l := Listen{
			Artist:      get("artist"),
			Album:       get("album"),
			Title:       get("track"),
			TrackMBID:   get("track_mbid"),
			ReleaseMBID: get("album_mbid"),
			Source:      SourceLastFM,
			RawPayload:  nil,
		}
		if m := get("artist_mbid"); m != "" {
			l.ArtistMBIDs = []string{m}
		}
		// Last.fm "track mbid" identifies the recording.
		l.RecordingMBID = l.TrackMBID
		var terr error
		if u := get("uts"); u != "" {
			sec, perr := strconv.ParseInt(strings.TrimSpace(u), 10, 64)
			if perr != nil {
				terr = fmt.Errorf("bad uts %q", u)
			} else {
				l.PlayedAt = time.Unix(sec, 0).UTC()
			}
		} else if t := firstNonEmpty(get("utc_time"), get("time"), get("date")); t != "" {
			l.PlayedAt, terr = parseLastfmTime(t)
		}
		if terr != nil {
			rn.rowErr(idx, terr, []byte(raw))
			continue
		}
		if err := validate(l); err != nil {
			// A currently-playing row in an API dump has no timestamp; count
			// it as an error like any other invalid row.
			rn.rowErr(idx, err, []byte(raw))
			continue
		}
		if err := rn.push(l); err != nil {
			return rn.res, err
		}
	}
	return rn.res, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

type lfmItem struct {
	MBID string `json:"mbid"`
	Text string `json:"#text"`
	Name string `json:"name"`
}

type lfmTrack struct {
	Artist lfmItem `json:"artist"`
	Album  lfmItem `json:"album"`
	Name   string  `json:"name"`
	MBID   string  `json:"mbid"`
	Date   *struct {
		UTS string `json:"uts"`
	} `json:"date"`
	Attr struct {
		NowPlaying string `json:"nowplaying"`
	} `json:"@attr"`
}

func (rn *run) lfmTrack(idx int, raw json.RawMessage) error {
	rn.res.Rows++
	var t lfmTrack
	if err := json.Unmarshal(raw, &t); err != nil {
		rn.rowErr(idx, err, raw)
		return nil
	}
	if t.Attr.NowPlaying == "true" || t.Date == nil {
		rn.skip()
		return nil
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(t.Date.UTS), 10, 64)
	if err != nil {
		rn.rowErr(idx, fmt.Errorf("bad date.uts %q", t.Date.UTS), raw)
		return nil
	}
	l := Listen{
		PlayedAt:      time.Unix(sec, 0).UTC(),
		Artist:        firstNonEmpty(t.Artist.Text, t.Artist.Name),
		Album:         t.Album.Text,
		Title:         t.Name,
		TrackMBID:     t.MBID,
		RecordingMBID: t.MBID,
		ReleaseMBID:   t.Album.MBID,
		Source:        SourceLastFM,
		RawPayload:    append(json.RawMessage(nil), raw...),
	}
	if t.Artist.MBID != "" {
		l.ArtistMBIDs = []string{t.Artist.MBID}
	}
	if err := validate(l); err != nil {
		rn.rowErr(idx, err, raw)
		return nil
	}
	return rn.push(l)
}

// parseLastfmJSON streams either an array of pages or one response object.
func parseLastfmJSON(rn *run, r io.Reader) error {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	n := 0
	if d, _ := tok.(json.Delim); d == '[' {
		for dec.More() {
			var page struct {
				Track json.RawMessage `json:"track"`
			}
			if err := dec.Decode(&page); err != nil {
				return fmt.Errorf("page: %w", err)
			}
			if err := lfmTracks(rn, page.Track, &n); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	}
	return lfmObject(rn, dec, &n)
}

// lfmObject walks an object whose "track" array (possibly nested under
// "recenttracks") is streamed; everything else is skipped.
func lfmObject(rn *run, dec *json.Decoder, n *int) error {
	found := false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		switch kt.(string) {
		case "recenttracks":
			t, err := dec.Token()
			if err != nil {
				return err
			}
			if d, _ := t.(json.Delim); d != '{' {
				return errors.New("recenttracks is not an object")
			}
			if err := lfmObject(rn, dec, n); err != nil {
				return err
			}
			found = true
			if _, err := dec.Token(); err != nil { // '}'
				return err
			}
		case "track":
			found = true
			if err := eachArrayElem(dec, func(_ int, raw json.RawMessage) error {
				*n++
				return rn.lfmTrack(*n, raw)
			}); err != nil {
				return err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
		}
	}
	if !found && *n == 0 && rn.res.Rows == 0 {
		return errors.New("not a Last.fm JSON export: no track array")
	}
	return nil
}

func lfmTracks(rn *run, arr json.RawMessage, n *int) error {
	var els []json.RawMessage
	if err := json.Unmarshal(arr, &els); err != nil {
		return nil // page without tracks
	}
	for _, el := range els {
		*n++
		if err := rn.lfmTrack(*n, el); err != nil {
			return err
		}
	}
	return nil
}
