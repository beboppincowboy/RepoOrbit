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

func childKey(repoID, parentID int64) string {
	return fmt.Sprintf("repo:%d:parent:%d", repoID, parentID)
}

// registerChild commits discovered history work with the parent page checkpoint.
func registerChild(ctx context.Context, tx pgx.Tx, repoID int64, resource string, parentID, number int64, now time.Time) error {
	kind := jobs.KindActivityComments
	if resource == "reviews" {
		kind = jobs.KindActivityReviews
	}
	_, err := tx.Exec(ctx, `INSERT INTO activity_children(repository_id,resource,parent_id,parent_number) VALUES($1,$2,$3,$4)
		ON CONFLICT(repository_id,resource,parent_id) DO UPDATE SET parent_number=EXCLUDED.parent_number, cursor='', complete=false, generation=activity_children.generation+1, scan_started_at=NULL, previous_scan=NULL, current_scan='{}'`, repoID, resource, parentID, number)
	if err != nil {
		return err
	}
	return jobs.Enqueue(ctx, tx, kind, childKey(repoID, parentID), now)
}

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

// RequestActivity schedules issue backfill alongside the eligible selection.
func RequestActivity(ctx context.Context, tx pgx.Tx, repoID int64, now time.Time) error {
	ok, err := db.New(tx).HasEligibleSelection(ctx, repoID)
	if err != nil || !ok {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO activity_resources (repository_id, resource, boundary) VALUES ($1, 'issues', $2) ON CONFLICT DO NOTHING`, repoID, now.Add(-90*24*time.Hour)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO activity_resources (repository_id, resource, boundary) SELECT $1, 'pull_requests', boundary FROM activity_resources WHERE repository_id=$1 AND resource='issues' ON CONFLICT DO NOTHING`, repoID); err != nil {
		return err
	}
	for _, resource := range []string{"releases", "issue_events"} {
		if _, err := tx.Exec(ctx, `INSERT INTO activity_resources (repository_id, resource, boundary) SELECT $1, $2, boundary FROM activity_resources WHERE repository_id=$1 AND resource='issues' ON CONFLICT DO NOTHING`, repoID, resource); err != nil {
			return err
		}
	}
	for _, kind := range []jobs.Kind{jobs.KindActivityReleases, jobs.KindActivityIssueEvents} {
		if err := jobs.Enqueue(ctx, tx, kind, repoKey(repoID), now); err != nil {
			return err
		}
	}
	if err := jobs.Enqueue(ctx, tx, jobs.KindActivityIssues, repoKey(repoID), now); err != nil {
		return err
	}
	return jobs.Enqueue(ctx, tx, jobs.KindActivityPullRequests, repoKey(repoID), now)
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
			if err := RequestActivity(ctx, tx, id, time.Now()); err != nil {
				return 0, err
			}
			n++
		}
	}
	return n, tx.Commit(ctx)
}
