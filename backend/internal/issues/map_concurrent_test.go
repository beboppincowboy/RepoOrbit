package issues

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

func TestMapConcurrentKeepsOrder(t *testing.T) {
	got, err := mapConcurrent(context.Background(), []int{3, 1, 2}, func(_ context.Context, n int) (int, error) { return n * 10, nil })
	if err != nil || !slices.Equal(got, []int{30, 10, 20}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// Siblings cancelled by the first failure return context.Canceled; the real cause must win over them.
func TestMapConcurrentRealErrorBeatsSiblingCancellation(t *testing.T) {
	upstream := errors.New("upstream 502")
	_, err := mapConcurrent(context.Background(), []int{0, 1, 2, 3}, func(ctx context.Context, n int) (int, error) {
		if n == 2 {
			return 0, upstream
		}
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, upstream) {
		t.Fatalf("got %v, want the upstream error", err)
	}
}

// An Unauthorized error revokes the credential, so it wins over any other failure regardless of position.
func TestMapConcurrentUnauthorizedWins(t *testing.T) {
	ready := make(chan struct{})
	_, err := mapConcurrent(context.Background(), []int{0, 1}, func(ctx context.Context, n int) (int, error) {
		if n == 0 {
			close(ready)
			return 0, errors.New("upstream 502")
		}
		<-ready
		return 0, &githubapp.APIError{Status: http.StatusUnauthorized}
	})
	var api *githubapp.APIError
	if !errors.As(err, &api) || !api.Unauthorized() {
		t.Fatalf("got %v, want the Unauthorized error", err)
	}
}
