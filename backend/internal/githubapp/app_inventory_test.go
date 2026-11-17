package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestAppInventory(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		malformed bool
		suspended bool
	}{{name: "complete"}, {name: "malformed", malformed: true}, {name: "suspended", suspended: true}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "" {
					t.Error("missing authorization")
				}
				switch r.URL.Path {
				case "/app/installations":
					if tc.suspended {
						w.Write([]byte(`[{"id":1,"suspended_at":"2026-01-01T00:00:00Z"}]`))
						return
					}
					w.Write([]byte(`[{"id":1,"suspended_at":null}]`))
				case "/app/installations/1/access_tokens":
					if r.Method != http.MethodPost {
						t.Error("expected POST")
					}
					w.Write([]byte(`{"token":"secret"}`))
				case "/installation/repositories":
					if tc.malformed {
						w.Write([]byte(`{"total_count":2,"repositories":[{"id":4}]}`))
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "repositories": []map[string]int64{{"id": 4}, {"id": 2}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := New("client", "secret", server.URL, server.URL, server.URL)
			got, err := c.AppInventory(context.Background(), key)
			if tc.malformed || tc.suspended {
				if err == nil || got != nil {
					t.Fatalf("expected error, got %v %v", got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, map[int64][]int64{1: {2, 4}}) {
				t.Fatalf("inventory %v: %v", got, err)
			}
		})
	}
}
