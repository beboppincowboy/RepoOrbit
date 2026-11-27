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

func (d *Deps) syncDelivery(ctx context.Context, job jobs.Job, fence jobs.ClaimFence, resource string) (result error) {
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
			_, err := tx.Exec(ctx, `UPDATE activity_resources SET last_error_code=$3 WHERE repository_id=$1 AND resource=$2`, repoID, resource, code)
			return err
		})
	}()
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
		p, fetchErr := fetchDeliveryPage(ctx, fetcher, token, repo.FullName, stage, query, boundary)
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
		ids, err := commitDeliveryStage(ctx, tx, repoID, stage, boundary, p)
		if err != nil {
			return err
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
		if err := reconcileDeliveryStage(ctx, tx, repoID, stage, boundary); err != nil {
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

func fetchDeliveryPage(ctx context.Context, fetcher deliveryFetcher, token, fullName, stage, query string, boundary time.Time) (deliveryPage, error) {
	var p deliveryPage
	var err error
	switch stage {
	case "workflows":
		p.workflows, p.next, err = fetcher.FetchWorkflows(ctx, token, fullName, query)
		return p, err
	case "runs":
		p.runs, p.next, err = fetcher.FetchWorkflowRuns(ctx, token, fullName, query)
		if err != nil {
			return p, err
		}
		// Newest first: once a page reaches past the boundary, later pages hold nothing to keep.
		if n := len(p.runs); n > 0 && p.runs[n-1].CreatedAt.Before(boundary) {
			p.next = ""
		}
		p.jobs, err = fetchWorkflowJobsForRuns(ctx, fetcher, token, fullName, p.runs, boundary)
		return p, err
	default:
		p.deployments, p.next, err = fetcher.FetchDeployments(ctx, token, fullName, query)
		if err != nil {
			return p, err
		}
		if n := len(p.deployments); n > 0 && p.deployments[n-1].CreatedAt.Before(boundary) {
			p.next = ""
		}
		p.statuses, err = fetchDeploymentStatusesForPage(ctx, fetcher, token, fullName, p.deployments, boundary)
		return p, err
	}
}

func fetchWorkflowJobsForRuns(ctx context.Context, fetcher deliveryFetcher, token, fullName string, runs []githubapp.WorkflowRun, boundary time.Time) (map[int64][]githubapp.WorkflowJob, error) {
	jobsByRun := make(map[int64][]githubapp.WorkflowJob)
	for _, run := range runs {
		if run.CreatedAt.Before(boundary) {
			continue
		}
		jobCursor := ""
		seen := map[string]bool{}
		for {
			items, next, err := fetcher.FetchWorkflowJobs(ctx, token, fullName, run.ID, jobCursor)
			if err != nil {
				return nil, err
			}
			jobsByRun[run.ID] = append(jobsByRun[run.ID], items...)
			if next == "" {
				break
			}
			if seen[next] {
				return nil, errors.New("cyclic job pagination")
			}
			seen[next] = true
			jobCursor = next
		}
	}
	return jobsByRun, nil
}

func fetchDeploymentStatusesForPage(ctx context.Context, fetcher deliveryFetcher, token, fullName string, deployments []githubapp.Deployment, boundary time.Time) (map[int64][]githubapp.DeploymentStatus, error) {
	statuses := make(map[int64][]githubapp.DeploymentStatus)
	for _, dep := range deployments {
		if dep.CreatedAt.Before(boundary) {
			continue
		}
		items, err := fetcher.FetchDeploymentStatuses(ctx, token, fullName, dep.ID)
		if err != nil {
			return nil, err
		}
		statuses[dep.ID] = items
	}
	return statuses, nil
}

func commitDeliveryStage(ctx context.Context, tx pgx.Tx, repoID int64, stage string, boundary time.Time, p deliveryPage) ([]int64, error) {
	switch stage {
	case "workflows":
		return commitWorkflows(ctx, tx, repoID, p.workflows)
	case "runs":
		return commitWorkflowRuns(ctx, tx, repoID, boundary, p.runs, p.jobs)
	default:
		return commitDeployments(ctx, tx, repoID, boundary, p.deployments, p.statuses)
	}
}

func commitWorkflows(ctx context.Context, tx pgx.Tx, repoID int64, workflows []githubapp.Workflow) ([]int64, error) {
	ids := make([]int64, 0, len(workflows))
	for _, v := range workflows {
		if v.ID <= 0 || v.UpdatedAt.IsZero() {
			return nil, errors.New("invalid workflow")
		}
		ids = append(ids, v.ID)
		_, err := tx.Exec(ctx, `INSERT INTO workflows(repository_id,source_id,name,path,state,html_url,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(repository_id,source_id) DO UPDATE SET name=excluded.name,path=excluded.path,state=excluded.state,html_url=excluded.html_url,updated_at=excluded.updated_at WHERE workflows.updated_at<=excluded.updated_at`, repoID, v.ID, v.Name, v.Path, v.State, v.HTMLURL, v.UpdatedAt)
		if err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func commitWorkflowRuns(ctx context.Context, tx pgx.Tx, repoID int64, boundary time.Time, runs []githubapp.WorkflowRun, jobsByRun map[int64][]githubapp.WorkflowJob) ([]int64, error) {
	ids := make([]int64, 0, len(runs))
	for _, v := range runs {
		if v.ID <= 0 || v.WorkflowID <= 0 || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
			return nil, errors.New("invalid workflow run")
		}
		if v.CreatedAt.Before(boundary) {
			continue
		}
		ids = append(ids, v.ID)
		for _, pr := range v.PullRequests {
			if pr.Number <= 0 {
				return nil, errors.New("invalid run pull request")
			}
		}
		numbers := v.PullRequestNumbers(repoID)
		_, err := tx.Exec(ctx, `INSERT INTO workflow_runs(repository_id,source_id,workflow_id,workflow_name,run_number,event,status,conclusion,head_branch,head_sha,html_url,created_at,run_started_at,updated_at,pull_request_numbers) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(repository_id,source_id) DO UPDATE SET workflow_id=excluded.workflow_id,workflow_name=excluded.workflow_name,run_number=excluded.run_number,event=excluded.event,status=excluded.status,conclusion=excluded.conclusion,head_branch=excluded.head_branch,head_sha=excluded.head_sha,html_url=excluded.html_url,run_started_at=excluded.run_started_at,updated_at=excluded.updated_at,pull_request_numbers=excluded.pull_request_numbers WHERE workflow_runs.updated_at<=excluded.updated_at`, repoID, v.ID, v.WorkflowID, v.Name, v.RunNumber, v.Event, v.Status, v.Conclusion, v.HeadBranch, v.HeadSHA, v.HTMLURL, v.CreatedAt, v.RunStartedAt, v.UpdatedAt, numbers)
		if err != nil {
			return nil, err
		}
		for _, j := range jobsByRun[v.ID] {
			if j.ID <= 0 || j.RunID != v.ID || j.CreatedAt.IsZero() {
				return nil, errors.New("invalid workflow job")
			}
			// Negative IDs let the checkpoint compare job and run listings without collisions.
			ids = append(ids, -j.ID)
			_, err := tx.Exec(ctx, `INSERT INTO workflow_jobs(repository_id,source_id,run_id,name,status,conclusion,html_url,created_at,started_at,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(repository_id,source_id) DO UPDATE SET run_id=excluded.run_id,name=excluded.name,status=excluded.status,conclusion=excluded.conclusion,html_url=excluded.html_url,started_at=excluded.started_at,completed_at=excluded.completed_at`, repoID, j.ID, j.RunID, j.Name, j.Status, j.Conclusion, j.HTMLURL, j.CreatedAt, j.StartedAt, j.CompletedAt)
			if err != nil {
				return nil, err
			}
		}
	}
	return ids, nil
}

func commitDeployments(ctx context.Context, tx pgx.Tx, repoID int64, boundary time.Time, deployments []githubapp.Deployment, statusesByDeployment map[int64][]githubapp.DeploymentStatus) ([]int64, error) {
	ids := make([]int64, 0, len(deployments))
	for _, v := range deployments {
		if v.ID <= 0 || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
			return nil, errors.New("invalid deployment")
		}
		if v.CreatedAt.Before(boundary) {
			continue
		}
		ids = append(ids, v.ID)
		state, description, url, updated, err := resolveDeploymentState(v.UpdatedAt, statusesByDeployment[v.ID])
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO deployments(repository_id,source_id,environment,sha,ref,state,description,html_url,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(repository_id,source_id) DO UPDATE SET environment=excluded.environment,sha=excluded.sha,ref=excluded.ref,state=excluded.state,description=excluded.description,html_url=excluded.html_url,updated_at=excluded.updated_at WHERE deployments.updated_at<=excluded.updated_at`, repoID, v.ID, v.Environment, v.SHA, v.Ref, state, description, url, v.CreatedAt, updated)
		if err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func resolveDeploymentState(updated time.Time, statuses []githubapp.DeploymentStatus) (state, description, htmlURL string, latestUpdated time.Time, err error) {
	var latest *githubapp.DeploymentStatus
	for i := range statuses {
		s := &statuses[i]
		if s.ID <= 0 || s.State == "" || s.UpdatedAt.IsZero() {
			return "", "", "", time.Time{}, errors.New("invalid deployment status")
		}
		if latest == nil || s.CreatedAt.After(latest.CreatedAt) || (s.CreatedAt.Equal(latest.CreatedAt) && s.ID > latest.ID) {
			latest = s
		}
	}
	if latest == nil {
		return "", "", "", updated, nil
	}
	htmlURL = latest.EnvironmentURL
	if htmlURL == "" {
		htmlURL = latest.TargetURL
	}
	if latest.UpdatedAt.After(updated) {
		updated = latest.UpdatedAt
	}
	return latest.State, latest.Description, htmlURL, updated, nil
}

func reconcileDeliveryStage(ctx context.Context, tx pgx.Tx, repoID int64, stage string, boundary time.Time) error {
	switch stage {
	case "workflows":
		_, err := tx.Exec(ctx, `DELETE FROM workflows WHERE repository_id=$1 AND source_id NOT IN (SELECT unnest(current_scan) FROM activity_resources WHERE repository_id=$1 AND resource='actions')`, repoID)
		return err
	case "runs":
		if _, err := tx.Exec(ctx, `DELETE FROM workflow_jobs WHERE repository_id=$1 AND run_id IN (SELECT source_id FROM workflow_runs WHERE repository_id=$1 AND created_at >= $2) AND -source_id NOT IN (SELECT unnest(current_scan) FROM activity_resources WHERE repository_id=$1 AND resource='actions')`, repoID, boundary); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM workflow_runs WHERE repository_id=$1 AND created_at >= $2 AND source_id NOT IN (SELECT unnest(current_scan) FROM activity_resources WHERE repository_id=$1 AND resource='actions')`, repoID, boundary)
		return err
	default:
		_, err := tx.Exec(ctx, `DELETE FROM deployments WHERE repository_id=$1 AND created_at >= $2 AND source_id NOT IN (SELECT unnest(current_scan) FROM activity_resources WHERE repository_id=$1 AND resource='deployments')`, repoID, boundary)
		return err
	}
}
