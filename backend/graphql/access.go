package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/auth"
)

// userID is the signed-in user's id; real-mode resolvers never run without one.
func (r *Resolver) userID(ctx context.Context) (int64, error) {
	v, ok := auth.FromContext(ctx)
	if !ok || v.Demo || v.UserID == 0 {
		return 0, fmt.Errorf("sign in required")
	}
	return v.UserID, nil
}

// laterRetry keeps the latest retry time (RFC 3339) among withheld items.
func laterRetry(current *string, t time.Time) *string {
	if t.IsZero() {
		return current
	}
	if current != nil {
		if prev, err := time.Parse(time.RFC3339, *current); err == nil && prev.After(t) {
			return current
		}
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func unverifiedError(retryAt time.Time) error {
	if retryAt.IsZero() {
		return fmt.Errorf("access could not be verified; retry later")
	}
	return fmt.Errorf("access could not be verified; retry after %s", retryAt.UTC().Format(time.RFC3339))
}
