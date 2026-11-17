package ingestion

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

type releaseFetcher interface {
	FetchReleasesCursor(context.Context, string, string, string) ([]githubapp.Release, string, error)
}
type issueEventFetcher interface {
	FetchIssueEventsCursor(context.Context, string, string, string) ([]githubapp.IssueEvent, string, error)
}
type activityPage struct {
	items any
	next  string
}

func supportedIssueEvent(event string) bool {
	switch event {
	case "closed", "reopened", "assigned", "unassigned", "labeled", "unlabeled":
		return true
	}
	return false
}
func (d *Deps) SyncReleases(ctx context.Context, j jobs.Job, f jobs.ClaimFence) error {
	return d.syncEventResource(ctx, j, f, "releases")
}
func (d *Deps) SyncIssueEvents(ctx context.Context, j jobs.Job, f jobs.ClaimFence) error {
	return d.syncEventResource(ctx, j, f, "issue_events")
}

func (d *Deps) syncEventResource(ctx context.Context, job jobs.Job, fence jobs.ClaimFence, resource string) (result error) {
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
			_, err := tx.Exec(ctx, `UPDATE activity_resources SET last_error_code=$3 WHERE repository_id=$1 AND resource=$2`, repoID, resource, code)
			return err
		})
	}()
	var phase, cursor string
	var generation int64
	err = d.Pool.QueryRow(ctx, `SELECT phase,cursor,generation FROM activity_resources WHERE repository_id=$1 AND resource=$2`, repoID, resource).Scan(&phase, &cursor, &generation)
	if errors.Is(err, pgx.ErrNoRows) || phase == "current" {
		return nil
	}
	if err != nil {
		return err
	}
	started := d.Clock.Now()
	err = d.withEvidencePage(ctx, job, fence, repoID, func(token, name string) (any, error) {
		if resource == "releases" {
			f, ok := d.GitHub.(releaseFetcher)
			if !ok {
				return nil, errors.New("release fetch unavailable")
			}
			items, next, err := f.FetchReleasesCursor(ctx, token, name, cursor)
			return activityPage{items, next}, err
		}
		f, ok := d.GitHub.(issueEventFetcher)
		if !ok {
			return nil, errors.New("event fetch unavailable")
		}
		items, next, err := f.FetchIssueEventsCursor(ctx, token, name, cursor)
		return activityPage{items, next}, err
	}, func(tx pgx.Tx, data any) error {
		var storedPhase, storedCursor string
		var storedGeneration int64
		err := tx.QueryRow(ctx, `SELECT phase,cursor,generation FROM activity_resources WHERE repository_id=$1 AND resource=$2 FOR UPDATE`, repoID, resource).Scan(&storedPhase, &storedCursor, &storedGeneration)
		if errors.Is(err, pgx.ErrNoRows) {
			return errRace
		}
		if err != nil {
			return err
		}
		if phase != storedPhase || cursor != storedCursor || generation != storedGeneration {
			return errRace
		}
		page := data.(activityPage)
		ids := []int64{}
		if resource == "releases" {
			for _, v := range page.items.([]githubapp.Release) {
				if v.Draft || v.PublishedAt == nil {
					continue
				}
				if v.ID <= 0 || v.CreatedAt.IsZero() || v.PublishedAt.IsZero() {
					return errors.New("invalid release")
				}
				aid, login, kind, err := evidenceActor(v.Author)
				if err != nil {
					return err
				}
				ids = append(ids, v.ID)
				var updated any
				if !v.UpdatedAt.IsZero() {
					updated = v.UpdatedAt
				}
				_, err = tx.Exec(ctx, `INSERT INTO activity_releases(source_id,repository_id,name,tag_name,body,html_url,created_at,updated_at,published_at,observed_at,prerelease,author_id,author_login,author_type) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT(source_id) DO UPDATE SET name=EXCLUDED.name,tag_name=EXCLUDED.tag_name,body=EXCLUDED.body,html_url=EXCLUDED.html_url,updated_at=EXCLUDED.updated_at,published_at=EXCLUDED.published_at,observed_at=EXCLUDED.observed_at,prerelease=EXCLUDED.prerelease,author_id=EXCLUDED.author_id,author_login=EXCLUDED.author_login,author_type=EXCLUDED.author_type WHERE activity_releases.repository_id=EXCLUDED.repository_id AND activity_releases.updated_at IS NOT NULL AND EXCLUDED.updated_at IS NOT NULL AND activity_releases.updated_at<EXCLUDED.updated_at`, v.ID, repoID, v.Name, v.TagName, v.Body, v.HTMLURL, v.CreatedAt, updated, v.PublishedAt, started, v.Prerelease, aid, login, kind)
				if err != nil {
					return err
				}
			}
		} else {
			for _, v := range page.items.([]githubapp.IssueEvent) {
				if !supportedIssueEvent(v.Event) {
					continue
				}
				if v.ID <= 0 || v.Issue == nil || v.Issue.ID <= 0 || v.Issue.Number <= 0 || v.CreatedAt.IsZero() {
					return errors.New("invalid issue event")
				}
				aid, login, kind, err := evidenceActor(v.Actor)
				if err != nil {
					return err
				}
				sid, slogin, skind, err := evidenceActor(v.Assignee)
				if err != nil {
					return err
				}
				var label any
				if v.Label != nil {
					label = v.Label.Name
				}
				ids = append(ids, v.ID)
				_, err = tx.Exec(ctx, `INSERT INTO activity_issue_events(source_id,repository_id,issue_id,issue_number,event,url,created_at,observed_at,actor_id,actor_login,actor_type,assignee_id,assignee_login,assignee_type,label) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(source_id) DO NOTHING`, v.ID, repoID, v.Issue.ID, v.Issue.Number, v.Event, v.URL, v.CreatedAt, started, aid, login, kind, sid, slogin, skind, label)
				if err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE activity_resources SET scan_started_at=COALESCE(scan_started_at,$3),current_scan=ARRAY(SELECT DISTINCT x FROM unnest(current_scan || $4::bigint[]) AS x ORDER BY x),last_error_code=NULL WHERE repository_id=$1 AND resource=$2`, repoID, resource, started, ids)
		if err != nil {
			return err
		}
		if page.next != "" {
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET page=page+1,cursor=$3 WHERE repository_id=$1 AND resource=$2`, repoID, resource, page.next)
			return err
		}
		if phase == "backfill" {
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase='catchup',page=1,cursor='',scan_started_at=NULL,current_scan='{}',previous_scan=NULL WHERE repository_id=$1 AND resource=$2`, repoID, resource)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase=CASE WHEN previous_scan=current_scan THEN 'current' ELSE 'catchup' END,watermark=CASE WHEN previous_scan=current_scan THEN scan_started_at ELSE watermark END,last_success_at=CASE WHEN previous_scan=current_scan THEN $3 ELSE last_success_at END,previous_scan=current_scan,current_scan='{}',scan_started_at=NULL,page=1,cursor='' WHERE repository_id=$1 AND resource=$2`, repoID, resource, d.Clock.Now())
		return err
	})
	if err != nil {
		return err
	}
	return &jobs.Defer{RetryAt: d.Clock.Now(), Reason: "continue"}
}
