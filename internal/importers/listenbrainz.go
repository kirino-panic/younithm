package importers

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ListenBrainz parses ListenBrainz listen exports. Accepted layouts:
//   - JSON Lines, one listen per line (the files in the export zip, and what
//     the export API produces);
//   - a top-level JSON array of listens;
//   - an API-style envelope {"payload":{"listens":[...]}} or {"payload":[...]}.
//
// Use ListenBrainzZip for the full export archive.
type ListenBrainz struct{}

func (ListenBrainz) Name() string { return SourceListenBrainz }

type lbListen struct {
	ListenedAt    *int64  `json:"listened_at"`
	RecordingMSID string  `json:"recording_msid"`
	TrackMeta     lbTrack `json:"track_metadata"`
}

type lbTrack struct {
	ArtistName  string `json:"artist_name"`
	TrackName   string `json:"track_name"`
	ReleaseName string `json:"release_name"`
	Info        struct {
		RecordingMBID    string   `json:"recording_mbid"`
		TrackMBID        string   `json:"track_mbid"`
		ReleaseMBID      string   `json:"release_mbid"`
		ReleaseGroupMBID string   `json:"release_group_mbid"`
		ArtistMBIDs      []string `json:"artist_mbids"`
		ISRC             string   `json:"isrc"`
		SpotifyID        string   `json:"spotify_id"`
		DurationMs       *int64   `json:"duration_ms"`
		Duration         *float64 `json:"duration"`
		DurationPlayedMs *int64   `json:"duration_played_ms"`
		DurationPlayed   *float64 `json:"duration_played"`
	} `json:"additional_info"`
	Mapping *struct {
		RecordingMBID string   `json:"recording_mbid"`
		ReleaseMBID   string   `json:"release_mbid"`
		ArtistMBIDs   []string `json:"artist_mbids"`
	} `json:"mbid_mapping"`
}

func secToMs(f *float64) int64 {
	if f == nil || *f <= 0 {
		return 0
	}
	return int64(*f*1000 + 0.5)
}

func (l lbListen) normalize(raw []byte) (Listen, error) {
	if l.ListenedAt == nil {
		return Listen{}, fmt.Errorf("missing listened_at")
	}
	t := l.TrackMeta
	out := Listen{
		PlayedAt:         time.Unix(*l.ListenedAt, 0).UTC(),
		Artist:           t.ArtistName,
		Title:            t.TrackName,
		Album:            t.ReleaseName,
		RecordingMBID:    t.Info.RecordingMBID,
		TrackMBID:        t.Info.TrackMBID,
		ReleaseMBID:      t.Info.ReleaseMBID,
		ReleaseGroupMBID: t.Info.ReleaseGroupMBID,
		ArtistMBIDs:      t.Info.ArtistMBIDs,
		ISRC:             t.Info.ISRC,
		SpotifyTrackID:   spotifyIDFromAny(t.Info.SpotifyID),
		Source:           SourceListenBrainz,
		SourceID:         l.RecordingMSID,
		RawPayload:       append(json.RawMessage(nil), raw...),
	}
	// Submitted IDs win; fall back to the server-side mapping (the same
	// precedence ListenBrainz documents, minus manual mappings we can't see).
	if m := t.Mapping; m != nil {
		if out.RecordingMBID == "" {
			out.RecordingMBID = m.RecordingMBID
		}
		if out.ReleaseMBID == "" {
			out.ReleaseMBID = m.ReleaseMBID
		}
		if len(out.ArtistMBIDs) == 0 {
			out.ArtistMBIDs = m.ArtistMBIDs
		}
	}
	if t.Info.DurationMs != nil && *t.Info.DurationMs > 0 {
		out.TrackDurationMs = *t.Info.DurationMs
	} else {
		out.TrackDurationMs = secToMs(t.Info.Duration)
	}
	if t.Info.DurationPlayedMs != nil && *t.Info.DurationPlayedMs > 0 {
		out.DurationPlayedMs = *t.Info.DurationPlayedMs
	} else {
		out.DurationPlayedMs = secToMs(t.Info.DurationPlayed)
	}
	if err := validate(out); err != nil {
		return Listen{}, err
	}
	return out, nil
}

// spotifyIDFromAny accepts a URL (https://open.spotify.com/track/ID), a URI
// (spotify:track:ID) or a bare ID.
func spotifyIDFromAny(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "spotify:") {
		parts := strings.Split(s, ":")
		return parts[len(parts)-1]
	}
	if i := strings.Index(s, "open.spotify.com/"); i >= 0 {
		p := s[i:]
		if q := strings.IndexAny(p, "?#"); q >= 0 {
			p = p[:q]
		}
		return path.Base(p)
	}
	return s
}

func (p ListenBrainz) Parse(ctx context.Context, r io.Reader, emit func(Listen) error, onRowErr func(RowError)) (Result, error) {
	rn := newRun(ctx, emit, onRowErr)
	err := p.parseInto(rn, r)
	return rn.res, err
}

func (ListenBrainz) handle(rn *run, idx int, raw []byte) error {
	rn.res.Rows++
	var l lbListen
	if err := json.Unmarshal(raw, &l); err != nil {
		rn.rowErr(idx, err, raw)
		return nil
	}
	out, err := l.normalize(raw)
	if err != nil {
		rn.rowErr(idx, err, raw)
		return nil
	}
	return rn.push(out)
}

func (p ListenBrainz) parseInto(rn *run, r io.Reader) error {
	r, first, err := peekFirstByte(r)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	if first != '[' && first != '{' {
		return fmt.Errorf("not JSON (starts with %q)", first)
	}
	if first == '{' {
		// JSON Lines: one object per line, so a bad line is a row error and
		// parsing resumes at the next line. A single-line API envelope is
		// also recognised.
		return eachJSONLine(r, func(idx int, line []byte) error {
			if isEnvelope(line) {
				return p.envelope(rn, line)
			}
			return p.handle(rn, idx, line)
		})
	}
	dec := json.NewDecoder(r)
	return eachArrayElem(dec, func(idx int, raw json.RawMessage) error { return p.handle(rn, idx, raw) })
}

func isEnvelope(raw []byte) bool {
	var probe struct {
		Payload json.RawMessage `json:"payload"`
	}
	return json.Unmarshal(raw, &probe) == nil && len(probe.Payload) > 0
}

func (p ListenBrainz) envelope(rn *run, raw []byte) error {
	var env struct {
		Payload json.RawMessage `json:"payload"`
	}
	_ = json.Unmarshal(raw, &env)
	var arr []json.RawMessage
	if err := json.Unmarshal(env.Payload, &arr); err != nil {
		var obj struct {
			Listens []json.RawMessage `json:"listens"`
		}
		if err2 := json.Unmarshal(env.Payload, &obj); err2 != nil {
			return fmt.Errorf("unrecognised payload envelope")
		}
		arr = obj.Listens
	}
	for i, el := range arr {
		if err := p.handle(rn, i+1, el); err != nil {
			return err
		}
	}
	return nil
}

// ListenBrainzZip parses a ListenBrainz export archive: every listens/**.jsonl
// entry in chronological (year, month) order. The zip central
// directory needs random access, entries are streamed one at a time.
func ListenBrainzZip(ctx context.Context, ra io.ReaderAt, size int64, emit func(Listen) error, onRowErr func(RowError)) (Result, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return Result{}, fmt.Errorf("opening zip: %w", err)
	}
	var files []*zip.File
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		if strings.HasSuffix(n, ".jsonl") && (strings.HasPrefix(n, "listens/") || strings.Contains(n, "/listens/")) {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return Result{}, fmt.Errorf("no listens/*.jsonl entries in archive")
	}
	// Month files are named 1.jsonl .. 12.jsonl (verified against
	// listenbrainz-server background/export.py), so plain lexical order would
	// put 10 before 2; compare numeric path segments numerically.
	sort.Slice(files, func(i, j int) bool { return lessNatural(files[i].Name, files[j].Name) })
	rn := newRun(ctx, emit, onRowErr)
	for _, f := range files {
		rc, err := f.Open()
		if err != nil {
			return rn.res, err
		}
		perr := ListenBrainz{}.parseInto(rn, rc)
		rc.Close()
		if perr != nil {
			return rn.res, fmt.Errorf("%s: %w", f.Name, perr)
		}
	}
	return rn.res, nil
}

// lessNatural orders paths segment by segment, comparing all-digit segments
// (after trimming a ".jsonl" suffix) numerically.
func lessNatural(a, b string) bool {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := strings.TrimSuffix(as[i], ".jsonl"), strings.TrimSuffix(bs[i], ".jsonl")
		if x == y {
			continue
		}
		xn, e1 := strconv.Atoi(x)
		yn, e2 := strconv.Atoi(y)
		if e1 == nil && e2 == nil {
			return xn < yn
		}
		return x < y
	}
	return len(as) < len(bs)
}
