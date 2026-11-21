// Package snapshot caches raw provider responses in the provider_snapshot
// table so canonical fields can be re-derived without
// refetching, and so repeated misses do not hammer rate-limited providers.
package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Kind values for provider_snapshot.kind.
const (
	KindRecording = "recording"
	KindRelease   = "release"
	KindArtist    = "artist"
)

// Default TTLs. Positive MusicBrainz data changes slowly; misses may become
// hits when someone adds the data upstream, so they expire sooner.
const (
	DefaultTTL         = 30 * 24 * time.Hour
	DefaultNegativeTTL = 3 * 24 * time.Hour
)

const tsLayout = "2006-01-02T15:04:05.000Z"

// Entry is a cached provider response.
type Entry struct {
	Provider   string
	ProviderID string
	Kind       string
	FetchedAt  time.Time
	ExpiresAt  time.Time // zero = never
	NotFound   bool
	Raw        []byte
	FetchCount int
}

// Fresh reports whether the entry may be used without refetching at now.
func (e *Entry) Fresh(now time.Time) bool {
	return e.ExpiresAt.IsZero() || now.Before(e.ExpiresAt)
}

type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

func New(db *sql.DB) *Store { return &Store{DB: db, Now: time.Now} }

// Get returns the cached entry (fresh or stale) or (nil, nil) when absent.
// Callers check Entry.Fresh to decide whether to refetch; a stale entry is
// still useful as a fallback when the provider is unreachable.
func (s *Store) Get(ctx context.Context, provider, id, kind string) (*Entry, error) {
	var (
		e       Entry
		fetched string
		expires sql.NullString
		status  string
		raw     string
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT fetched_at, expires_at, status, raw_json, fetch_count
		   FROM provider_snapshot WHERE provider=? AND provider_id=? AND kind=?`,
		provider, id, kind).Scan(&fetched, &expires, &status, &raw, &e.FetchCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.Provider, e.ProviderID, e.Kind = provider, id, kind
	e.FetchedAt, _ = time.Parse(tsLayout, fetched)
	if expires.Valid {
		e.ExpiresAt, _ = time.Parse(tsLayout, expires.String)
	}
	e.NotFound = status == "not_found"
	e.Raw = []byte(raw)
	return &e, nil
}

// Put stores a positive response. ttl <= 0 means DefaultTTL.
func (s *Store) Put(ctx context.Context, provider, id, kind string, raw []byte, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return s.put(ctx, provider, id, kind, "ok", string(raw), ttl)
}

// PutNotFound stores a negative cache entry. ttl <= 0 means DefaultNegativeTTL.
func (s *Store) PutNotFound(ctx context.Context, provider, id, kind string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = DefaultNegativeTTL
	}
	return s.put(ctx, provider, id, kind, "not_found", "{}", ttl)
}

func (s *Store) put(ctx context.Context, provider, id, kind, status, raw string, ttl time.Duration) error {
	now := s.Now().UTC()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO provider_snapshot (provider, provider_id, kind, fetched_at, raw_json, status, expires_at, fetch_count)
		 VALUES (?,?,?,?,?,?,?,1)
		 ON CONFLICT(provider, provider_id, kind) DO UPDATE SET
		   fetched_at=excluded.fetched_at, raw_json=excluded.raw_json, status=excluded.status,
		   expires_at=excluded.expires_at, fetch_count=provider_snapshot.fetch_count+1`,
		provider, id, kind, now.Format(tsLayout), raw, status, now.Add(ttl).Format(tsLayout))
	return err
}

// PurgeExpired deletes entries expired before now minus grace; returns count.
func (s *Store) PurgeExpired(ctx context.Context, grace time.Duration) (int64, error) {
	cutoff := s.Now().UTC().Add(-grace).Format(tsLayout)
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM provider_snapshot WHERE expires_at IS NOT NULL AND expires_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
