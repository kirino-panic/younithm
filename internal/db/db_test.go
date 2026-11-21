package db

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPoolsConcurrentWritesAndBackup(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if b, err := MigrateWithBackup(p); err != nil || b != "" {
		t.Fatalf("fresh migrate: backup=%q err=%v", b, err)
	}
	if _, err := p.Write.Exec(`CREATE TABLE t_conc (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Write.Exec(`INSERT INTO t_conc VALUES (1)`); err != nil {
				t.Error(err)
			}
			var n int
			if err := p.Read.QueryRow(`SELECT count(*) FROM t_conc`).Scan(&n); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := p.Read.Exec(`INSERT INTO t_conc VALUES (2)`); err == nil {
		t.Fatal("read pool accepted a write")
	}
	out, err := Backup(p, "test")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatalf("backup missing: %v", err)
	}
	if filepath.Dir(out) != filepath.Join(dir, "backups") {
		t.Fatal("unexpected backup dir", out)
	}
	// Up to date: no pre-migrate backup.
	if b, err := MigrateWithBackup(p); err != nil || b != "" {
		t.Fatalf("expected no backup, got %q %v", b, err)
	}
}
