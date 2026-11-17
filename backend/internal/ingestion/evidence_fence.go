package ingestion

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

// withEvidencePage performs remote I/O outside the fenced transaction and rechecks access before committing.
func (d *Deps) withEvidencePage(ctx context.Context, job jobs.Job, fence jobs.ClaimFence, repoID int64, fetch func(string, string) (any, error), commit func(pgx.Tx, any) error) error {
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
	for _, uid := range users {
		if until := d.throttledUntil(ctx, uid); until.After(d.Clock.Now()) {
			return &jobs.Defer{RetryAt: until, Reason: CodeRateLimited}
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
			return fence.Commit(ctx, func(tx pgx.Tx) error {
				current, err := snap.StillCurrent(ctx, tx)
				if err != nil {
					return err
				}
				if !current {
					return errRace
				}
				if err := db.New(tx).MarkRepositoryPrivate(ctx, repoID); err != nil {
					return err
				}
				return accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeRepository, repoID)
			})
		}
		before := d.Access.CheckOne(ctx, uid, repoID, true)
		if before.Unverified {
			if err := tighten(); err != nil && !errors.Is(err, errRace) {
				return err
			}
			transient = true
			if !before.RetryAt.IsZero() {
				d.throttle(ctx, uid, before.RetryAt)
				return &jobs.Defer{RetryAt: before.RetryAt, Reason: CodeRateLimited}
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
		data, err := fetch(token, before.Repo.FullName)
		if err != nil {
			var api *githubapp.APIError
			if errors.As(err, &api) && api.RateLimited() {
				d.throttle(ctx, uid, api.RetryAt)
				return &jobs.Defer{RetryAt: api.RetryAt, Reason: CodeRateLimited}
			}
			if errors.As(err, &api) && api.Unauthorized() {
				_ = d.Tokens.Revoke(ctx, uid)
				continue
			}
			return errors.New("activity fetch failed")
		}
		after := d.Access.CheckOne(ctx, uid, repoID, true)
		if after.Unverified {
			if err := tighten(); err != nil && !errors.Is(err, errRace) {
				return err
			}
			if !after.RetryAt.IsZero() {
				d.throttle(ctx, uid, after.RetryAt)
				return &jobs.Defer{RetryAt: after.RetryAt, Reason: CodeRateLimited}
			}
			return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeUnverified}
		}
		if !after.Allowed || after.Repo.Private != before.Repo.Private || after.Repo.FullName != before.Repo.FullName {
			if !after.Allowed || after.Repo.Private {
				if err := tighten(); err != nil && !errors.Is(err, errRace) {
					return err
				}
			}
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
