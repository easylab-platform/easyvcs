package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
)

// secretKey returns the key used to encrypt secret columns (remotes.token,
// repositories.mirror_token). It comes from EASYVCS_SECRET_KEY; when empty the
// secrets are stored in plaintext (backward compatible). The key must be 16,
// 24, or 32 bytes for AES.
func secretKey() []byte {
	k := os.Getenv("EASYVCS_SECRET_KEY")
	if k == "" {
		return nil
	}
	return []byte(k)
}

// encryptSecret encrypts plaintext secret with AES-GCM. If no key is configured
// it returns plaintext (so behavior is unchanged unless EASYVCS_SECRET_KEY is
// set). The result is base64 of (nonce||ciphertext), prefixed with "enc:" so we
// can distinguish encrypted values from legacy plaintext on read.
func encryptSecret(s string) (string, error) {
	key := secretKey()
	if key == nil {
		return s, nil
	}
	block, err := aes.NewCipher(normalizeKey(key))
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
	out := gcm.Seal(nonce, nonce, []byte(s), nil)
	return "enc:" + base64.StdEncoding.EncodeToString(out), nil
}

// decryptSecret reverses encryptSecret. Values without the "enc:" prefix are
// treated as legacy plaintext and returned as-is (backward compatible). If no
// key is configured, encrypted values cannot be read and an error is returned
// only if the value carried an "enc:" prefix.
func decryptSecret(s string) (string, error) {
	key := secretKey()
	if len(s) >= 4 && s[:4] == "enc:" {
		if key == nil {
			return "", fmt.Errorf("secret is encrypted but EASYVCS_SECRET_KEY is not set")
		}
		raw, err := base64.StdEncoding.DecodeString(s[4:])
		if err != nil {
			return "", err
		}
		block, err := aes.NewCipher(normalizeKey(key))
		if err != nil {
			return "", err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return "", err
		}
		ns := gcm.NonceSize()
		if len(raw) < ns {
			return "", fmt.Errorf("encrypted secret too short")
		}
		pt, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
		if err != nil {
			return "", err
		}
		return string(pt), nil
	}
	return s, nil
}

// normalizeKey pads/truncates a key to a valid AES size (16/24/32). This keeps
// EASYVCS_SECRET_KEY ergonomic (any non-empty string works). For the full
// security property use an exact 32-byte key.
func normalizeKey(key []byte) []byte {
	switch len(key) {
	case 16, 24, 32:
		return key
	case 0:
		return make([]byte, 32)
	}
	if len(key) < 32 {
		out := make([]byte, 32)
		copy(out, key)
		return out
	}
	return key[:32]
}
