// Package db opens the SQLite database and runs embedded migrations.
//
// Two pools are exposed (see Pools): a single-connection Write pool and a
// multi-connection Read pool. SQLite allows one writer at a time; funnelling
// every write through one connection serialises them in Go instead of
// surfacing "database is locked" errors, while WAL lets readers run
// concurrently with that writer.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // pure-Go driver, no CGO
)

//go:embed migrations/*.sql
var migrations embed.FS

const dbFile = "younithm.db"

func dsn(path string, readOnly bool) string {
	s := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	if readOnly {
		s += "&_pragma=query_only(1)"
	}
	return s
}

// Pools holds the split connection pools.
//
//	Read:  several connections, query_only. Use for SELECTs.
//	Write: exactly one connection. Use for every INSERT/UPDATE/DELETE and
//	       for any transaction that writes (BeginTx on Write).
//
// Read and Write are independent connections: a read on Read will not see
// uncommitted data from a transaction open on Write.
type Pools struct {
	Read  *sql.DB
	Write *sql.DB
	Path  string // path of the database file
}

// Close closes both pools.
func (p *Pools) Close() error {
	e1 := p.Read.Close()
	e2 := p.Write.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

// OpenPools opens (creating if needed) the database inside dir, in WAL mode,
// with split read and write pools.
func OpenPools(dir string) (*Pools, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	path := filepath.Join(dir, dbFile)

	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := w.Ping(); err != nil { // creates the file + enables WAL
		w.Close()
		return nil, err
	}

	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	n := min(max(runtime.NumCPU(), 2), 8)
	r.SetMaxOpenConns(n)
	r.SetMaxIdleConns(n)
	if err := r.Ping(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	return &Pools{Read: r, Write: w, Path: path}, nil
}

// Open is the legacy single-pool opener, kept for backward compatibility.
// The returned handle is a general read/write pool with no write
// serialisation. New code should use OpenPools.
//
// Deprecated: use OpenPools.
func Open(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	d, err := sql.Open("sqlite", dsn(filepath.Join(dir, dbFile), false))
	if err != nil {
		return nil, err
	}
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Migrate applies all pending migrations without taking a backup.
func Migrate(d *sql.DB) error {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(d, "migrations")
}

// hasPending reports whether any embedded migration is not yet applied.
func hasPending(d *sql.DB) (bool, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return false, err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, d, sub)
	if err != nil {
		return false, err
	}
	return p.HasPending(context.Background())
}

// MigrateWithBackup applies pending migrations. If any are pending and the
// database was already migrated by an earlier version, it first writes a
// consistent copy (VACUUM INTO) to <dir>/backups/younithm-pre-migrate-<ts>.db.
// It returns the backup path ("" if none was needed). Migration is aborted if
// the backup fails. The newest 5 pre-migrate backups are kept.
func MigrateWithBackup(p *Pools) (string, error) {
	pending, err := hasPending(p.Write)
	if err != nil {
		return "", fmt.Errorf("check pending migrations: %w", err)
	}
	if !pending {
		return "", nil
	}
	var backup string
	if ver, err := goose.GetDBVersion(p.Write); err == nil && ver > 0 {
		backup, err = Backup(p, "pre-migrate")
		if err != nil {
			return "", fmt.Errorf("backup before migrate: %w", err)
		}
		log.Printf("db: backed up to %s before migrating from v%d", backup, ver)
		pruneBackups(filepath.Dir(backup), "younithm-pre-migrate-", 5)
	}
	return backup, Migrate(p.Write)
}

// Backup writes a consistent snapshot of the database to
// <dir>/backups/younithm-<tag>-<timestamp>.db and returns its path.
func Backup(p *Pools, tag string) (string, error) {
	dir := filepath.Join(filepath.Dir(p.Path), "backups")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	out := filepath.Join(dir, fmt.Sprintf("younithm-%s-%s.db", tag, time.Now().UTC().Format("20060102T150405.000Z")))
	// VACUUM INTO takes a literal, not a bound parameter.
	if _, err := p.Write.Exec("VACUUM INTO '" + strings.ReplaceAll(out, "'", "''") + "'"); err != nil {
		return "", err
	}
	return out, nil
}

func pruneBackups(dir, prefix string, keep int) {
	m, _ := filepath.Glob(filepath.Join(dir, prefix+"*.db")) // sorted; names embed timestamps
	for i := 0; i < len(m)-keep; i++ {
		_ = os.Remove(m[i])
	}
}
