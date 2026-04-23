// Package cryptobox is an AES-256-GCM envelope used to encrypt sensitive
// strings (OAuth refresh/access tokens) at rest. A Box is safe for concurrent
// use.
package cryptobox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Box seals and opens byte slices with AES-256-GCM. The nonce is prepended to
// each ciphertext.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a raw 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("cryptobox: key must be 32 bytes (got %d)", len(key))
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

// NewFromHex builds a Box from a hex-encoded 32-byte key (64 hex chars).
func NewFromHex(keyHex string) (*Box, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("cryptobox: invalid hex key: %w", err)
	}
	return New(key)
}

// Seal encrypts plain. An empty input returns a nil ciphertext (so callers can
// represent "no value" without a ciphertext round-trip).
func (b *Box) Seal(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Open decrypts ct. An empty input returns the empty string.
func (b *Box) Open(ct []byte) (string, error) {
	if len(ct) == 0 {
		return "", nil
	}
	ns := b.aead.NonceSize()
	if len(ct) < ns {
		return "", errors.New("cryptobox: ciphertext too short")
	}
	nonce, body := ct[:ns], ct[ns:]
	plain, err := b.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
