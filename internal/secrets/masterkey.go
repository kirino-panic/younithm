package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// EnvMasterKey holds the key itself (base64 or hex of 32 bytes).
	EnvMasterKey = "YOUNITHM_MASTER_KEY"
	// EnvMasterKeyFile points at a file holding the key (same encodings).
	EnvMasterKeyFile = "YOUNITHM_MASTER_KEY_FILE"
	// DefaultKeyFile is the auto-generated key, inside the config dir.
	DefaultKeyFile = "master.key"
)

// KeySource says where the master key came from (for the startup log; never
// log the key itself).
type KeySource string

const (
	SourceEnv       KeySource = "env"
	SourceFile      KeySource = "file"
	SourceGenerated KeySource = "generated"
	SourceExisting  KeySource = "config-dir"
)

// KeyOptions controls key resolution. Getenv defaults to os.Getenv.
type KeyOptions struct {
	ConfigDir string
	Getenv    func(string) string
}

// LoadMasterKey resolves the master key in order: YOUNITHM_MASTER_KEY,
// YOUNITHM_MASTER_KEY_FILE, <ConfigDir>/master.key. If none exists the last
// one is generated (random, mode 0600) and written. An explicitly configured
// source that is unreadable or malformed is an error, never a silent fallback
// to a fresh key (that would orphan existing ciphertext).
func LoadMasterKey(o KeyOptions) ([]byte, KeySource, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := strings.TrimSpace(getenv(EnvMasterKey)); v != "" {
		k, err := decodeKey(v)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", EnvMasterKey, err)
		}
		return k, SourceEnv, nil
	}
	if p := strings.TrimSpace(getenv(EnvMasterKeyFile)); p != "" {
		k, err := readKeyFile(p)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", EnvMasterKeyFile, err)
		}
		return k, SourceFile, nil
	}
	if o.ConfigDir == "" {
		return nil, "", errors.New("secrets: no config dir for master key")
	}
	path := filepath.Join(o.ConfigDir, DefaultKeyFile)
	k, err := readKeyFile(path)
	if err == nil {
		return k, SourceExisting, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	k = make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(o.ConfigDir, 0o700); err != nil {
		return nil, "", err
	}
	// O_EXCL: never overwrite a key another process just created.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			k2, err2 := readKeyFile(path)
			if err2 != nil {
				return nil, "", fmt.Errorf("%s: %w", path, err2)
			}
			return k2, SourceExisting, nil
		}
		return nil, "", err
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(k) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return nil, "", werr
	}
	return k, SourceGenerated, nil
}

func readKeyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeKey(strings.TrimSpace(string(b)))
}

func decodeKey(s string) ([]byte, error) {
	if len(s) == 2*KeySize {
		if k, err := hex.DecodeString(s); err == nil {
			return k, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil && len(k) == KeySize {
			return k, nil
		}
	}
	return nil, fmt.Errorf("key must be %d bytes encoded as base64 or hex", KeySize)
}

// OpenBox resolves the master key and returns a Box.
func OpenBox(o KeyOptions) (*Box, KeySource, error) {
	k, src, err := LoadMasterKey(o)
	if err != nil {
		return nil, "", err
	}
	b, err := NewBox(k)
	return b, src, err
}
