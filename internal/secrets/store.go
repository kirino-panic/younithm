package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrNotSet is returned by Get when a secret has no value.
var ErrNotSet = errors.New("secrets: not set")

// ErrBadName is returned for a malformed secret name.
var ErrBadName = errors.New("secrets: invalid secret name")

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.]{0,63}$`)

// Meta is everything the HTTP API may reveal about a secret.
type Meta struct {
	Name      string
	Set       bool
	Hint      string // last 4 chars, may be empty
	UpdatedAt time.Time
}

// Store is the encrypted provider-secret store. Set/Has/Meta/Delete serve the
// admin API. Get exists for in-process provider code only and must never be
// wired to an HTTP response.
type Store struct {
	read, write *sql.DB
	box         *Box
	red         *Redactor
	now         func() time.Time
}

// NewStore builds a Store. red may be nil. Values written (or later read) are
// registered with it so they never reach the logs.
func NewStore(read, write *sql.DB, box *Box, red *Redactor) *Store {
	return &Store{read: read, write: write, box: box, red: red, now: time.Now}
}

func aadFor(name string) string { return "provider_secret:" + name }

const tsFmt = "2006-01-02T15:04:05.000Z"

// Set encrypts and stores value, replacing any previous one.
func (s *Store) Set(ctx context.Context, name, value string) error {
	if !nameRE.MatchString(name) {
		return ErrBadName
	}
	if value == "" {
		return errors.New("secrets: empty value")
	}
	ct, err := s.box.Encrypt(value, aadFor(name))
	if err != nil {
		return err
	}
	s.red.Add(value)
	now := s.now().UTC().Format(tsFmt)
	_, err = s.write.ExecContext(ctx,
		`INSERT INTO provider_secret (name, ciphertext, hint, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET ciphertext = excluded.ciphertext, hint = excluded.hint, updated_at = excluded.updated_at`,
		name, ct, Hint(value), now, now)
	return err
}

// Has reports whether a secret is set.
func (s *Store) Has(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.read.QueryRowContext(ctx, `SELECT 1 FROM provider_secret WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Meta returns display metadata; Set is false when absent.
func (s *Store) Meta(ctx context.Context, name string) (Meta, error) {
	m := Meta{Name: name}
	var upd string
	err := s.read.QueryRowContext(ctx, `SELECT hint, updated_at FROM provider_secret WHERE name = ?`, name).Scan(&m.Hint, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	m.Set = true
	m.UpdatedAt, _ = time.Parse(tsFmt, upd)
	return m, nil
}

// Delete removes a secret; deleting an absent one is not an error.
func (s *Store) Delete(ctx context.Context, name string) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM provider_secret WHERE name = ?`, name)
	return err
}

// Get decrypts a secret for in-process use (provider clients). Never expose
// the result over HTTP or log it.
func (s *Store) Get(ctx context.Context, name string) (string, error) {
	var ct string
	err := s.read.QueryRowContext(ctx, `SELECT ciphertext FROM provider_secret WHERE name = ?`, name).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotSet
	}
	if err != nil {
		return "", err
	}
	v, err := s.box.Decrypt(ct, aadFor(name))
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", name, err)
	}
	s.red.Add(v)
	return v, nil
}

// LoadIntoRedactor decrypts every stored secret and registers it with the
// redactor, so values set in earlier runs are scrubbed from logs too.
// Undecryptable rows are skipped and counted.
func (s *Store) LoadIntoRedactor(ctx context.Context) (loaded, failed int, err error) {
	rows, err := s.read.QueryContext(ctx, `SELECT name, ciphertext FROM provider_secret`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, ct string
		if err := rows.Scan(&name, &ct); err != nil {
			return loaded, failed, err
		}
		v, derr := s.box.Decrypt(ct, aadFor(name))
		if derr != nil {
			failed++
			continue
		}
		s.red.Add(v)
		loaded++
	}
	return loaded, failed, rows.Err()
}

// Rotate re-encrypts every row sealed with a non-primary key. Used after
// adding a new primary key to the Box. Returns the number rewritten.
func (s *Store) Rotate(ctx context.Context) (int, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT name, ciphertext FROM provider_secret`)
	if err != nil {
		return 0, err
	}
	type row struct{ name, ct string }
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.ct); err != nil {
			rows.Close()
			return 0, err
		}
		if s.box.NeedsRotation(r.ct) {
			todo = append(todo, r)
		}
	}
	rows.Close()
	n := 0
	for _, r := range todo {
		v, err := s.box.Decrypt(r.ct, aadFor(r.name))
		if err != nil {
			return n, fmt.Errorf("rotate %q: %w", r.name, err)
		}
		ct, err := s.box.Encrypt(v, aadFor(r.name))
		if err != nil {
			return n, err
		}
		if _, err := s.write.ExecContext(ctx, `UPDATE provider_secret SET ciphertext = ? WHERE name = ?`, ct, r.name); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
