package ingestion

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

type issueFetcher interface {
	FetchIssuesCursor(context.Context, string, string, string) ([]githubapp.Issue, string, error)
}

type issueTimelineFetcher interface {
	FetchIssueTimelinePRLinks(context.Context, string, string, int64) ([]githubapp.IssuePRLink, error)
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
	// withEvidencePage pauses a job without eligible selectors; an issue job (demo repositories included) finishes.
	users, err := db.New(d.Pool).ListEligibleSelectors(ctx, repoID)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	var scanTime, now time.Time
	var items []githubapp.Issue
	var links map[int64][]githubapp.IssuePRLink
	var next string
	committed := false
	err = d.withEvidencePage(ctx, job, fence, repoID, func(token string, repo githubapp.Repo) (any, error) {
		scanTime = d.Clock.Now()
		var err error
		items, next, err = fetcher.FetchIssuesCursor(ctx, token, repo.FullName, cursor)
		if err == nil {
			if timeline, ok := d.GitHub.(issueTimelineFetcher); ok {
				links = make(map[int64][]githubapp.IssuePRLink)
				for _, issue := range items {
					if issue.PullRequest != nil || issue.Number <= 0 || (issue.State != "open" && issue.CreatedAt.Before(boundary) && issue.UpdatedAt.Before(boundary) && (issue.ClosedAt == nil || issue.ClosedAt.Before(boundary))) {
						continue
					}
					found, linkErr := timeline.FetchIssueTimelinePRLinks(ctx, token, repo.FullName, issue.Number)
					if linkErr != nil {
						if failsIssuePage(ctx, linkErr) {
							return nil, linkErr
						}
						// No map entry: the commit keeps the issue's stored links.
						slog.Warn("issue timeline skipped", "repository", repoID, "issue", issue.Number, "err", linkErr)
						continue
					}
					links[issue.Number] = found
				}
			}
		}
		return nil, err
	}, func(tx pgx.Tx, _ any) error {
		committed, now = true, d.Clock.Now()
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
			if err := ProjectIssue(ctx, tx, repoID, issue); err != nil {
				return err
			}
			if issueLinks, ok := links[issue.Number]; ok {
				if _, err := tx.Exec(ctx, `DELETE FROM issue_pull_request_links WHERE repository_id=$1 AND issue_number=$2 AND source IN ('connected','cross-referenced')`, repoID, issue.Number); err != nil {
					return err
				}
				for _, link := range issueLinks {
					if _, err := tx.Exec(ctx, `INSERT INTO issue_pull_request_links(repository_id,issue_number,pull_request_number,source,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(repository_id,issue_number,pull_request_number) DO UPDATE SET source=EXCLUDED.source,created_at=EXCLUDED.created_at`, repoID, issue.Number, link.Number, link.Source, link.CreatedAt); err != nil {
						return err
					}
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
	if err != nil || !committed {
		return err
	}
	return &jobs.Defer{RetryAt: now, Reason: "continue"}
}

// failsIssuePage reports whether a timeline fetch error must fail the whole issue page (cancellation, rate limit,
// rejected credential or forbidden) rather than skip that one issue's links.
func failsIssuePage(ctx context.Context, err error) bool {
	var api *githubapp.APIError
	return ctx.Err() != nil || errors.As(err, &api) && (api.RateLimited() || api.Unauthorized() || api.Status == http.StatusForbidden)
}

// ProjectIssue upserts one GitHub issue (from a sync page or a create response) with its labels and assignees into the
// activity projection, inside tx. The caller must already have checked its access fence in tx (a sync: claim fence
// and accessfence StillCurrent; a write: StillCurrent); ProjectIssue does not check it. The caller must skip pull requests (issue.PullRequest).
// A stored copy that is as new or newer, or a tombstoned issue, is left alone and nil returned: a sync page may have
// been fetched before the stored copy. An invalid author, label or assignee is an error.
func ProjectIssue(ctx context.Context, tx pgx.Tx, repoID int64, issue githubapp.Issue) error {
	return projectIssue(ctx, tx, repoID, issue, false, true)
}

// LockIssue takes the per-issue write lock for the rest of tx: issue-write readbacks hold it while they project
// GitHub's confirmed copy, and a signed issue deletion takes it before its tombstone, so neither misses the other's
// uncommitted write. It is reentrant within one transaction.
func LockIssue(ctx context.Context, tx pgx.Tx, repoID, number int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+issueLockKey+`)`, repoID, number)
	return err
}

// tryLockIssue is LockIssue without waiting: false means another transaction holds the issue.
func tryLockIssue(ctx context.Context, tx pgx.Tx, repoID, number int64) (bool, error) {
	var locked bool
	err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(`+issueLockKey+`)`, repoID, number).Scan(&locked)
	return locked, err
}

// issueLockKey is the advisory key of one issue ($1 repository id, $2 issue number) shared by every per-issue lock.
const issueLockKey = `hashtextextended(format('issue-write:%s:%s', $1::bigint, $2::bigint), 0)`

// ProjectConfirmedIssue is ProjectIssue for an issue read back right after a confirmed write, by a caller that
// serializes readbacks of that issue (so the later readback is projected last). It also replaces a stored copy with
// the same updated_at: GitHub's timestamps have one-second resolution, so an edit in the same second
// as the stored copy would otherwise be lost.
func ProjectConfirmedIssue(ctx context.Context, tx pgx.Tx, repoID int64, issue githubapp.Issue) error {
	return projectIssue(ctx, tx, repoID, issue, true, true)
}

// projectCleanupObservation is ProjectIssue for the status label cleanup's own readback. Not the same-second variant:
// the readback was taken before the lock, so it must not replace a copy an issue write projected in the same second. It
// never queues the cleanup again, so a refused removal cannot make the cleanup schedule itself forever.
func projectCleanupObservation(ctx context.Context, tx pgx.Tx, repoID int64, issue githubapp.Issue) error {
	return projectIssue(ctx, tx, repoID, issue, false, false)
}

// projectIssue is ProjectIssue (sameSecond false) or ProjectConfirmedIssue (true). With cleanup, a projected closed
// issue that still has a status label queues the app's status label cleanup; the cleanup itself projects without it.
func projectIssue(ctx context.Context, tx pgx.Tx, repoID int64, issue githubapp.Issue, sameSecond, cleanup bool) error {
	actorID, actorLogin, actorType, err := evidenceActor(issue.User)
	if err != nil {
		return err
	}
	var inserted int64
	err = tx.QueryRow(ctx, `INSERT INTO activity_issues (source_id, repository_id, number, title, state, html_url, created_at, updated_at, closed_at, author_id, author_login, author_type) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (source_id) DO UPDATE SET title=EXCLUDED.title,state=EXCLUDED.state,html_url=EXCLUDED.html_url,updated_at=EXCLUDED.updated_at,closed_at=EXCLUDED.closed_at,author_id=EXCLUDED.author_id,author_login=EXCLUDED.author_login,author_type=EXCLUDED.author_type WHERE activity_issues.repository_id=EXCLUDED.repository_id AND (activity_issues.updated_at < EXCLUDED.updated_at OR ($13 AND activity_issues.updated_at = EXCLUDED.updated_at)) RETURNING source_id`, issue.ID, repoID, issue.Number, issue.Title, issue.State, issue.HTMLURL, issue.CreatedAt, issue.UpdatedAt, issue.ClosedAt, actorID, actorLogin, actorType, sameSecond).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the stored copy is as new or newer, or the issue is tombstoned
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
	if !cleanup {
		return nil
	}
	return enqueueStatusCleanup(ctx, tx, repoID, issue)
}
