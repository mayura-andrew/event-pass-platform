// Package secrets encrypts integration credentials (Google refresh tokens)
// at rest with AES-256-GCM. A sealed value is version(1) | nonce | ciphertext,
// so the key can be rotated later by adding a version.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

const version = 1

type Box struct{ aead cipher.AEAD }

// New uses a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("the integrations key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// FromBase64 reads INTEGRATIONS_KEY (openssl rand -base64 32).
func FromBase64(s string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("INTEGRATIONS_KEY must be base64 (openssl rand -base64 32)")
	}
	return New(key)
}

// Derived makes a box from another secret, for local development only.
func Derived(secret []byte) *Box {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("nsac integrations key v1"))
	b, _ := New(mac.Sum(nil))
	return b
}

// Seal encrypts plaintext; associated data (e.g. the integration ID) binds
// the ciphertext to its row, so it can't be copied to another.
func (b *Box) Seal(plaintext, associated []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{version}, nonce...)
	return b.aead.Seal(out, nonce, plaintext, associated), nil
}

func (b *Box) Open(sealed, associated []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < 1+n || sealed[0] != version {
		return nil, errors.New("not a sealed secret")
	}
	return b.aead.Open(nil, sealed[1:1+n], sealed[1+n:], associated)
}
