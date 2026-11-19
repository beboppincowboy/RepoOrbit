package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInstallationCredentialsReuseAndSeparateCooldowns(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.Header.Get("Authorization") == "" {
			t.Error("expected authenticated token exchange")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": r.URL.Path, "expires_at": time.Now().Add(time.Hour),
		})
	}))
	defer server.Close()
	credentials := NewInstallationCredentials(New("client", "", "", server.URL, ""), key)
	ctx := context.Background()
	first, err := credentials.AccessToken(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := credentials.AccessToken(ctx, 1)
	if err != nil || first != second || requests != 1 {
		t.Fatalf("expected one exchange, got %d (err: %v)", requests, err)
	}
	credentials.RecordAPIError(1, first, &APIError{Status: 429, RetryAt: time.Now().Add(time.Minute)})
	_, err = credentials.AccessToken(ctx, 1)
	var api *APIError
	if !errors.As(err, &api) || !api.RateLimited() || requests != 1 {
		t.Fatalf("expected cooldown without exchange, got %v, %d exchanges", err, requests)
	}
	if _, err := credentials.AccessToken(ctx, 2); err != nil || requests != 2 {
		t.Fatalf("expected a separate exchange for installation 2, got %v, %d exchanges", err, requests)
	}
}
