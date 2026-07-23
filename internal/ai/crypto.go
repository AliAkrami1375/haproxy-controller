// Package ai integrates an OpenRouter-backed assistant that builds HAProxy
// configuration through a staged, self-validating agent.
package ai

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// secretPrefix marks a value produced by Encrypt so Decrypt can tell an
// encrypted secret from a value that predates encryption or was set by hand.
const secretPrefix = "enc:v1:"

// Encrypt seals a secret (the OpenRouter API key) with AES-256-GCM using a key
// derived from the controller's session secret. The result is safe to store in
// the settings table and is tagged so it can be recognised later.
func Encrypt(plaintext, passphrase string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(deriveKey(passphrase))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return secretPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. A value without the marker is returned unchanged,
// so a key placed directly in the config keeps working.
func Decrypt(stored, passphrase string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, secretPrefix) {
		return stored, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, secretPrefix))
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	block, err := aes.NewCipher(deriveKey(passphrase))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("stored secret is corrupt")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", errors.New("stored secret could not be decrypted (session secret changed?)")
	}
	return string(plain), nil
}

// IsEncrypted reports whether a stored value is one Encrypt produced.
func IsEncrypted(v string) bool { return strings.HasPrefix(v, secretPrefix) }

func deriveKey(passphrase string) []byte {
	sum := sha256.Sum256([]byte("haproxy-controller/ai-secret\x00" + passphrase))
	return sum[:]
}

// MaskKey returns a display-safe hint for an API key: the last four characters
// only. It never reveals the secret.
func MaskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "••••"
	}
	return "••••••••" + key[len(key)-4:]
}
