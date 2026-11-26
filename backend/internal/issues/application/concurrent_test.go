package application

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

func TestMapConcurrentKeepsOrder(t *testing.T) {
	got, err := MapConcurrent(context.Background(), []int{3, 1, 2}, func(_ context.Context, n int) (int, error) { return n * 10, nil })
	if err != nil || !slices.Equal(got, []int{30, 10, 20}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// Siblings cancelled by the first failure return context.Canceled; the real cause must win over them.
func TestMapConcurrentRealErrorBeatsSiblingCancellation(t *testing.T) {
	upstream := errors.New("upstream 502")
	_, err := MapConcurrent(context.Background(), []int{0, 1, 2, 3}, func(ctx context.Context, n int) (int, error) {
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

// rawUnauthorized stands in for *githubapp.APIError{Status: 401}, which status_labels.go and queries.go still pass.
type rawUnauthorized struct{}

func (rawUnauthorized) Error() string      { return "github: status 401" }
func (rawUnauthorized) Unauthorized() bool { return true }

// An Unauthorized error revokes the credential, so it wins over any other failure regardless of position.
func TestMapConcurrentUnauthorizedWins(t *testing.T) {
	for _, unauth := range []error{
		&RemoteError{Kind: domain.Unauthorized, Status: 401, Err: rawUnauthorized{}},
		rawUnauthorized{},
	} {
		ready := make(chan struct{})
		_, err := MapConcurrent(context.Background(), []int{0, 1}, func(ctx context.Context, n int) (int, error) {
			if n == 0 {
				close(ready)
				return 0, errors.New("upstream 502")
			}
			<-ready
			return 0, unauth
		})
		if !unauthorized(err) {
			t.Fatalf("got %v, want the Unauthorized error", err)
		}
	}
}

// GitHub answers bursts on one user token with secondary rate limits, so at most maxConcurrentCalls run at once.
func TestMapConcurrentCapsInFlightCalls(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	items := make([]int, 10)
	_, err := MapConcurrent(context.Background(), items, func(context.Context, int) (int, error) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return 0, nil
	})
	if err != nil || peak != maxConcurrentCalls {
		t.Fatalf("peak %d, err %v; want peak %d", peak, err, maxConcurrentCalls)
	}
}

// A free slot wins over cancellation: every item that fits in maxConcurrentCalls is started even when the batch is
// already cancelled, so a 401 among them is still seen and wins. A sibling's failure cancels the same ctx, but the
// loop usually reaches the next item before that happens; a cancelled parent puts the loop in that state every time.
// Without the fast path each slot is a coin flip against ctx.Done(), so the rounds make a pass by luck vanishingly rare.
func TestMapConcurrentFreeSlotWinsOverCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for round := range 20 {
		var mu sync.Mutex
		var started []int
		_, err := MapConcurrent(ctx, []int{0, 1, 2}, func(_ context.Context, n int) (int, error) {
			mu.Lock()
			started = append(started, n)
			mu.Unlock()
			switch n {
			case 0:
				return 0, errors.New("upstream 502")
			case 2:
				return 0, rawUnauthorized{}
			}
			return 0, nil
		})
		slices.Sort(started)
		if !slices.Equal(started, []int{0, 1, 2}) || !unauthorized(err) {
			t.Fatalf("round %d: started %v, err %v; want all of [0 1 2] started and the Unauthorized error", round, started, err)
		}
	}
}
