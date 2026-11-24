package ingestion

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

type pullRequestFetcher interface {
	FetchPullRequestsCursor(context.Context, string, string, string) ([]githubapp.PullRequest, string, error)
}

const CodePullRequestsDisabled = "pull_requests_disabled"

func pullRequestsDisabled(repo githubapp.Repo) bool {
	return repo.HasPullRequests != nil && !*repo.HasPullRequests
}

type pullRequestPage struct {
	disabled bool
	items    []githubapp.PullRequest
	next     string
}

// SyncPullRequests imports one page and atomically registers all required child histories.
func (d *Deps) SyncPullRequests(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) (result error) {
	repoID, err := parseRepoKey(job.Key)
	if err != nil {
		return jobs.Permanent(err)
	}
	defer func() {
		code, ok := activityFailureCode(result)
		if !ok {
			return
		}
		_ = fence.Commit(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE activity_resources SET last_error_code=$2 WHERE repository_id=$1 AND resource='pull_requests'`, repoID, code)
			return err
		})
	}()
	var phase, cursor string
	var generation int64
	var boundary time.Time
	err = d.Pool.QueryRow(ctx, `SELECT phase,cursor,generation,boundary FROM activity_resources WHERE repository_id=$1 AND resource='pull_requests'`, repoID).Scan(&phase, &cursor, &generation, &boundary)
	if errors.Is(err, pgx.ErrNoRows) || phase == "current" {
		return nil
	}
	if err != nil {
		return err
	}
	fetcher, ok := d.GitHub.(pullRequestFetcher)
	if !ok {
		return jobs.Permanent(errors.New("pull request fetch unavailable"))
	}
	started := d.Clock.Now()
	skipped := false
	err = d.withEvidencePage(ctx, job, fence, repoID, func(token string, repo githubapp.Repo) (any, error) {
		if pullRequestsDisabled(repo) {
			return pullRequestPage{disabled: true}, nil
		}
		items, next, err := fetcher.FetchPullRequestsCursor(ctx, token, repo.FullName, cursor)
		return pullRequestPage{items: items, next: next}, err
	}, func(tx pgx.Tx, data any) error {
		var storedPhase, storedCursor string
		var storedGeneration int64
		var firstPass bool
		err := tx.QueryRow(ctx, `SELECT phase,cursor,generation,previous_scan IS NULL FROM activity_resources WHERE repository_id=$1 AND resource='pull_requests' FOR UPDATE`, repoID).Scan(&storedPhase, &storedCursor, &storedGeneration, &firstPass)
		if errors.Is(err, pgx.ErrNoRows) {
			return errRace
		}
		if err != nil {
			return err
		}
		if phase != storedPhase || cursor != storedCursor || generation != storedGeneration {
			return errRace
		}
		page := data.(pullRequestPage)
		if page.disabled {
			skipped = true
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase='current',page=1,cursor='',scan_started_at=NULL,previous_scan=NULL,current_scan='{}',watermark=NULL,last_error_code=$2 WHERE repository_id=$1 AND resource='pull_requests'`, repoID, CodePullRequestsDisabled)
			return err
		}
		ids := make([]int64, 0, len(page.items))
		for _, p := range page.items {
			if p.ID <= 0 || p.Number <= 0 || p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
				return errors.New("invalid pull request page")
			}
			if p.State != "open" && p.CreatedAt.Before(boundary) && p.UpdatedAt.Before(boundary) && (p.MergedAt == nil || p.MergedAt.Before(boundary)) && (p.ClosedAt == nil || p.ClosedAt.Before(boundary)) {
				continue
			}
			actorID, actorLogin, actorType, err := evidenceActor(p.User)
			if err != nil {
				return err
			}
			ids = append(ids, p.ID)
			_, err = tx.Exec(ctx, `INSERT INTO activity_pull_requests(source_id,repository_id,number,title,state,html_url,created_at,updated_at,closed_at,merged_at,author_id,author_login,author_type) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(source_id) DO UPDATE SET title=EXCLUDED.title,state=EXCLUDED.state,html_url=EXCLUDED.html_url,updated_at=EXCLUDED.updated_at,closed_at=EXCLUDED.closed_at,merged_at=EXCLUDED.merged_at,author_id=EXCLUDED.author_id,author_login=EXCLUDED.author_login,author_type=EXCLUDED.author_type WHERE activity_pull_requests.repository_id=EXCLUDED.repository_id AND activity_pull_requests.updated_at<EXCLUDED.updated_at`, p.ID, repoID, p.Number, p.Title, p.State, p.HTMLURL, p.CreatedAt, p.UpdatedAt, p.ClosedAt, p.MergedAt, actorID, actorLogin, actorType)
			if err != nil {
				return err
			}
			if err = registerChild(ctx, tx, repoID, "reviews", p.ID, p.Number, firstPass, started); err != nil {
				return err
			}
			if err = registerChild(ctx, tx, repoID, "comments", p.Number, p.Number, firstPass, started); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE activity_resources SET scan_started_at=COALESCE(scan_started_at,$2),current_scan=ARRAY(SELECT DISTINCT x FROM unnest(current_scan || $3::bigint[]) AS x ORDER BY x),last_error_code=NULL WHERE repository_id=$1 AND resource='pull_requests'`, repoID, started, ids)
		if err != nil {
			return err
		}
		if page.next != "" {
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET page=page+1,cursor=$2 WHERE repository_id=$1 AND resource='pull_requests'`, repoID, page.next)
			return err
		}
		if phase == "backfill" {
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase='catchup',page=1,cursor='',scan_started_at=NULL,current_scan='{}',previous_scan=NULL WHERE repository_id=$1 AND resource='pull_requests'`, repoID)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase=CASE WHEN previous_scan=current_scan THEN 'current' ELSE 'catchup' END,watermark=CASE WHEN previous_scan=current_scan THEN scan_started_at ELSE watermark END,last_success_at=CASE WHEN previous_scan=current_scan THEN $2 ELSE last_success_at END,previous_scan=current_scan,current_scan='{}',scan_started_at=NULL,page=1,cursor='' WHERE repository_id=$1 AND resource='pull_requests'`, repoID, d.Clock.Now())
		return err
	})
	if err != nil || skipped {
		return err
	}
	return &jobs.Defer{RetryAt: d.Clock.Now(), Reason: "continue"}
}
