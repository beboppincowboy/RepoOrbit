package ingestion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// withEvidencePage performs remote I/O outside the fenced transaction and rechecks access before committing.
func (d *Deps) withEvidencePage(ctx context.Context, job jobs.Job, fence jobs.ClaimFence, repoID int64, fetch func(string, githubapp.Repo) (any, error), commit func(pgx.Tx, any) error) error {
	q := db.New(d.Pool)
	repo, err := q.GetSyncRepository(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && repo.IsDemo) {
		return nil
	}
	if err != nil {
		return err
	}
	users, err := q.ListEligibleSelectors(ctx, repoID)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return &jobs.Pause{Reason: CodeNoCredential}
	}
	transient := false
	var rateRetry time.Time
	for _, uid := range users {
		if until := d.throttledUntil(ctx, uid); until.After(d.Clock.Now()) {
			rateRetry = earlierRetry(rateRetry, until)
			continue
		}
		token, err := d.Tokens.AccessToken(ctx, uid)
		if errors.Is(err, auth.ErrUnauthenticated) {
			continue
		}
		if err != nil {
			transient = true
			continue
		}
		snap, err := accessfence.Take(ctx, d.Pool, uid, repoID)
		if err != nil {
			return err
		}
		tighten := func() error {
			if repo.IsPrivate {
				return nil
			}
			return tightenToPrivate(ctx, fence, repoID)
		}
		before := d.checkActivityAccess(ctx, uid, repoID, token)
		if before.Unverified {
			if err := tighten(); err != nil && !errors.Is(err, errRace) {
				return err
			}
			if !before.RetryAt.IsZero() {
				d.throttle(ctx, uid, before.RetryAt)
				rateRetry = earlierRetry(rateRetry, before.RetryAt)
			} else {
				transient = true
			}
			continue
		}
		if !before.Allowed {
			continue
		}
		if !repo.IsPrivate && before.Repo.Private {
			if err := tighten(); err != nil && !errors.Is(err, errRace) {
				return err
			}
			return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
		}
		fetchToken := token
		installation := before.InstallationID
		if d.InstallationCredentials != nil && installation != 0 {
			fetchToken, err = d.InstallationCredentials.AccessToken(ctx, installation)
			if err != nil {
				fetchErr := d.installationFetchError(installation, fetchToken, err)
				var deferred *jobs.Defer
				if errors.As(fetchErr, &deferred) {
					rateRetry = earlierRetry(rateRetry, deferred.RetryAt)
					continue
				}
				return fetchErr
			}
		}
		deferIfMoved := func(after repositories.Decision) (bool, error) {
			if after.Allowed && after.InstallationID == before.InstallationID && after.Repo.Private == before.Repo.Private && after.Repo.FullName == before.Repo.FullName {
				return false, nil
			}
			if !after.Allowed || after.Repo.Private {
				if err := tighten(); err != nil && !errors.Is(err, errRace) {
					return true, err
				}
			}
			return true, &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
		}
		data, err := fetch(fetchToken, before.Repo)
		if err != nil && fetchToken != token {
			var api *githubapp.APIError
			if errors.As(err, &api) && api.Unauthorized() {
				after := d.checkActivityAccess(ctx, uid, repoID, token)
				if !after.Unverified {
					if moved, moveErr := deferIfMoved(after); moved {
						return moveErr
					}
				}
			}
			fetchErr := d.installationFetchError(installation, fetchToken, err)
			var deferred *jobs.Defer
			if errors.As(fetchErr, &deferred) {
				rateRetry = earlierRetry(rateRetry, deferred.RetryAt)
				continue
			}
			return fetchErr
		}
		if err != nil {
			var api *githubapp.APIError
			if errors.As(err, &api) && api.RateLimited() {
				d.throttle(ctx, uid, api.RetryAt)
				rateRetry = earlierRetry(rateRetry, api.RetryAt)
				continue
			}
			if errors.As(err, &api) && api.Unauthorized() {
				_ = d.Tokens.Revoke(ctx, uid)
				continue
			}
			return errors.New("activity fetch failed")
		}
		after := d.checkActivityAccess(ctx, uid, repoID, token)
		if after.Unverified {
			if err := tighten(); err != nil && !errors.Is(err, errRace) {
				return err
			}
			if !after.RetryAt.IsZero() {
				d.throttle(ctx, uid, after.RetryAt)
				rateRetry = earlierRetry(rateRetry, after.RetryAt)
				continue
			}
			return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeUnverified}
		}
		if moved, moveErr := deferIfMoved(after); moved {
			return moveErr
		}
		if job.Kind == jobs.KindActivityPullRequests && pullRequestsDisabled(before.Repo) != pullRequestsDisabled(after.Repo) {
			return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
		}
		err = fence.Commit(ctx, func(tx pgx.Tx) error {
			if _, err := db.New(tx).LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: repoID, UserID: uid}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) || isLockNotAvailable(err) {
					return errRace
				}
				return err
			}
			current, err := snap.StillCurrent(ctx, tx)
			if err != nil {
				return err
			}
			if !current {
				return errRace
			}
			return commit(tx, data)
		})
		if errors.Is(err, errRace) {
			return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
		}
		return err
	}
	if !rateRetry.IsZero() {
		return &jobs.Defer{RetryAt: rateRetry, Reason: CodeRateLimited}
	}
	if transient {
		return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeUnverified}
	}
	return &jobs.Pause{Reason: CodeNoCredential}
}

// evidenceActor preserves an absent source actor as SQL NULL rather than a fabricated identity.
func evidenceActor(u *githubapp.IssueUser) (any, any, any, error) {
	if u == nil {
		return nil, nil, nil, nil
	}
	if u.ID <= 0 || u.Login == "" || u.Type == "" {
		return nil, nil, nil, errors.New("invalid activity actor")
	}
	return u.ID, u.Login, u.Type, nil
}

// tightenToPrivate records that repoID turned private: it marks the repository private and bumps its access epoch
// inside one fenced commit. It takes no snapshot check: tightening can only withhold data, so a stale snapshot is no
// reason to skip it, and sharing the global fence row first would turn the bump into a lock upgrade that two
// concurrent tightenings deadlock on.
func tightenToPrivate(ctx context.Context, fence jobs.ClaimFence, repoID int64) error {
	return fence.Commit(ctx, func(tx pgx.Tx) error {
		if err := db.New(tx).MarkRepositoryPrivate(ctx, repoID); err != nil {
			return err
		}
		return accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeRepository, repoID)
	})
}

func (d *Deps) installationFetchError(id int64, token string, err error) error {
	var api *githubapp.APIError
	if errors.As(err, &api) {
		d.InstallationCredentials.RecordAPIError(id, token, api)
		if api.RateLimited() {
			return &jobs.Defer{RetryAt: api.RetryAt, Reason: CodeRateLimited}
		}
	}
	return fmt.Errorf("installation activity fetch: %w", err)
}
