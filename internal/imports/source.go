package imports

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"younithm/internal/importers"
)

// ErrUnknownFormat is returned when auto-detection cannot decide.
var ErrUnknownFormat = errors.New("could not detect the file format; choose one explicitly")

// FormatNames lists the supported format identifiers.
func FormatNames() []string {
	var n []string
	for k := range importers.Registry() {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func isZip(head []byte) bool { return bytes.HasPrefix(head, []byte("PK\x03\x04")) }

// Detect guesses the format of the file at path from its first 64 KiB (or,
// for a zip, its entry names). It reads only a prefix.
func Detect(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 64*1024)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if isZip(head) {
		st, err := f.Stat()
		if err != nil {
			return "", err
		}
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			return "", fmt.Errorf("unreadable zip: %w", err)
		}
		for _, e := range zr.File {
			ln := strings.ToLower(e.Name)
			switch {
			case strings.HasSuffix(ln, ".jsonl") && (strings.HasPrefix(ln, "listens/") || strings.Contains(ln, "/listens/")):
				return importers.SourceListenBrainz, nil
			case spotifyEntry(ln):
				return importers.SourceSpotify, nil
			case appleEntry(ln):
				return importers.SourceAppleMusic, nil
			}
		}
		return "", ErrUnknownFormat
	}
	return sniffText(head)
}

func sniffText(head []byte) (string, error) {
	head = bytes.TrimPrefix(head, []byte{0xEF, 0xBB, 0xBF})
	t := bytes.TrimLeft(head, " \t\r\n")
	if len(t) == 0 {
		return "", ErrUnknownFormat
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if bytes.Contains(t, []byte(s)) {
				return true
			}
		}
		return false
	}
	switch t[0] {
	case '[':
		switch {
		case has(`"ms_played"`, `"msPlayed"`, `"master_metadata_track_name"`, `"trackName"`):
			return importers.SourceSpotify, nil
		case has(`"track_metadata"`, `"listened_at"`):
			return importers.SourceListenBrainz, nil
		case has(`"#text"`, `"uts"`):
			return importers.SourceLastFM, nil
		}
	case '{':
		switch {
		case has(`"recenttracks"`, `"#text"`):
			return importers.SourceLastFM, nil
		case has(`"scrobbles"`, `"maloja"`):
			return importers.SourceMaloja, nil
		case has(`"track_metadata"`, `"payload"`):
			return importers.SourceListenBrainz, nil
		case has(`"listens"`) && has(`"aliases"`, `"version"`):
			return importers.SourceKoito, nil
		case has(`"listened_at"`):
			return importers.SourceListenBrainz, nil
		}
	default:
		// CSV: Last.fm exporters, with a header naming artist and track.
		line, _, _ := bytes.Cut(t, []byte("\n"))
		l := strings.ToLower(string(line))
		if importers.IsAppleMusicHeader(l) {
			return importers.SourceAppleMusic, nil
		}
		if strings.Contains(l, "artist") && (strings.Contains(l, "track") || strings.Contains(l, "title")) {
			return importers.SourceLastFM, nil
		}
		if bytes.Count(line, []byte(",")) == 3 { // headerless artist,album,title,time
			return importers.SourceLastFM, nil
		}
	}
	return "", ErrUnknownFormat
}

func spotifyEntry(lowerName string) bool {
	if !strings.HasSuffix(lowerName, ".json") || strings.Contains(lowerName, "video") {
		return false
	}
	return strings.Contains(lowerName, "streaming_history_audio") || strings.Contains(lowerName, "endsong")
}

// appleEntry matches the play-history CSVs inside Apple's privacy export zip,
// which also holds many unrelated CSVs (library, favourites, ...).
func appleEntry(lowerName string) bool {
	if !strings.HasSuffix(lowerName, ".csv") {
		return false
	}
	return strings.Contains(lowerName, "play activity") || strings.Contains(lowerName, "play history")
}

func parserFor(format string, opts JobOptions) (importers.Parser, error) {
	if requiredFiles(format) != nil { // never run a multi-file format on one file
		return nil, fmt.Errorf("%s needs more than one file and cannot be parsed as a single one", format)
	}
	if format == importers.SourceSpotify {
		return importers.Spotify{MinPlayedMs: opts.SpotifyMinMs}, nil
	}
	p, ok := importers.Registry()[format]
	if !ok {
		return nil, fmt.Errorf("unknown format %q (supported: %s)", format, strings.Join(FormatNames(), ", "))
	}
	return p, nil
}

// ParseFile stream-parses the upload at path (plain file or zip) with the
// parser for format, calling emit per listen. Nothing is loaded whole.
func ParseFile(ctx context.Context, path, format string, opts JobOptions,
	emit func(importers.Listen) error, onRowErr func(importers.RowError)) (importers.Result, error) {
	p, err := parserFor(format, opts)
	if err != nil {
		return importers.Result{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return importers.Result{}, err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	if !isZip(head[:n]) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return importers.Result{}, err
		}
		return p.Parse(ctx, f, emit, onRowErr)
	}
	st, err := f.Stat()
	if err != nil {
		return importers.Result{}, err
	}
	if format == importers.SourceListenBrainz {
		return importers.ListenBrainzZip(ctx, f, st.Size(), emit, onRowErr)
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return importers.Result{}, fmt.Errorf("opening zip: %w", err)
	}
	var files []*zip.File
	for _, e := range zr.File {
		ln := strings.ToLower(e.Name)
		if e.FileInfo().IsDir() || strings.HasPrefix(ln, "__macosx/") {
			continue
		}
		if format == importers.SourceSpotify {
			if spotifyEntry(ln) {
				files = append(files, e)
			}
		} else if format == importers.SourceAppleMusic {
			if appleEntry(ln) {
				files = append(files, e)
			}
		} else if strings.HasSuffix(ln, ".json") || strings.HasSuffix(ln, ".csv") {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		return importers.Result{}, fmt.Errorf("no %s files found in the zip", format)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	var total importers.Result
	for _, e := range files {
		rc, err := e.Open()
		if err != nil {
			return total, err
		}
		res, perr := p.Parse(ctx, rc, emit, onRowErr)
		rc.Close()
		total.Rows += res.Rows
		total.Parsed += res.Parsed
		total.Skipped += res.Skipped
		total.Errors += res.Errors
		if !res.Earliest.IsZero() && (total.Earliest.IsZero() || res.Earliest.Before(total.Earliest)) {
			total.Earliest = res.Earliest
		}
		if res.Latest.After(total.Latest) {
			total.Latest = res.Latest
		}
		if perr != nil {
			return total, fmt.Errorf("%s: %w", e.Name, perr)
		}
	}
	return total, nil
}
