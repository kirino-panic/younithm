// Package secrets implements encryption at rest for provider secrets: a master key (env, file, or auto-generated in the config dir), an
// AES-256-GCM box with a versioned, rotation-ready ciphertext format, a
// write-only encrypted store, and a log redactor.
//
// Threat model (docs/security.md): protects a leaked database or backup,
// not a compromised host (the key file lives next to the DB by default).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the master key length in bytes (AES-256).
const KeySize = 32

// Ciphertext format (text, safe for a SQLite TEXT column):
//
//	ys1:<base64url-raw( keyID[4] || nonce[12] || AES-256-GCM(plaintext) )>
//
// "ys1" is the format version. keyID is the first 4 bytes of SHA-256(key), so
// a Box holding several keys can pick the right one when decrypting; that is
// what makes rotation possible (add the new key, re-encrypt, drop the old).
// The additional authenticated data (aad) binds a ciphertext to its purpose
// (e.g. "provider_secret:spotify.client_secret"), so values cannot be swapped
// between rows.
const (
	prefixV1  = "ys1:"
	keyIDSize = 4
	nonceSize = 12
)

var (
	// ErrNotEncrypted is returned by Decrypt for a value without the prefix.
	ErrNotEncrypted = errors.New("secrets: value is not encrypted")
	// ErrUnknownKey means the ciphertext was sealed with a key this Box lacks.
	ErrUnknownKey = errors.New("secrets: ciphertext sealed with an unknown master key")
	// ErrDecrypt covers tampering, a wrong key or a wrong aad.
	ErrDecrypt = errors.New("secrets: decryption failed")
)

// IsEncrypted reports whether s looks like a ciphertext produced by Encrypt.
func IsEncrypted(s string) bool { return strings.HasPrefix(s, prefixV1) }

type keyEntry struct {
	id   [keyIDSize]byte
	aead cipher.AEAD
}

// Box encrypts with its primary key and decrypts with any key it holds.
type Box struct {
	primary keyEntry
	all     map[[keyIDSize]byte]keyEntry
}

func newEntry(key []byte) (keyEntry, error) {
	if len(key) != KeySize {
		return keyEntry{}, fmt.Errorf("secrets: master key must be %d bytes, got %d", KeySize, len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return keyEntry{}, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return keyEntry{}, err
	}
	var e keyEntry
	sum := sha256.Sum256(key)
	copy(e.id[:], sum[:keyIDSize])
	e.aead = aead
	return e, nil
}

// NewBox builds a Box. primary is used to encrypt; old keys (optional) are
// only used to decrypt, for rotation.
func NewBox(primary []byte, old ...[]byte) (*Box, error) {
	p, err := newEntry(primary)
	if err != nil {
		return nil, err
	}
	b := &Box{primary: p, all: map[[keyIDSize]byte]keyEntry{p.id: p}}
	for _, k := range old {
		e, err := newEntry(k)
		if err != nil {
			return nil, err
		}
		b.all[e.id] = e
	}
	return b, nil
}

// Encrypt seals plaintext under the primary key, bound to aad.
func (b *Box) Encrypt(plaintext, aad string) (string, error) {
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := make([]byte, 0, keyIDSize+nonceSize+len(plaintext)+b.primary.aead.Overhead())
	out = append(out, b.primary.id[:]...)
	out = append(out, nonce...)
	out = b.primary.aead.Seal(out, nonce, []byte(plaintext), []byte(aad))
	return prefixV1 + base64.RawURLEncoding.EncodeToString(out), nil
}

// Decrypt opens a value produced by Encrypt with the same aad.
func (b *Box) Decrypt(ciphertext, aad string) (string, error) {
	if !IsEncrypted(ciphertext) {
		return "", ErrNotEncrypted
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ciphertext, prefixV1))
	if err != nil || len(raw) < keyIDSize+nonceSize {
		return "", ErrDecrypt
	}
	var id [keyIDSize]byte
	copy(id[:], raw[:keyIDSize])
	e, ok := b.all[id]
	if !ok {
		return "", ErrUnknownKey
	}
	nonce := raw[keyIDSize : keyIDSize+nonceSize]
	pt, err := e.aead.Open(nil, nonce, raw[keyIDSize+nonceSize:], []byte(aad))
	if err != nil {
		return "", ErrDecrypt
	}
	return string(pt), nil
}

// NeedsRotation reports whether ciphertext was sealed with a non-primary key.
func (b *Box) NeedsRotation(ciphertext string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ciphertext, prefixV1))
	if err != nil || len(raw) < keyIDSize {
		return false
	}
	return string(raw[:keyIDSize]) != string(b.primary.id[:])
}
