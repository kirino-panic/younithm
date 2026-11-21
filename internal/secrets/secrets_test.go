package secrets_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"younithm/internal/db"
	"younithm/internal/secrets"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, secrets.KeySize) }

func TestBoxRoundTripAndAAD(t *testing.T) {
	b, _ := secrets.NewBox(key(1))
	ct, err := b.Encrypt("hunter2-secret", "a")
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.IsEncrypted(ct) || strings.Contains(ct, "hunter2") {
		t.Fatalf("bad ciphertext %q", ct)
	}
	if pt, err := b.Decrypt(ct, "a"); err != nil || pt != "hunter2-secret" {
		t.Fatalf("got %q %v", pt, err)
	}
	if _, err := b.Decrypt(ct, "b"); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("wrong aad: %v", err)
	}
	ct2, _ := b.Encrypt("hunter2-secret", "a")
	if ct == ct2 {
		t.Fatal("nonce reuse")
	}
	// Tamper.
	raw := []byte(ct)
	raw[len(raw)-2] ^= 1
	if _, err := b.Decrypt(string(raw), "a"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := b.Decrypt("plain", "a"); !errors.Is(err, secrets.ErrNotEncrypted) {
		t.Fatal(err)
	}
	if _, err := secrets.NewBox([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestBoxRotation(t *testing.T) {
	oldB, _ := secrets.NewBox(key(1))
	ct, _ := oldB.Encrypt("v", "x")
	newB, _ := secrets.NewBox(key(2), key(1))
	if pt, err := newB.Decrypt(ct, "x"); err != nil || pt != "v" {
		t.Fatalf("old key via new box: %q %v", pt, err)
	}
	if !newB.NeedsRotation(ct) {
		t.Fatal("should need rotation")
	}
	if _, err := mustBox(t, key(2)).Decrypt(ct, "x"); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
	ct2, _ := newB.Encrypt("v", "x")
	if newB.NeedsRotation(ct2) {
		t.Fatal("fresh ciphertext flagged")
	}
}

func mustBox(t *testing.T, k []byte) *secrets.Box {
	t.Helper()
	b, err := secrets.NewBox(k)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMasterKeyResolution(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{}
	opt := secrets.KeyOptions{ConfigDir: dir, Getenv: func(k string) string { return env[k] }}

	// 1. Generated on first run, mode 0600, stable afterwards.
	k1, src, err := secrets.LoadMasterKey(opt)
	if err != nil || src != secrets.SourceGenerated || len(k1) != secrets.KeySize {
		t.Fatalf("%v %v %d", err, src, len(k1))
	}
	st, err := os.Stat(filepath.Join(dir, secrets.DefaultKeyFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", st, err)
	}
	k2, src, _ := secrets.LoadMasterKey(opt)
	if src != secrets.SourceExisting || !bytes.Equal(k1, k2) {
		t.Fatal("key not stable")
	}

	// 2. File override beats config dir.
	other := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(other, []byte(hex.EncodeToString(key(7))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env[secrets.EnvMasterKeyFile] = other
	k3, src, err := secrets.LoadMasterKey(opt)
	if err != nil || src != secrets.SourceFile || !bytes.Equal(k3, key(7)) {
		t.Fatalf("file override: %v %v", src, err)
	}

	// 3. Env beats file.
	env[secrets.EnvMasterKey] = base64.StdEncoding.EncodeToString(key(9))
	k4, src, err := secrets.LoadMasterKey(opt)
	if err != nil || src != secrets.SourceEnv || !bytes.Equal(k4, key(9)) {
		t.Fatalf("env override: %v %v", src, err)
	}

	// 4. Malformed explicit config is an error, not a silent new key.
	env[secrets.EnvMasterKey] = "not-a-key"
	if _, _, err := secrets.LoadMasterKey(opt); err == nil {
		t.Fatal("malformed env key accepted")
	}
	delete(env, secrets.EnvMasterKey)
	env[secrets.EnvMasterKeyFile] = filepath.Join(dir, "missing")
	if _, _, err := secrets.LoadMasterKey(opt); err == nil {
		t.Fatal("missing key file fell back silently")
	}

	// 5. Corrupt key in the config dir must not be overwritten.
	delete(env, secrets.EnvMasterKeyFile)
	p := filepath.Join(dir, secrets.DefaultKeyFile)
	_ = os.WriteFile(p, []byte("garbage"), 0o600)
	if _, _, err := secrets.LoadMasterKey(opt); err == nil {
		t.Fatal("corrupt key replaced")
	}
	if b, _ := os.ReadFile(p); string(b) != "garbage" {
		t.Fatal("corrupt key file overwritten")
	}
}

func TestRedactor(t *testing.T) {
	r := secrets.NewRedactor("sup3rs3cretvalue", "abc") // "abc" too short, ignored
	if got := r.Redact("token=sup3rs3cretvalue abc"); got != "token=[redacted] abc" {
		t.Fatal(got)
	}
	var buf bytes.Buffer
	l := log.New(r.Writer(&buf), "", 0)
	l.Printf("connecting with %s", "sup3rs3cretvalue")
	if strings.Contains(buf.String(), "sup3r") {
		t.Fatalf("leaked: %q", buf.String())
	}
	var nilR *secrets.Redactor
	if nilR.Redact("x") != "x" {
		t.Fatal("nil redactor")
	}
}

func TestHint(t *testing.T) {
	for in, want := range map[string]string{
		"0123456789abcdef3f2a": "3f2a",
		"short":                "",
		"-----BEGIN PRIVATE KEY-----\nMIGTAgEAMBMGByqGSM49\nabcdWXYZ\n-----END PRIVATE KEY-----": "WXYZ",
	} {
		if got := secrets.Hint(in); got != want {
			t.Errorf("Hint(%q)=%q want %q", in, got, want)
		}
	}
}

func newStore(t *testing.T, box *secrets.Box, red *secrets.Redactor) (*secrets.Store, *db.Pools) {
	t.Helper()
	p, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	if err := db.Migrate(p.Write); err != nil {
		t.Fatal(err)
	}
	return secrets.NewStore(p.Read, p.Write, box, red), p
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	red := secrets.NewRedactor()
	box := mustBox(t, key(3))
	s, p := newStore(t, box, red)

	if m, _ := s.Meta(ctx, "spotify.client_secret"); m.Set {
		t.Fatal("should start unset")
	}
	if _, err := s.Get(ctx, "spotify.client_secret"); !errors.Is(err, secrets.ErrNotSet) {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "Bad Name", "x"); !errors.Is(err, secrets.ErrBadName) {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "spotify.client_secret", "client-secret-value-a3f2"); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Meta(ctx, "spotify.client_secret")
	if !m.Set || m.Hint != "a3f2" || m.UpdatedAt.IsZero() {
		t.Fatalf("%+v", m)
	}
	if ok, _ := s.Has(ctx, "spotify.client_secret"); !ok {
		t.Fatal("Has")
	}
	if v, err := s.Get(ctx, "spotify.client_secret"); err != nil || v != "client-secret-value-a3f2" {
		t.Fatalf("%q %v", v, err)
	}
	// Plaintext must not be in the DB.
	var ct string
	_ = p.Read.QueryRow(`SELECT ciphertext FROM provider_secret`).Scan(&ct)
	if strings.Contains(ct, "client-secret") || !secrets.IsEncrypted(ct) {
		t.Fatalf("stored %q", ct)
	}
	if got := red.Redact("x client-secret-value-a3f2"); strings.Contains(got, "client-secret") {
		t.Fatal("store did not register value with redactor")
	}
	// A ciphertext moved to another row must not decrypt (AAD).
	_, _ = p.Write.Exec(`INSERT INTO provider_secret (name, ciphertext) VALUES ('apple.key_id', ?)`, ct)
	if _, err := s.Get(ctx, "apple.key_id"); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("row swap accepted: %v", err)
	}
	// Overwrite then delete.
	_ = s.Set(ctx, "spotify.client_secret", "another-secret-b7c1")
	if m, _ := s.Meta(ctx, "spotify.client_secret"); m.Hint != "b7c1" {
		t.Fatal(m.Hint)
	}
	if err := s.Delete(ctx, "spotify.client_secret"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has(ctx, "spotify.client_secret"); ok {
		t.Fatal("not deleted")
	}
}

func TestStoreRotateAndLoadRedactor(t *testing.T) {
	ctx := context.Background()
	oldBox := mustBox(t, key(1))
	s, p := newStore(t, oldBox, nil)
	_ = s.Set(ctx, "spotify.client_secret", "rotate-me-secret-1234")

	newBox, _ := secrets.NewBox(key(2), key(1))
	red := secrets.NewRedactor()
	s2 := secrets.NewStore(p.Read, p.Write, newBox, red)
	if l, f, err := s2.LoadIntoRedactor(ctx); err != nil || l != 1 || f != 0 {
		t.Fatalf("%d %d %v", l, f, err)
	}
	if strings.Contains(red.Redact("rotate-me-secret-1234"), "rotate") {
		t.Fatal("not registered")
	}
	if n, err := s2.Rotate(ctx); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := s2.Rotate(ctx); n != 0 {
		t.Fatal("second rotate should be a no-op")
	}
	only := secrets.NewStore(p.Read, p.Write, mustBox(t, key(2)), nil)
	if v, err := only.Get(ctx, "spotify.client_secret"); err != nil || v != "rotate-me-secret-1234" {
		t.Fatalf("%q %v", v, err)
	}
}
