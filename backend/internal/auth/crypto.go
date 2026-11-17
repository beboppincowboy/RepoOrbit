package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// seal encrypts a secret with AES-256-GCM; the random nonce is prepended to the ciphertext.
func seal(key []byte, plaintext string) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func open(key, sealed []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	return string(plain), err
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sha256Digest is the stored form of a session token or OAuth state. Not for passwords: the inputs are 256-bit random.
func sha256Digest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// csrfToken is derived from the secret session token, so only the session's holder can compute it.
func csrfToken(key []byte, sessionToken string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("csrf:" + sessionToken))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
