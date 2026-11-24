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
type childPage struct {
	reviews []githubapp.Review
	next    string
}

func (d *Deps) syncChild(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) (result error) {
	const resource = "reviews"
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
	err = d.withEvidencePage(ctx, job, fence, repoID, func(token string, repo githubapp.Repo) (any, error) {
		f, ok := d.GitHub.(reviewFetcher)
		if !ok {
			return nil, errors.New("reviews unavailable")
		}
		items, next, err := f.FetchReviewsCursor(ctx, token, repo.FullName, number, cursor)
		return childPage{reviews: items, next: next}, err
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
		ids := make([]int64, 0, len(page.reviews))
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
		_, err = tx.Exec(ctx, `UPDATE activity_children SET scan_started_at=COALESCE(scan_started_at,$4), current_scan=ARRAY(SELECT DISTINCT x FROM unnest(current_scan || $5::bigint[]) AS x ORDER BY x),last_error_code=NULL WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID, started, ids)
		if err != nil {
			return err
		}
		if page.next != "" {
			_, err = tx.Exec(ctx, `UPDATE activity_children SET cursor=$4 WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID, page.next)
			return err
		}
		// A scan is stable when it repeats the previous one, or when it fit in one response ($5): the two-scan rule
		// guards against items shifting between pages, which a single page cannot suffer.
		_, err = tx.Exec(ctx, `UPDATE activity_children SET complete=($5 OR COALESCE(previous_scan=current_scan,false)),watermark=CASE WHEN $5 OR previous_scan=current_scan THEN scan_started_at ELSE watermark END,last_success_at=CASE WHEN $5 OR previous_scan=current_scan THEN $4 ELSE last_success_at END,previous_scan=current_scan,current_scan='{}',scan_started_at=NULL,cursor='' WHERE repository_id=$1 AND resource=$2 AND parent_id=$3`, repoID, resource, parentID, d.Clock.Now(), cursor == "")
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
	return d.syncChild(ctx, job, fence)
}
