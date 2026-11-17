package githubapp_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
)

func TestAppInventoryFakePaginationAndRateLimit(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New("client", "secret")
	f.AppPublicKey = &key.PublicKey
	var ids []int64
	for i := 0; i < 101; i++ {
		ids = append(ids, f.AddRepo("owner", "repo", true))
	}
	f.Install("owner", 1, ids...)
	for i := int64(2); i <= 101; i++ {
		f.Install("other", i)
	}
	server := httptest.NewServer(f.Handler())
	defer server.Close()
	c := githubapp.New("client", "secret", server.URL, server.URL, server.URL)
	inventory, err := c.AppInventory(context.Background(), key)
	if err != nil || len(inventory) != 101 || len(inventory[1]) != 101 {
		t.Fatalf("incomplete inventory: %d installations, %d repos: %v", len(inventory), len(inventory[1]), err)
	}
	f.LimitUntil(time.Now().Add(time.Minute))
	inventory, err = c.AppInventory(context.Background(), key)
	var apiErr *githubapp.APIError
	if inventory != nil || !errors.As(err, &apiErr) || !apiErr.RateLimited() {
		t.Fatalf("expected rate limit, got %v %v", inventory, err)
	}
}
