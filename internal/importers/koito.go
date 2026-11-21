package importers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Koito parses Koito's JSON export (export version "1"):
//
//	{"version":"1","exported_at":"...","user":"...","listens":[{
//	  "listened_at":"RFC3339","client":"...",
//	  "track":{"mbid":null,"duration":215,"aliases":[{"alias":"T","is_primary":true}]},
//	  "album":{"mbid":null,"aliases":[...],"various_artists":false},
//	  "artists":[{"mbid":null,"is_primary":true,"aliases":[{"alias":"A","primary":true}]}]}]}
//
// Koito stores names as aliases; the one flagged primary (else the first) is
// the display name. Track duration is seconds (verified against Koito's own
// test export: 354 for a ~6 minute track). Artists are joined with ", ".
type Koito struct{}

func (Koito) Name() string { return SourceKoito }

type koitoAlias struct {
	Alias string `json:"alias"`
	// Koito's models.Alias tags this field "is_primary" (verified against
	// gabehf/Koito internal/models/alias.go). "primary" is accepted too for
	// hand-edited files.
	IsPrimary bool `json:"is_primary"`
	Primary   bool `json:"primary"`
}

func koitoName(as []koitoAlias) string {
	for _, a := range as {
		if (a.IsPrimary || a.Primary) && a.Alias != "" {
			return a.Alias
		}
	}
	for _, a := range as {
		if a.Alias != "" {
			return a.Alias
		}
	}
	return ""
}

type koitoListen struct {
	ListenedAt string `json:"listened_at"`
	Client     string `json:"client"`
	Track      struct {
		MBID     string       `json:"mbid"`
		Duration float64      `json:"duration"`
		Aliases  []koitoAlias `json:"aliases"`
	} `json:"track"`
	Album struct {
		MBID    string       `json:"mbid"`
		Aliases []koitoAlias `json:"aliases"`
	} `json:"album"`
	Artists []struct {
		MBID      string       `json:"mbid"`
		IsPrimary bool         `json:"is_primary"`
		Aliases   []koitoAlias `json:"aliases"`
	} `json:"artists"`
}

func (Koito) Parse(ctx context.Context, r io.Reader, emit func(Listen) error, onRowErr func(RowError)) (Result, error) {
	rn := newRun(ctx, emit, onRowErr)
	r, _, err := peekFirstByte(r)
	if err == io.EOF {
		return rn.res, nil
	}
	if err != nil {
		return rn.res, err
	}
	dec := json.NewDecoder(r)
	err = eachKeyedArray(dec, "listens", func(idx int, raw json.RawMessage) error {
		rn.res.Rows++
		var k koitoListen
		if err := json.Unmarshal(raw, &k); err != nil {
			rn.rowErr(idx, err, raw)
			return nil
		}
		t, terr := time.Parse(time.RFC3339, k.ListenedAt)
		if terr != nil {
			rn.rowErr(idx, fmt.Errorf("bad listened_at %q", k.ListenedAt), raw)
			return nil
		}
		l := Listen{
			PlayedAt:        t.UTC(),
			Title:           koitoName(k.Track.Aliases),
			Album:           koitoName(k.Album.Aliases),
			RecordingMBID:   k.Track.MBID,
			ReleaseMBID:     k.Album.MBID,
			TrackDurationMs: secToMs(&k.Track.Duration),
			Source:          SourceKoito,
			RawPayload:      append(json.RawMessage(nil), raw...),
		}
		var names []string
		for _, a := range k.Artists {
			if n := koitoName(a.Aliases); n != "" {
				names = append(names, n)
			}
			if a.MBID != "" {
				l.ArtistMBIDs = append(l.ArtistMBIDs, a.MBID)
			}
		}
		l.Artist = strings.Join(names, ", ")
		if err := validate(l); err != nil {
			rn.rowErr(idx, err, raw)
			return nil
		}
		return rn.push(l)
	})
	return rn.res, err
}
