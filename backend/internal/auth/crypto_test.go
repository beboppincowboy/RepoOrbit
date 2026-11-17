package auth

import (
	"bytes"
	"testing"
)

func TestSealRoundTripAndTamper(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	sealed, err := seal(key, "ghu_secret")
	if err != nil || bytes.Contains(sealed, []byte("ghu_secret")) {
		t.Fatalf("seal: %v", err)
	}
	if got, err := open(key, sealed); err != nil || got != "ghu_secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := open(key, sealed); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := open(bytes.Repeat([]byte{8}, 32), sealed); err == nil {
		t.Fatal("wrong key accepted")
	}
}
