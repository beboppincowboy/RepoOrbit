package ingestion

import (
	"context"
	"crypto/rsa"
	"errors"
	"log"
	"reflect"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

// AppInventory obtains a complete, authenticated app-wide installation/repository inventory.
type AppInventory interface {
	AppInventory(context.Context, *rsa.PrivateKey) (map[int64][]int64, error)
}

// CleanupPrivate removes a private projection only after two matching complete inventories.
func (d *Deps) CleanupPrivate(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
	repoID, err := parseRepoKey(job.Key)
	if err != nil {
		return jobs.Permanent(err)
	}
	// Every deferral first wipes the drafts this cleanup was told to (a revoked installation's holders), so they never
	// wait on a complete inventory: one suspended installation anywhere fails every inventory. The shared projection
	// still waits. A covered holder loses drafts too; that is the fail-closed side. wipe_users is cleared only if no
	// invalidation arrived meanwhile (version unchanged). The wipe commits through the claim fence like every other
	// cleanup effect: a worker that lost its claim wipes nothing and returns ErrLostClaim.
	retry := func(at time.Time) error {
		if err := fence.Commit(ctx, func(tx pgx.Tx) error {
			var version int64
			err := tx.QueryRow(ctx, `WITH c AS (SELECT c.version, c.wipe_users FROM activity_cleanup c JOIN repositories r ON r.id = c.repository_id
				WHERE c.repository_id = $1 AND r.is_private AND c.wipe_users <> '{}'), w AS (
				UPDATE write_operations o SET draft = NULL, outcome = redacted_write_outcome(o.outcome) FROM c
				WHERE o.repository_id = $1 AND o.draft IS NOT NULL AND o.user_id = ANY(c.wipe_users))
				SELECT version FROM c`, repoID).Scan(&version)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE activity_cleanup SET wipe_users = '{}' WHERE repository_id = $1 AND version = $2`, repoID, version)
			return err
		}); errors.Is(err, jobs.ErrLostClaim) {
			return err
		} else if err != nil {
			log.Printf("ingestion: cleanup draft wipe for repository %d: %v", repoID, err)
		}
		if at.IsZero() {
			at = d.Clock.Now().Add(time.Minute)
		}
		return &jobs.Defer{RetryAt: at, Reason: CodeUnverified}
	}
	var version int64
	var private bool
	err = d.Pool.QueryRow(ctx, `SELECT c.version, r.is_private FROM activity_cleanup c JOIN repositories r ON r.id=c.repository_id WHERE c.repository_id=$1`, repoID).Scan(&version, &private)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return retry(time.Time{})
	}
	snap, err := accessfence.Take(ctx, d.Pool, 0, repoID)
	if err != nil {
		return retry(time.Time{})
	}
	if d.Inventory == nil || d.AppKey == nil {
		return retry(time.Time{})
	}
	first, err := d.Inventory.AppInventory(ctx, d.AppKey)
	if err != nil {
		var api *githubapp.APIError
		if errors.As(err, &api) {
			return retry(api.RetryAt)
		}
		return retry(time.Time{})
	}
	second, err := d.Inventory.AppInventory(ctx, d.AppKey)
	if err != nil {
		var api *githubapp.APIError
		if errors.As(err, &api) {
			return retry(api.RetryAt)
		}
		return retry(time.Time{})
	}
	if !reflect.DeepEqual(first, second) {
		return retry(time.Time{})
	}
	covering := []int64{} // installations whose inventory includes the repository
	for inst, ids := range second {
		if slices.Contains(ids, repoID) {
			covering = append(covering, inst)
		}
	}
	present := len(covering) > 0
	// Holding a covering installation is not access: GitHub grants a member only the covered repositories they can
	// see themselves. Of the actors a covering installation names, keep the drafts of exactly those a fresh check
	// still allows; an unverifiable check defers the whole cleanup, as an incomplete inventory does.
	keep := []int64{}
	if private && present {
		rows, err := d.Pool.Query(ctx, `SELECT DISTINCT o.user_id FROM write_operations o JOIN installations i ON i.user_id = o.user_id
			WHERE o.repository_id = $1 AND o.draft IS NOT NULL AND i.installation_id = ANY($2) ORDER BY o.user_id`, repoID, covering)
		if err != nil {
			return retry(time.Time{})
		}
		actors, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return retry(time.Time{})
		}
		for _, uid := range actors {
			if d.Access == nil {
				return retry(time.Time{})
			}
			switch dec := d.Access.CheckOne(ctx, uid, repoID, true); {
			case dec.Unverified:
				return retry(dec.RetryAt)
			case dec.Allowed:
				keep = append(keep, uid)
			}
		}
	}
	err = fence.Commit(ctx, func(tx pgx.Tx) error {
		current, err := snap.StillCurrent(ctx, tx)
		if err != nil {
			return err
		}
		if !current {
			return errRace
		}
		var liveVersion int64
		var livePrivate bool
		err = tx.QueryRow(ctx, `SELECT c.version, r.is_private FROM activity_cleanup c JOIN repositories r ON r.id=c.repository_id WHERE c.repository_id=$1 FOR UPDATE OF c, r`, repoID).Scan(&liveVersion, &livePrivate)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if liveVersion != version || livePrivate != private {
			return errRace
		}
		// Every actor not confirmed above lost private access: their issue-write drafts go and their outcomes are
		// redacted (everyone's when no installation covers). An access change since the check bumps the version.
		if private {
			if _, err = tx.Exec(ctx, `UPDATE write_operations SET draft=NULL, outcome=redacted_write_outcome(outcome) WHERE repository_id=$1 AND draft IS NOT NULL
				AND NOT (user_id = ANY($2))`, repoID, keep); err != nil {
				return err
			}
		}
		if private && !present {
			if _, err = tx.Exec(ctx, `DELETE FROM activity_children WHERE repository_id=$1`, repoID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `DELETE FROM activity_comments WHERE repository_id=$1`, repoID); err != nil {
				return err
			}
			for _, table := range []string{"activity_releases", "activity_issue_events", "app_operations", "workflow_jobs", "workflow_runs", "workflows", "deployments", "issue_pull_request_links"} {
				if _, err = tx.Exec(ctx, `DELETE FROM `+table+` WHERE repository_id=$1`, repoID); err != nil {
					return err
				}
			}
			// Reviews normally go with their PR via the cascade; delete by their own repository too so a review
			// tagged with this repository whose PR row lives elsewhere cannot outlive the purge.
			if _, err = tx.Exec(ctx, `DELETE FROM activity_reviews WHERE repository_id=$1`, repoID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `DELETE FROM activity_pull_requests WHERE repository_id=$1`, repoID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `DELETE FROM activity_issues WHERE repository_id=$1`, repoID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `DELETE FROM issues WHERE repository_id=$1`, repoID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE activity_resources SET phase='backfill', page=1, cursor='', generation=generation+1, watermark=NULL, scan_started_at=NULL, previous_scan=NULL, current_scan='{}', last_success_at=NULL, last_error_code=NULL WHERE repository_id=$1`, repoID); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM activity_cleanup WHERE repository_id=$1 AND version=$2`, repoID, version)
		return err
	})
	if errors.Is(err, jobs.ErrLostClaim) {
		return err
	}
	if err != nil {
		return retry(time.Time{})
	}
	return nil
}
