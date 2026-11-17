package ingestion

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

type reviewFetcher interface {
	FetchReviewsCursor(context.Context, string, string, int64, string) ([]githubapp.Review, string, error)
}
type commentFetcher interface {
	FetchIssueCommentsCursor(context.Context, string, string, int64, string) ([]githubapp.IssueComment, string, error)
}
type childPage struct {
	reviews  []githubapp.Review
	comments []githubapp.IssueComment
	next     string
}

func (d *Deps) syncChild(ctx context.Context, job jobs.Job, fence jobs.ClaimFence, resource string) (result error) {
	var repoID, parentID int64
	if _, err := fmt.Sscanf(job.Key, "repo:%d:parent:%d", &repoID, &parentID); err != nil || repoID <= 0 || parentID <= 0 || job.Key != childKey(repoID, parentID) {
		return jobs.Permanent(errors.New("invalid child key"))
	}
	defer func() {
		code, ok := activityFailureCode(result)
		if !ok {
			return
		}
		_ = fence.Commit(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE activity_children SET last_error_code=$4 WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID, code)
			return err
		})
	}()
	var number, generation int64
	var cursor string
	var complete bool
	err := d.Pool.QueryRow(ctx, `SELECT parent_number,generation,cursor,complete FROM activity_children WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID).Scan(&number, &generation, &cursor, &complete)
	if errors.Is(err, pgx.ErrNoRows) || complete {
		return nil
	}
	if err != nil {
		return err
	}
	started := d.Clock.Now()
	err = d.withEvidencePage(ctx, job, fence, repoID, func(token, name string) (any, error) {
		if resource == "reviews" {
			f, ok := d.GitHub.(reviewFetcher)
			if !ok {
				return nil, errors.New("reviews unavailable")
			}
			items, next, err := f.FetchReviewsCursor(ctx, token, name, number, cursor)
			return childPage{reviews: items, next: next}, err
		}
		f, ok := d.GitHub.(commentFetcher)
		if !ok {
			return nil, errors.New("comments unavailable")
		}
		items, next, err := f.FetchIssueCommentsCursor(ctx, token, name, number, cursor)
		return childPage{comments: items, next: next}, err
	}, func(tx pgx.Tx, data any) error {
		var liveGen int64
		var liveCursor string
		var liveComplete bool
		err := tx.QueryRow(ctx, `SELECT generation,cursor,complete FROM activity_children WHERE repository_id=$1 AND resource=$2 AND parent_id=$3 FOR UPDATE`, repoID, resource, parentID).Scan(&liveGen, &liveCursor, &liveComplete)
		if errors.Is(err, pgx.ErrNoRows) {
			return errRace
		}
		if err != nil {
			return err
		}
		if liveGen != generation || liveCursor != cursor || liveComplete {
			return errRace
		}
		page := data.(childPage)
		ids := make([]int64, 0, len(page.reviews)+len(page.comments))
		for _, r := range page.reviews {
			if r.SubmittedAt == nil || strings.EqualFold(r.State, "PENDING") {
				continue
			}
			if r.ID <= 0 || r.SubmittedAt.IsZero() {
				return errors.New("invalid review page")
			}
			actorID, actorLogin, actorType, err := evidenceActor(r.User)
			if err != nil {
				return err
			}
			ids = append(ids, r.ID)
			_, err = tx.Exec(ctx, `INSERT INTO activity_reviews(source_id,repository_id,pull_request_id,state,body,html_url,submitted_at,author_id,author_login,author_type) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(source_id) DO UPDATE SET state=EXCLUDED.state,body=EXCLUDED.body,html_url=EXCLUDED.html_url,submitted_at=EXCLUDED.submitted_at,author_id=EXCLUDED.author_id,author_login=EXCLUDED.author_login,author_type=EXCLUDED.author_type WHERE activity_reviews.repository_id=EXCLUDED.repository_id AND activity_reviews.submitted_at<EXCLUDED.submitted_at`, r.ID, repoID, parentID, r.State, r.Body, r.HTMLURL, r.SubmittedAt, actorID, actorLogin, actorType)
			if err != nil {
				return err
			}
		}
		for _, c := range page.comments {
			if c.ID <= 0 || c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
				return errors.New("invalid comment page")
			}
			actorID, actorLogin, actorType, err := evidenceActor(c.User)
			if err != nil {
				return err
			}
			ids = append(ids, c.ID)
			_, err = tx.Exec(ctx, `INSERT INTO activity_comments(source_id,repository_id,issue_number,body,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(source_id) DO UPDATE SET body=EXCLUDED.body,html_url=EXCLUDED.html_url,updated_at=EXCLUDED.updated_at,author_id=EXCLUDED.author_id,author_login=EXCLUDED.author_login,author_type=EXCLUDED.author_type WHERE activity_comments.repository_id=EXCLUDED.repository_id AND activity_comments.updated_at<EXCLUDED.updated_at`, c.ID, repoID, number, c.Body, c.HTMLURL, c.CreatedAt, c.UpdatedAt, actorID, actorLogin, actorType)
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE activity_children SET scan_started_at=COALESCE(scan_started_at,$4), current_scan=ARRAY(SELECT DISTINCT x FROM unnest(current_scan || $5::bigint[]) AS x ORDER BY x),last_error_code=NULL WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID, started, ids)
		if err != nil {
			return err
		}
		if page.next != "" {
			_, err = tx.Exec(ctx, `UPDATE activity_children SET cursor=$4 WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID, page.next)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE activity_children SET complete=COALESCE(previous_scan=current_scan,false),watermark=CASE WHEN previous_scan=current_scan THEN scan_started_at ELSE watermark END,last_success_at=CASE WHEN previous_scan=current_scan THEN $4 ELSE last_success_at END,previous_scan=current_scan,current_scan='{}',scan_started_at=NULL,cursor='' WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID, d.Clock.Now())
		return err
	})
	if err != nil {
		return err
	}
	var done bool
	if err = d.Pool.QueryRow(ctx, `SELECT complete FROM activity_children WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	return &jobs.Defer{RetryAt: d.Clock.Now(), Reason: "continue"}
}
func (d *Deps) SyncReviews(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
	return d.syncChild(ctx, job, fence, "reviews")
}
func (d *Deps) SyncComments(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
	return d.syncChild(ctx, job, fence, "comments")
}
