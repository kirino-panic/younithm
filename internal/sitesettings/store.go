// Package sitesettings holds admin-owned site settings: per-section public
// visibility.
// Reads are cheap single-row lookups; writes come only from admin endpoints.
package sitesettings

import (
	"context"
	"database/sql"
	"errors"
)

// Section is a part of the public surface the admin can hide.
type Section struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Sections lists every toggle in display order. All default to public.
var Sections = []Section{
	{"scrobbles", "Scrobble list"},
	{"now_playing", "Now playing"},
	{"recent", "Recent listens"},
	{"top", "Top artists and tracks"},
	{"stats", "Stats"},
	{"library", "Artist, album and track pages"},
}

// Known reports whether key is a valid section key.
func Known(key string) bool {
	for _, s := range Sections {
		if s.Key == key {
			return true
		}
	}
	return false
}

// Store reads and writes the admin_setting table.
type Store struct {
	read, write *sql.DB
	debug       bool
}

// SetDebug switches the debug tools on or off (see config.DebugTools). Off,
// their endpoints are not mounted and their settings have no effect.
func (s *Store) SetDebug(on bool) { s.debug = on }

// DebugEnabled reports whether the debug tools are on.
func (s *Store) DebugEnabled() bool { return s.debug }

func NewStore(read, write *sql.DB) *Store { return &Store{read: read, write: write} }

func (s *Store) get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM admin_setting WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) put(ctx context.Context, key, value string) error {
	_, err := s.write.ExecContext(ctx, `INSERT INTO admin_setting (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value,
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`, key, value)
	return err
}

// Public reports whether section is visible to anonymous visitors (default true).
func (s *Store) Public(ctx context.Context, section string) (bool, error) {
	v, ok, err := s.get(ctx, "public."+section)
	if err != nil || !ok {
		return true, err
	}
	return v != "0", nil
}

// Visibility returns every section's public flag.
func (s *Store) Visibility(ctx context.Context) (map[string]bool, error) {
	out := make(map[string]bool, len(Sections))
	for _, sec := range Sections {
		p, err := s.Public(ctx, sec.Key)
		if err != nil {
			return nil, err
		}
		out[sec.Key] = p
	}
	return out, nil
}

// SetPublic sets one section's visibility.
func (s *Store) SetPublic(ctx context.Context, section string, public bool) error {
	v := "0"
	if public {
		v = "1"
	}
	return s.put(ctx, "public."+section, v)
}
