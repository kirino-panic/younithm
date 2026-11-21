// Package importers contains pure, streaming file parsers that turn exports
// from other scrobble services into a normalized Listen stream.
//
// Parsers know nothing about the database, dedupe, matching or jobs
// (see internal/imports). They only read an io.Reader and call emit for every
// valid row; bad rows are reported through onRowErr and parsing continues.
package importers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Source identifiers used in Listen.Source and the registry.
const (
	SourceListenBrainz = "listenbrainz"
	SourceLastFM       = "lastfm"
	SourceSpotify      = "spotify"
	SourceMaloja       = "maloja"
	SourceKoito        = "koito"
	SourceAppleMusic   = "applemusic"
)

// Listen is the normalized representation of one play. Strings are raw,
// exactly as found in the source (raw values are never overwritten). Zero values mean "unknown".
type Listen struct {
	PlayedAt         time.Time // UTC, playback start
	DurationPlayedMs int64     // how long it was actually played
	TrackDurationMs  int64     // full track length

	Artist      string
	Title       string
	Album       string
	AlbumArtist string

	RecordingMBID    string
	TrackMBID        string
	ReleaseMBID      string
	ReleaseGroupMBID string
	ArtistMBIDs      []string
	ISRC             string
	SpotifyTrackID   string // bare base62 id, not the URI
	AppleSongID      string // bare numeric Apple Music catalog id

	Source   string // one of the Source* constants
	SourceID string // stable source-side id when one exists (e.g. LB recording_msid)

	RawPayload json.RawMessage // original record, for scrobble.raw_payload
}

// RowError describes one row that could not be turned into a Listen.
type RowError struct {
	Index int    // 1-based record index (physical line for JSONL/CSV, element for arrays)
	Err   error  // reason
	Raw   string // truncated copy of the offending record
}

func (e RowError) Error() string { return fmt.Sprintf("row %d: %v", e.Index, e.Err) }

// Result summarises a parse run. Skipped rows are valid but intentionally
// ignored (e.g. Spotify plays under the minimum duration, podcasts).
type Result struct {
	Rows     int // records seen
	Parsed   int // listens emitted
	Skipped  int
	Errors   int
	Earliest time.Time
	Latest   time.Time
}

// Parser is the plugin interface. Parse streams r, calling emit for each
// Listen. emit returning an error aborts parsing with that error. onRowErr
// may be nil. A non-nil returned error means the stream itself is unusable
// (I/O error, syntactically broken JSON, context cancelled); per-row problems
// never produce one.
type Parser interface {
	Name() string
	Parse(ctx context.Context, r io.Reader, emit func(Listen) error, onRowErr func(RowError)) (Result, error)
}

// Registry returns the available parsers by name.
func Registry() map[string]Parser {
	ps := []Parser{ListenBrainz{}, LastFM{}, Spotify{}, Maloja{}, Koito{}, AppleMusic{}}
	m := make(map[string]Parser, len(ps))
	for _, p := range ps {
		m[p.Name()] = p
	}
	return m
}

// run is shared bookkeeping for all parsers.
type run struct {
	ctx      context.Context
	emit     func(Listen) error
	onRowErr func(RowError)
	res      Result
}

func newRun(ctx context.Context, emit func(Listen) error, onRowErr func(RowError)) *run {
	return &run{ctx: ctx, emit: emit, onRowErr: onRowErr}
}

func (r *run) rowErr(idx int, err error, raw []byte) {
	r.res.Errors++
	if r.onRowErr != nil {
		s := string(raw)
		if len(s) > 512 {
			s = s[:512] + "..."
		}
		r.onRowErr(RowError{Index: idx, Err: err, Raw: s})
	}
}

// push counts and emits one listen. Listens without artist, title or a
// timestamp are rejected as row errors by the caller via validate.
func (r *run) push(l Listen) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	r.res.Parsed++
	if r.res.Earliest.IsZero() || l.PlayedAt.Before(r.res.Earliest) {
		r.res.Earliest = l.PlayedAt
	}
	if l.PlayedAt.After(r.res.Latest) {
		r.res.Latest = l.PlayedAt
	}
	return r.emit(l)
}

func (r *run) skip() { r.res.Skipped++ }

// validate checks the minimum a Listen needs.
func validate(l Listen) error {
	switch {
	case l.PlayedAt.IsZero() || l.PlayedAt.Unix() <= 0:
		return fmt.Errorf("missing or invalid timestamp")
	case l.Title == "":
		return fmt.Errorf("missing track title")
	case l.Artist == "":
		return fmt.Errorf("missing artist")
	}
	return nil
}
