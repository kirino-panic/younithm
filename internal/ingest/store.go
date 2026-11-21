// Package ingest implements ListenBrainz-compatible scrobble ingestion, the
// now-playing store, and the scrobble list/detail read API.
// See docs/api.md.
package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
)

// TimeFormat is how scrobble.played_at is stored (UTC, second precision).
const TimeFormat = "2006-01-02T15:04:05Z"

// NewScrobble is one normalized listen to store. Raw values are kept exactly
// as submitted.
type NewScrobble struct {
	UserID           int64 // 0 means the default user (1)
	PlayedAt         time.Time
	DurationPlayedMs int64
	TrackDurationMs  int64
	Artist, Title    string
	Album            string
	RecordingMBID    string
	ReleaseMBID      string
	ArtistMBIDs      []string
	ISRC             string
	Source           string
	RawPayload       json.RawMessage
}

// Store is the single write path for scrobbles.
type Store struct {
	DB              *sql.DB
	DedupeTolerance time.Duration
	// Identify finds or creates the
	// artists, recording and release the listen names (docs/identity.md).
	// It runs inside the insert transaction; an error aborts the batch.
	Identify func(ctx context.Context, tx *sql.Tx, l NewScrobble) (recordingID, releaseID int64, err error)
	mu       sync.Mutex
}

// Normalize lowercases, trims and collapses whitespace for dedupe comparison.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// Insert stores listens in one transaction, skipping duplicates
// (same user, played_at within tolerance, same normalized artist+title).
func (s *Store) Insert(ctx context.Context, ls []NewScrobble) (accepted, duplicates int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tol := s.DedupeTolerance
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	for _, l := range ls {
		if l.UserID == 0 {
			l.UserID = 1
		}
		at := l.PlayedAt.UTC()
		rows, err := tx.QueryContext(ctx,
			`SELECT raw_artist, raw_title FROM scrobble WHERE user_id=? AND played_at BETWEEN ? AND ?`,
			l.UserID, at.Add(-tol).Format(TimeFormat), at.Add(tol).Format(TimeFormat))
		if err != nil {
			return 0, 0, err
		}
		dup := false
		na, nt := Normalize(l.Artist), Normalize(l.Title)
		for rows.Next() {
			var a, t string
			if err := rows.Scan(&a, &t); err != nil {
				rows.Close()
				return 0, 0, err
			}
			if Normalize(a) == na && Normalize(t) == nt {
				dup = true
			}
		}
		rows.Close()
		if dup {
			duplicates++
			continue
		}
		var payload, album, source, dur any
		if len(l.RawPayload) > 0 {
			payload = string(l.RawPayload)
		}
		if l.Album != "" {
			album = l.Album
		}
		if l.Source != "" {
			source = l.Source
		}
		if l.DurationPlayedMs > 0 {
			dur = l.DurationPlayedMs
		}
		var recID, relID any
		if s.Identify != nil {
			r, rel, err := s.Identify(ctx, tx, l)
			if err != nil {
				return 0, 0, err
			}
			if r != 0 {
				recID = r
				if rel != 0 {
					relID = rel
				}
			}
		}
		if recID == nil && s.Identify != nil && Normalize(l.Title) != "" {
			// Every titled listen maps to a recording (docs/identity.md);
			// the identity resolver returning none would break that rule.
			return 0, 0, errors.New("ingest: identity resolution returned no recording for a titled listen")
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO scrobble (user_id, played_at, duration_played_ms, source, raw_artist, raw_title, raw_album, raw_payload, recording_id, release_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			l.UserID, at.Format(TimeFormat), dur, source, l.Artist, l.Title, album, payload, recID, relID); err != nil {
			return 0, 0, err
		}
		accepted++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return accepted, duplicates, nil
}
