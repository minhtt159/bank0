package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
)

// TOTP-seed encryption at rest (AES-256-GCM, key = auth.mfa_enc_key). The DB
// stores nonce||ciphertext; the plaintext seed exists only transiently in Go.
// An unset/malformed key disables the MFA endpoints (503) — bank0 never stores
// a plaintext seed.

func (s *Server) mfaAEAD() (cipher.AEAD, error) {
	if s.cfg.Auth.MFAEncKey == "" {
		return nil, errors.New("auth.mfa_enc_key is not set")
	}
	key, err := base64.StdEncoding.DecodeString(s.cfg.Auth.MFAEncKey)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("auth.mfa_enc_key must be base64 of exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// mfaConfigured is the handlers' 503 gate.
func (s *Server) mfaConfigured() bool {
	_, err := s.mfaAEAD()
	return err == nil
}

func (s *Server) encryptSeed(plain []byte) ([]byte, error) {
	aead, err := s.mfaAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, aead.Seal(nil, nonce, plain, nil)...), nil
}

func (s *Server) decryptSeed(blob []byte) ([]byte, error) {
	aead, err := s.mfaAEAD()
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], nil)
}

// newRecoveryCodes returns n one-time codes from an unambiguous alphabet
// (base32, no padding → A-Z2-7). Plaintext goes to the client exactly once;
// only sha256 hashes are stored.
func newRecoveryCodes(n int) []string {
	codes := make([]string, 0, n)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	for i := 0; i < n; i++ {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		codes = append(codes, enc.EncodeToString(b)[:10])
	}
	return codes
}
