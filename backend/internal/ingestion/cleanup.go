package ingestion

import (
	"context"
	"crypto/rsa"
	"errors"
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
	retry := func(at time.Time) error {
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
		// Private access comes only through installations: an actor who holds none of the covering installations
		// lost it, so their issue-write drafts go and their outcomes are redacted (everyone's when none covers).
		if private {
			if _, err = tx.Exec(ctx, `UPDATE write_operations SET draft=NULL, outcome=redacted_write_outcome(outcome) WHERE repository_id=$1 AND draft IS NOT NULL
				AND user_id NOT IN (SELECT user_id FROM installations WHERE installation_id = ANY($2))`, repoID, covering); err != nil {
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
			for _, table := range []string{"activity_releases", "activity_issue_events"} {
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
