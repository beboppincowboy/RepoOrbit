// Package readguard enforces the shared guarded-read sequence for transport-neutral reads:
//
//  1. Take a user-scoped accessfence.Snapshot before checking repository access.
//  2. Check repository access with fresh=true, splitting repositories into allowed and withheld (unverified)
//     while keeping the latest retry time.
//  3. Run the caller's read callback (which must close rows before returning).
//  4. Re-check the fence; if it moved during the read, discard the value and run the stale handler.
//
// Database errors surface as ErrDatabaseUnavailable ("database unavailable"), never as withheld or unverified.
package readguard

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// ErrDatabaseUnavailable is returned when a database operation in the guard fails.
var ErrDatabaseUnavailable = errors.New("database unavailable")

// Auth holds the result of authorizing a read for an authenticated user.
type Auth struct {
	UserID   int64
	Allowed  []int64
	Withheld int
	RetryAt  time.Time
	Fence    accessfence.Snapshot
	Checks   map[int64]repositories.Decision
}

// Guard authorizes and fences reads for an authenticated user.
type Guard struct {
	Pool    *pgxpool.Pool
	Queries *db.Queries
	Access  *repositories.Service
}

// AuthorizeSelected snapshots the user's access fence, narrows rawIDs to the user's selected repositories,
// and checks access for each selected repository.
func (g *Guard) AuthorizeSelected(ctx context.Context, userID int64, rawIDs []int64) (Auth, error) {
	if len(rawIDs) == 0 {
		return Auth{UserID: userID}, nil
	}
	snap, err := accessfence.TakeSnapshotForUser(ctx, g.Pool, userID)
	if err != nil {
		return Auth{}, ErrDatabaseUnavailable
	}
	selected, err := g.Queries.ListSelectedIDs(ctx, db.ListSelectedIDsParams{UserID: userID, RepositoryIds: rawIDs})
	if err != nil {
		return Auth{}, ErrDatabaseUnavailable
	}
	if len(selected) == 0 {
		return Auth{UserID: userID}, nil
	}
	return g.finishAuth(ctx, userID, selected, snap), nil
}

// AuthorizePreselected snapshots the user's access fence and checks access for already-selected repository IDs.
// It always captures a fence snapshot even when selected is empty.
func (g *Guard) AuthorizePreselected(ctx context.Context, userID int64, selected []int64) (Auth, error) {
	snap, err := accessfence.TakeSnapshotForUser(ctx, g.Pool, userID)
	if err != nil {
		return Auth{}, ErrDatabaseUnavailable
	}
	return g.finishAuth(ctx, userID, selected, snap), nil
}

// FenceOnly captures a user-scoped access fence without running a selection or repository access check.
func (g *Guard) FenceOnly(ctx context.Context, userID int64) (Auth, error) {
	snap, err := accessfence.TakeSnapshotForUser(ctx, g.Pool, userID)
	if err != nil {
		return Auth{}, ErrDatabaseUnavailable
	}
	return Auth{UserID: userID, Fence: snap}, nil
}

func (g *Guard) finishAuth(ctx context.Context, userID int64, selected []int64, snap accessfence.Snapshot) Auth {
	checks := g.Access.Check(ctx, userID, selected, true)
	allowed := make([]int64, 0, len(selected))
	withheld := 0
	var retryAt time.Time
	for _, id := range selected {
		d := checks[id]
		if d.Allowed {
			allowed = append(allowed, id)
		}
		if d.Unverified {
			withheld++
			if !d.RetryAt.IsZero() && (retryAt.IsZero() || d.RetryAt.After(retryAt)) {
				retryAt = d.RetryAt
			}
		}
	}
	return Auth{
		UserID:   userID,
		Allowed:  allowed,
		Withheld: withheld,
		RetryAt:  retryAt,
		Fence:    snap,
		Checks:   checks,
	}
}

// Run executes read and then verifies that auth.Fence is still current. If read returns an error, it is returned
// directly. If the fence check fails or the fence moved, the read value is discarded and onStale is invoked.
func Run[T any](
	ctx context.Context,
	pool *pgxpool.Pool,
	auth Auth,
	read func() (T, error),
	onStale func() (T, error),
) (T, error) {
	val, err := read()
	if err != nil {
		var zero T
		return zero, err
	}
	current, err := auth.Fence.Current(ctx, pool)
	if err != nil || !current {
		return onStale()
	}
	return val, nil
}
