package repositories

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

type cleanupTokens struct{ err error }

func (t cleanupTokens) AccessToken(context.Context, int64) (string, error) { return "token", t.err }
func (cleanupTokens) Revoke(context.Context, int64) error                  { return nil }

func TestPrivatePresenceRequiresAuthoritativeListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/repos":
			_, _ = w.Write([]byte(`[{"id":1,"full_name":"a/b","private":true}]`))
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	service := &Service{GitHub: githubapp.New("", "", "", server.URL, ""), TokenSource: cleanupTokens{}}
	if d := service.CheckPrivatePresence(context.Background(), 1, 1); !d.Unverified {
		t.Fatalf("owned listing alone cannot prove private installation access: %+v", d)
	}
	if d := service.CheckPrivatePresence(context.Background(), 1, 2); !d.Unverified {
		t.Fatalf("incomplete listing cannot prove absence: %+v", d)
	}
	service.TokenSource = cleanupTokens{err: auth.ErrUnauthenticated}
	if d := service.CheckPrivatePresence(context.Background(), 1, 2); !d.Unverified {
		t.Fatalf("missing credential cannot prove absence: %+v", d)
	}
}

func TestPrivatePresenceRejectsNullLists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/repos":
			_, _ = w.Write([]byte(`null`))
		case "/user/installations":
			_, _ = w.Write([]byte(`{"installations":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	service := &Service{GitHub: githubapp.New("", "", "", server.URL, ""), TokenSource: cleanupTokens{}}
	if d := service.CheckPrivatePresence(context.Background(), 1, 2); !d.Unverified {
		t.Fatalf("null listing cannot prove absence: %+v", d)
	}
}
