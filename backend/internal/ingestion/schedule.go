// Package ingestion owns repository synchronization: scheduling shared metadata refreshes and running them
// with an eligible selecting user's credential. It imports no issue or activity data (activity ingestion does not exist yet).
package ingestion

import (
	"context"
	"errors"
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

// reviewRescanAge bounds how long a finished review history goes without a rescan when its pull request's updated_at
// does not move (an edit GitHub does not reflect there).
const reviewRescanAge = 24 * time.Hour

// registerReviewChild commits a pull request's review history work with the parent page checkpoint. Comments are
// not children: they sync as one repository-wide stream. On a refresh pass, a finished history is reopened only when
// the pull request changed after its last converged scan began, or that scan is older than reviewRescanAge.
func registerReviewChild(ctx context.Context, tx pgx.Tx, repoID, parentID, number int64, updatedAt time.Time, refreshCompleted bool, now time.Time) error {
	result, err := tx.Exec(ctx, `INSERT INTO activity_children(repository_id,resource,parent_id,parent_number) VALUES($1,'reviews',$2,$3)
		ON CONFLICT(repository_id,resource,parent_id) DO UPDATE SET parent_number=EXCLUDED.parent_number, cursor='', complete=false, generation=activity_children.generation+1, scan_started_at=NULL, previous_scan=NULL, current_scan='{}'
		WHERE (activity_children.complete AND $4 AND (activity_children.watermark IS NULL OR activity_children.watermark < $5 OR activity_children.watermark < $6))
			OR activity_children.parent_number <> EXCLUDED.parent_number`, repoID, parentID, number, refreshCompleted, updatedAt, now.Add(-reviewRescanAge))
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	return jobs.Enqueue(ctx, tx, jobs.KindActivityReviews, childKey(repoID, parentID), now)
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
	for _, resource := range []string{"releases", "issue_events", "comments", "actions", "deployments"} {
		if _, err := tx.Exec(ctx, `INSERT INTO activity_resources (repository_id, resource, boundary) SELECT $1, $2, boundary FROM activity_resources WHERE repository_id=$1 AND resource='issues' ON CONFLICT DO NOTHING`, repoID, resource); err != nil {
			return err
		}
	}
	for _, kind := range eventStreamKinds {
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
	ids, err := db.New(tx).ListFollowedRepositoryIDs(ctx)
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

// ActivityOutcome reports what SignalActivity did for one repository.
type ActivityOutcome string

const (
	ActivityAccepted   ActivityOutcome = "accepted"   // new activity work was queued
	ActivityCoalesced  ActivityOutcome = "coalesced"  // work was already queued or running; intent is retained by the queue
	ActivityDead       ActivityOutcome = "dead"       // a stream is dead; only an operator requeue revives it
	ActivityIneligible ActivityOutcome = "ineligible" // no eligible selection: nothing is scheduled
)

var activityParentKinds = []jobs.Kind{jobs.KindActivityIssues, jobs.KindActivityPullRequests, jobs.KindActivityReleases, jobs.KindActivityIssueEvents, jobs.KindActivityComments, jobs.KindActivityActions, jobs.KindActivityDeployments}

const CodePermissionDenied = "permission_denied"

// SignalActivity is the single reconciliation entry point for webhooks, polling and the Refresh action. It
// reopens every caught-up stream for another catch-up scan from the retained boundary (a stream still
// backfilling or catching up already rescans, so it is left alone) and enqueues the stream jobs. It never
// revives dead work and never moves a deferred or paused job's run time earlier, so throttling holds.
func SignalActivity(ctx context.Context, tx pgx.Tx, repoID int64, now time.Time) (ActivityOutcome, error) {
	ok, err := db.New(tx).HasEligibleSelection(ctx, repoID)
	if err != nil || !ok {
		return ActivityIneligible, err
	}
	var busy, dead int
	if err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('queued','running')), count(*) FILTER (WHERE state='dead') FROM jobs WHERE key=$1 AND kind = ANY($2)`, repoKey(repoID), activityParentKinds).Scan(&busy, &dead); err != nil {
		return "", err
	}
	// Job rows before checkpoint rows: sync commits hold their job row and then lock their checkpoint, so taking
	// the checkpoints first here could deadlock with them.
	if err = RequestActivity(ctx, tx, repoID, now); err != nil {
		return "", err
	}
	// A dead stream is skipped, not a reason to stop the others from reconciling.
	if _, err = tx.Exec(ctx, `UPDATE activity_resources r SET phase='catchup',page=1,cursor='',scan_started_at=NULL,current_scan='{}',previous_scan=NULL,generation=generation+1 WHERE repository_id=$1 AND phase='current' AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.key=$2 AND j.kind='activity_'||r.resource AND j.state='dead')`, repoID, repoKey(repoID)); err != nil {
		return "", err
	}
	if dead > 0 {
		return ActivityDead, nil
	}
	if busy > 0 {
		return ActivityCoalesced, nil
	}
	return ActivityAccepted, nil
}

// activityFailureCode maps a job error to the safe code stored in last_error_code. ok is false for a lost claim or an
// internal "continue" deferral, which are not failures. Unknown reasons are recorded as CodeUpstream.
func activityFailureCode(result error) (code string, ok bool) {
	if result == nil || errors.Is(result, jobs.ErrLostClaim) {
		return "", false
	}
	code = CodeUpstream
	var deferred *jobs.Defer
	var paused *jobs.Pause
	if errors.As(result, &deferred) {
		if deferred.Reason == "continue" {
			return "", false
		}
		code = deferred.Reason
	}
	if errors.As(result, &paused) {
		code = paused.Reason
	}
	switch code {
	case CodeNoCredential, CodeRateLimited, CodeUnverified, CodeAccessMoved, CodeUpstream, CodePermissionDenied:
		return code, true
	}
	return CodeUpstream, true
}

// eventStreamKinds are the streams added after issues and pull requests.
var eventStreamKinds = []jobs.Kind{jobs.KindActivityReleases, jobs.KindActivityIssueEvents, jobs.KindActivityComments, jobs.KindActivityActions, jobs.KindActivityDeployments}
