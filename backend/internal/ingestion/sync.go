package ingestion

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

type issueFetcher interface {
	FetchIssuesCursor(context.Context, string, string, string) ([]githubapp.Issue, string, error)
}

// SyncIssues imports one page and its checkpoint under the claim, selection and access fences.
func (d *Deps) SyncIssues(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) (result error) {
	defer func() {
		code, ok := activityFailureCode(result)
		if !ok {
			return
		}
		repoID, err := parseRepoKey(job.Key)
		if err != nil {
			return
		}
		_ = fence.Commit(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE activity_resources SET last_error_code=$2 WHERE repository_id=$1 AND resource='issues'`, repoID, code)
			return err
		})
	}()
	repoID, err := parseRepoKey(job.Key)
	if err != nil {
		return jobs.Permanent(err)
	}
	fetcher, ok := d.GitHub.(issueFetcher)
	if !ok {
		return jobs.Permanent(errors.New("issue fetch unavailable"))
	}
	q := db.New(d.Pool)
	repo, err := q.GetSyncRepository(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && repo.IsDemo) {
		return nil
	}
	if err != nil {
		return err
	}
	var boundary time.Time
	var phase string
	var cursor string
	var page int
	var generation int64
	err = d.Pool.QueryRow(ctx, `SELECT boundary, phase, page, generation, cursor FROM activity_resources WHERE repository_id=$1 AND resource='issues'`, repoID).Scan(&boundary, &phase, &page, &generation, &cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if phase == "current" {
		return nil
	}
	users, err := q.ListEligibleSelectors(ctx, repoID)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
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
		before := d.Access.CheckOne(ctx, uid, repoID, true)
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
		scanTime := d.Clock.Now()
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
		items, next, err := fetcher.FetchIssuesCursor(ctx, fetchToken, before.Repo.FullName, cursor)
		if err != nil && fetchToken != token {
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
			return errors.New("issue fetch failed")
		}
		after := d.Access.CheckOne(ctx, uid, repoID, true)
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
		if !after.Allowed || after.Repo.Private != before.Repo.Private || after.Repo.FullName != before.Repo.FullName {
			if !after.Allowed || after.Repo.Private {
				if err := tighten(); err != nil && !errors.Is(err, errRace) {
					return err
				}
			}
			return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
		}
		now := d.Clock.Now()
		err = fence.Commit(ctx, func(tx pgx.Tx) error {
			if _, err := db.New(tx).LockEligibleSelection(ctx, db.LockEligibleSelectionParams{RepositoryID: repoID, UserID: uid}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) || isLockNotAvailable(err) {
					return errRace
				}
				return err
			}
			if current, err := snap.StillCurrent(ctx, tx); err != nil {
				return err
			} else if !current {
				return errRace
			}
			var storedPage int
			var storedPhase string
			var storedCursor string
			var storedGeneration int64
			var firstPass bool
			if err := tx.QueryRow(ctx, `SELECT page, phase, generation, cursor, previous_scan IS NULL FROM activity_resources WHERE repository_id=$1 AND resource='issues' FOR UPDATE`, repoID).Scan(&storedPage, &storedPhase, &storedGeneration, &storedCursor, &firstPass); err != nil {
				return err
			}
			if storedPage != page || storedPhase != phase || storedGeneration != generation || storedCursor != cursor {
				return errRace
			}
			ids := make([]int64, 0, len(items))
			for _, issue := range items {
				if issue.PullRequest != nil {
					continue
				}
				if issue.ID <= 0 || issue.CreatedAt.IsZero() || issue.UpdatedAt.IsZero() {
					return errors.New("invalid issue page")
				}
				if issue.State != "open" && issue.CreatedAt.Before(boundary) && issue.UpdatedAt.Before(boundary) && (issue.ClosedAt == nil || issue.ClosedAt.Before(boundary)) {
					continue
				}
				ids = append(ids, issue.ID)
				var tombstoned bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM activity_tombstones WHERE resource='issues' AND source_id=$1)`, issue.ID).Scan(&tombstoned); err != nil {
					return err
				}
				if tombstoned {
					continue
				}
				if err := registerChild(ctx, tx, repoID, "comments", issue.Number, issue.Number, firstPass, now); err != nil {
					return err
				}
				actorID, actorLogin, actorType, err := evidenceActor(issue.User)
				if err != nil {
					return err
				}
				var inserted int64
				err = tx.QueryRow(ctx, `INSERT INTO activity_issues (source_id, repository_id, number, title, state, html_url, created_at, updated_at, closed_at, author_id, author_login, author_type) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (source_id) DO UPDATE SET title=EXCLUDED.title,state=EXCLUDED.state,html_url=EXCLUDED.html_url,updated_at=EXCLUDED.updated_at,closed_at=EXCLUDED.closed_at,author_id=EXCLUDED.author_id,author_login=EXCLUDED.author_login,author_type=EXCLUDED.author_type WHERE activity_issues.repository_id=EXCLUDED.repository_id AND activity_issues.updated_at < EXCLUDED.updated_at RETURNING source_id`, issue.ID, repoID, issue.Number, issue.Title, issue.State, issue.HTMLURL, issue.CreatedAt, issue.UpdatedAt, issue.ClosedAt, actorID, actorLogin, actorType).Scan(&inserted)
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `DELETE FROM activity_issue_labels WHERE issue_id=$1`, issue.ID); err != nil {
					return err
				}
				for _, label := range issue.Labels {
					if label.ID <= 0 {
						return errors.New("invalid issue label")
					}
					if _, err = tx.Exec(ctx, `INSERT INTO activity_issue_labels(issue_id,source_id,name) VALUES($1,$2,$3)`, issue.ID, label.ID, label.Name); err != nil {
						return err
					}
				}
				if _, err = tx.Exec(ctx, `DELETE FROM activity_issue_assignees WHERE issue_id=$1`, issue.ID); err != nil {
					return err
				}
				for _, assignee := range issue.Assignees {
					if assignee.ID <= 0 {
						return errors.New("invalid issue assignee")
					}
					if _, err = tx.Exec(ctx, `INSERT INTO activity_issue_assignees(issue_id,source_id,login,type) VALUES($1,$2,$3,$4)`, issue.ID, assignee.ID, assignee.Login, assignee.Type); err != nil {
						return err
					}
				}
			}
			_, err := tx.Exec(ctx, `UPDATE activity_resources SET scan_started_at=COALESCE(scan_started_at,$3), current_scan=ARRAY(SELECT DISTINCT x FROM unnest(current_scan || $4::bigint[]) AS x ORDER BY x), last_error_code=NULL WHERE repository_id=$1 AND resource=$2`, repoID, "issues", scanTime, ids)
			if err != nil {
				return err
			}
			if next != "" {
				_, err = tx.Exec(ctx, `UPDATE activity_resources SET page=page+1, cursor=$2 WHERE repository_id=$1 AND resource='issues'`, repoID, next)
				return err
			}
			if phase == "backfill" {
				_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase='catchup',page=1,cursor='',scan_started_at=NULL,current_scan='{}',previous_scan=NULL WHERE repository_id=$1 AND resource='issues'`, repoID)
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase=CASE WHEN previous_scan=current_scan THEN 'current' ELSE 'catchup' END, watermark=CASE WHEN previous_scan=current_scan THEN scan_started_at ELSE watermark END, last_success_at=CASE WHEN previous_scan=current_scan THEN $2 ELSE last_success_at END, previous_scan=current_scan, current_scan='{}',scan_started_at=NULL,page=1,cursor='' WHERE repository_id=$1 AND resource='issues'`, repoID, now)
			return err
		})
		if errors.Is(err, errRace) {
			return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeAccessMoved}
		}
		if err != nil {
			return err
		}
		return &jobs.Defer{RetryAt: now, Reason: "continue"}
	}
	if !rateRetry.IsZero() {
		return &jobs.Defer{RetryAt: rateRetry, Reason: CodeRateLimited}
	}
	if transient {
		return &jobs.Defer{RetryAt: d.Clock.Now().Add(raceRetry), Reason: CodeUnverified}
	}
	return &jobs.Pause{Reason: CodeNoCredential}
}
