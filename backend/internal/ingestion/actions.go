package ingestion

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

type deliveryFetcher interface {
	FetchWorkflows(context.Context, string, string, string) ([]githubapp.Workflow, string, error)
	FetchWorkflowRuns(context.Context, string, string, string) ([]githubapp.WorkflowRun, string, error)
	FetchWorkflowJobs(context.Context, string, string, int64, string) ([]githubapp.WorkflowJob, string, error)
	FetchDeployments(context.Context, string, string, string) ([]githubapp.Deployment, string, error)
	FetchDeploymentStatuses(context.Context, string, string, int64) ([]githubapp.DeploymentStatus, error)
}

type deliveryPage struct {
	workflows        []githubapp.Workflow
	runs             []githubapp.WorkflowRun
	jobs             map[int64][]githubapp.WorkflowJob
	deployments      []githubapp.Deployment
	statuses         map[int64][]githubapp.DeploymentStatus
	next             string
	permissionDenied bool
}

func (d *Deps) SyncActions(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
	return d.syncDelivery(ctx, job, fence, "actions")
}
func (d *Deps) SyncDeployments(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
	return d.syncDelivery(ctx, job, fence, "deployments")
}

func (d *Deps) syncDelivery(ctx context.Context, job jobs.Job, fence jobs.ClaimFence, resource string) error {
	repoID, err := parseRepoKey(job.Key)
	if err != nil {
		return jobs.Permanent(err)
	}
	var phase, cursor string
	var generation int64
	var boundary time.Time
	err = d.Pool.QueryRow(ctx, `SELECT phase,cursor,generation,boundary FROM activity_resources WHERE repository_id=$1 AND resource=$2`, repoID, resource).Scan(&phase, &cursor, &generation, &boundary)
	if errors.Is(err, pgx.ErrNoRows) || phase == "current" {
		return nil
	}
	if err != nil {
		return err
	}
	fetcher, ok := d.GitHub.(deliveryFetcher)
	if !ok {
		return jobs.Permanent(errors.New("delivery fetch unavailable"))
	}
	stage, query := "deployments", cursor
	if resource == "actions" {
		stage = "workflows"
		if strings.HasPrefix(cursor, "r:") {
			stage = "runs"
			query = strings.TrimPrefix(cursor, "r:")
		} else {
			query = strings.TrimPrefix(cursor, "w:")
		}
	}
	started := d.Clock.Now()
	committed, denied := false, false
	err = d.withEvidencePage(ctx, job, fence, repoID, func(token string, repo githubapp.Repo) (any, error) {
		p := deliveryPage{}
		var fetchErr error
		switch stage {
		case "workflows":
			p.workflows, p.next, fetchErr = fetcher.FetchWorkflows(ctx, token, repo.FullName, query)
		case "runs":
			if query == "" {
				query = "per_page=10"
			}
			p.runs, p.next, fetchErr = fetcher.FetchWorkflowRuns(ctx, token, repo.FullName, query)
			p.jobs = make(map[int64][]githubapp.WorkflowJob)
			if fetchErr == nil {
				for _, run := range p.runs {
					if run.CreatedAt.Before(boundary) {
						continue
					}
					jobCursor := ""
					seen := map[string]bool{}
					for {
						var items []githubapp.WorkflowJob
						var next string
						items, next, fetchErr = fetcher.FetchWorkflowJobs(ctx, token, repo.FullName, run.ID, jobCursor)
						if fetchErr != nil {
							break
						}
						p.jobs[run.ID] = append(p.jobs[run.ID], items...)
						if next == "" {
							break
						}
						if seen[next] {
							fetchErr = errors.New("cyclic job pagination")
							break
						}
						seen[next] = true
						jobCursor = next
					}
					if fetchErr != nil {
						break
					}
				}
			}
		case "deployments":
			p.deployments, p.next, fetchErr = fetcher.FetchDeployments(ctx, token, repo.FullName, query)
			p.statuses = make(map[int64][]githubapp.DeploymentStatus)
			if fetchErr == nil {
				for _, v := range p.deployments {
					if v.CreatedAt.Before(boundary) {
						continue
					}
					p.statuses[v.ID], fetchErr = fetcher.FetchDeploymentStatuses(ctx, token, repo.FullName, v.ID)
					if fetchErr != nil {
						break
					}
				}
			}
		}
		if fetchErr == nil && p.next != "" && p.next == query {
			fetchErr = errors.New("cyclic delivery pagination")
		}
		if errors.Is(fetchErr, githubapp.ErrPermissionDenied) {
			return deliveryPage{permissionDenied: true}, nil
		}
		return p, fetchErr
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
		if storedPhase != phase || storedCursor != cursor || storedGeneration != generation {
			return errRace
		}
		p := data.(deliveryPage)
		committed = true
		if p.permissionDenied {
			denied = true
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase='current',page=1,cursor='',scan_started_at=NULL,previous_scan=NULL,current_scan='{}',watermark=NULL,last_error_code=$3 WHERE repository_id=$1 AND resource=$2`, repoID, resource, CodePermissionDenied)
			return err
		}
		ids := []int64{}
		switch stage {
		case "workflows":
			for _, v := range p.workflows {
				if v.ID <= 0 || v.UpdatedAt.IsZero() {
					return errors.New("invalid workflow")
				}
				ids = append(ids, v.ID)
				_, err = tx.Exec(ctx, `INSERT INTO workflows(repository_id,source_id,name,path,state,html_url,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(repository_id,source_id) DO UPDATE SET name=excluded.name,path=excluded.path,state=excluded.state,html_url=excluded.html_url,updated_at=excluded.updated_at WHERE workflows.updated_at<=excluded.updated_at`, repoID, v.ID, v.Name, v.Path, v.State, v.HTMLURL, v.UpdatedAt)
				if err != nil {
					return err
				}
			}
		case "runs":
			for _, v := range p.runs {
				if v.ID <= 0 || v.WorkflowID <= 0 || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
					return errors.New("invalid workflow run")
				}
				if v.CreatedAt.Before(boundary) {
					continue
				}
				ids = append(ids, v.ID)
				numbers := []int64{}
				for _, pr := range v.PullRequests {
					if pr.Number <= 0 {
						return errors.New("invalid run pull request")
					}
					numbers = append(numbers, pr.Number)
				}
				_, err = tx.Exec(ctx, `INSERT INTO workflow_runs(repository_id,source_id,workflow_id,workflow_name,run_number,event,status,conclusion,head_branch,head_sha,html_url,created_at,run_started_at,updated_at,pull_request_numbers) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(repository_id,source_id) DO UPDATE SET workflow_id=excluded.workflow_id,workflow_name=excluded.workflow_name,run_number=excluded.run_number,event=excluded.event,status=excluded.status,conclusion=excluded.conclusion,head_branch=excluded.head_branch,head_sha=excluded.head_sha,html_url=excluded.html_url,run_started_at=excluded.run_started_at,updated_at=excluded.updated_at,pull_request_numbers=excluded.pull_request_numbers WHERE workflow_runs.updated_at<=excluded.updated_at`, repoID, v.ID, v.WorkflowID, v.Name, v.RunNumber, v.Event, v.Status, v.Conclusion, v.HeadBranch, v.HeadSHA, v.HTMLURL, v.CreatedAt, v.RunStartedAt, v.UpdatedAt, numbers)
				if err != nil {
					return err
				}
				for _, j := range p.jobs[v.ID] {
					if j.ID <= 0 || j.RunID != v.ID || j.CreatedAt.IsZero() {
						return errors.New("invalid workflow job")
					}
					// Negative IDs let the checkpoint compare job and run listings without collisions.
					ids = append(ids, -j.ID)
					_, err = tx.Exec(ctx, `INSERT INTO workflow_jobs(repository_id,source_id,run_id,name,status,conclusion,html_url,created_at,started_at,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(repository_id,source_id) DO UPDATE SET run_id=excluded.run_id,name=excluded.name,status=excluded.status,conclusion=excluded.conclusion,html_url=excluded.html_url,started_at=excluded.started_at,completed_at=excluded.completed_at`, repoID, j.ID, j.RunID, j.Name, j.Status, j.Conclusion, j.HTMLURL, j.CreatedAt, j.StartedAt, j.CompletedAt)
					if err != nil {
						return err
					}
				}
			}
		case "deployments":
			for _, v := range p.deployments {
				if v.ID <= 0 || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
					return errors.New("invalid deployment")
				}
				if v.CreatedAt.Before(boundary) {
					continue
				}
				ids = append(ids, v.ID)
				var latest *githubapp.DeploymentStatus
				for i := range p.statuses[v.ID] {
					s := &p.statuses[v.ID][i]
					if s.ID <= 0 || s.State == "" || s.UpdatedAt.IsZero() {
						return errors.New("invalid deployment status")
					}
					if latest == nil || s.CreatedAt.After(latest.CreatedAt) || (s.CreatedAt.Equal(latest.CreatedAt) && s.ID > latest.ID) {
						latest = s
					}
				}
				state, description, url, updated := "", "", "", v.UpdatedAt
				if latest != nil {
					state, description, url = latest.State, latest.Description, latest.EnvironmentURL
					if url == "" {
						url = latest.TargetURL
					}
					if latest.UpdatedAt.After(updated) {
						updated = latest.UpdatedAt
					}
				}
				_, err = tx.Exec(ctx, `INSERT INTO deployments(repository_id,source_id,environment,sha,ref,state,description,html_url,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(repository_id,source_id) DO UPDATE SET environment=excluded.environment,sha=excluded.sha,ref=excluded.ref,state=excluded.state,description=excluded.description,html_url=excluded.html_url,updated_at=excluded.updated_at WHERE deployments.updated_at<=excluded.updated_at`, repoID, v.ID, v.Environment, v.SHA, v.Ref, state, description, url, v.CreatedAt, updated)
				if err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE activity_resources SET scan_started_at=COALESCE(scan_started_at,$3),current_scan=ARRAY(SELECT DISTINCT x FROM unnest(current_scan || $4::bigint[]) x ORDER BY x),last_error_code=NULL WHERE repository_id=$1 AND resource=$2`, repoID, resource, started, ids)
		if err != nil {
			return err
		}
		if p.next != "" {
			next := p.next
			if resource == "actions" {
				if stage == "workflows" {
					next = "w:" + next
				} else {
					next = "r:" + next
				}
			}
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET page=page+1,cursor=$3 WHERE repository_id=$1 AND resource=$2`, repoID, resource, next)
			return err
		}
		var stable bool
		err = tx.QueryRow(ctx, `SELECT ($3::boolean OR previous_scan=current_scan) IS TRUE FROM activity_resources WHERE repository_id=$1 AND resource=$2`, repoID, resource, stage != "runs" && cursor == "").Scan(&stable)
		if err != nil {
			return err
		}
		if !stable {
			next := ""
			if resource == "actions" && stage == "runs" {
				next = "r:"
			}
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET previous_scan=current_scan,current_scan='{}',page=1,cursor=$3 WHERE repository_id=$1 AND resource=$2`, repoID, resource, next)
			return err
		}
		switch stage {
		case "workflows":
			_, err = tx.Exec(ctx, `DELETE FROM workflows WHERE repository_id=$1 AND source_id NOT IN (SELECT unnest(current_scan) FROM activity_resources WHERE repository_id=$1 AND resource='actions')`, repoID)
		case "runs":
			_, err = tx.Exec(ctx, `DELETE FROM workflow_jobs WHERE repository_id=$1 AND run_id IN (SELECT source_id FROM workflow_runs WHERE repository_id=$1 AND created_at >= $2) AND -source_id NOT IN (SELECT unnest(current_scan) FROM activity_resources WHERE repository_id=$1 AND resource='actions')`, repoID, boundary)
			if err == nil {
				_, err = tx.Exec(ctx, `DELETE FROM workflow_runs WHERE repository_id=$1 AND created_at >= $2 AND source_id NOT IN (SELECT unnest(current_scan) FROM activity_resources WHERE repository_id=$1 AND resource='actions')`, repoID, boundary)
			}
		case "deployments":
			_, err = tx.Exec(ctx, `DELETE FROM deployments WHERE repository_id=$1 AND created_at >= $2 AND source_id NOT IN (SELECT unnest(current_scan) FROM activity_resources WHERE repository_id=$1 AND resource='deployments')`, repoID, boundary)
		}
		if err != nil {
			return err
		}
		if stage == "workflows" {
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET cursor='r:',page=1,previous_scan=NULL,current_scan='{}' WHERE repository_id=$1 AND resource=$2`, repoID, resource)
			return err
		}
		if phase == "backfill" {
			_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase='catchup',cursor='',page=1,previous_scan=NULL,current_scan='{}' WHERE repository_id=$1 AND resource=$2`, repoID, resource)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE activity_resources SET phase='current',watermark=scan_started_at,last_success_at=$3,cursor='',page=1,previous_scan=NULL,current_scan='{}',scan_started_at=NULL WHERE repository_id=$1 AND resource=$2`, repoID, resource, d.Clock.Now())
		return err
	})
	if err != nil || !committed || denied {
		return err
	}
	return &jobs.Defer{RetryAt: d.Clock.Now(), Reason: "continue"}
}
