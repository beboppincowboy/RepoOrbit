// Package ingestion owns repository synchronization: scheduling shared metadata refreshes and running them
// with an eligible selecting user's credential. It imports no issue or activity data (activity ingestion does not exist yet).
package ingestion

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

func repoKey(repoID int64) string { return fmt.Sprintf("repo:%d", repoID) }

// RequestMetadataRefresh enqueues a metadata refresh for repoID inside tx, only when the repository
// has at least one active, follow-eligible selection (ordinary events never import
// unselected repositories). Used by selection changes, webhooks and polls. A refresh requested while one
// runs is retained by the queue as a follow-up; a dead one is not revived. Returns whether work was scheduled.
func RequestMetadataRefresh(ctx context.Context, tx pgx.Tx, repoID int64) (bool, error) {
	q := db.New(tx)
	ok, err := q.HasEligibleSelection(ctx, repoID)
	if err != nil || !ok {
		return false, err
	}
	if err := jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, repoKey(repoID), time.Now()); err != nil {
		return false, err
	}
	return true, nil
}

// ReconcileSelections schedules a refresh for every repository with an eligible active selection,
// including selections made before durable scheduling existed. Safe to repeat (the queue coalesces).
// Call at worker startup. Returns how many repositories were scheduled.
// ponytail: one transaction for all repositories; chunk it if selections ever reach many thousands.
func ReconcileSelections(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	ids, err := db.New(tx).ListRefreshableRepositoryIDs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		ok, err := RequestMetadataRefresh(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		if ok {
			n++
		}
	}
	return n, tx.Commit(ctx)
}
