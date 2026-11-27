//go:build integration

package analytics

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func TestRepositoryActionsMetricsAndAcceptanceExamples(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	// Repo 100: AE2 (3 succeeded, 1 failed, 2 in-progress => 75.0% success rate, 2 running jobs),
	// plus duration p50/p95 and queue wait p50 verification, plus Deployments.
	// Repo 101: AE4 (passing "Deploy" workflow, zero deployments => DeploymentUnavailable = true).
	// Repo 102: AE3 (actions permission_denied => ActionsCoverage.Reason = permission_denied).
	for _, stmt := range []string{
		`INSERT INTO repositories (id, name, description, is_private) VALUES
		 (100, 'test/ci-main', '', false),
		 (101, 'test/no-deploy', '', false),
		 (102, 'test/no-actions-perm', '', false)`,
		`INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark, last_error_code) VALUES
		 (100, 'issues', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', ''),
		 (100, 'pull_requests', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', ''),
		 (100, 'actions', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', ''),
		 (100, 'deployments', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', ''),
		 (101, 'issues', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', ''),
		 (101, 'pull_requests', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', ''),
		 (101, 'actions', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', ''),
		 (101, 'deployments', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', ''),
		 (102, 'actions', '2026-08-01 00:00Z', 'current', NULL, 'permission_denied'),
		 (102, 'deployments', '2026-08-01 00:00Z', 'current', '2026-11-17 11:00Z', '')`,
		`INSERT INTO workflows (repository_id, source_id, name, path, state, html_url, updated_at) VALUES
		 (100, 1, 'CI', '.github/workflows/ci.yml', 'active', 'https://github.com/test/ci-main/actions/workflows/ci.yml', '2026-11-17 00:00Z'),
		 (100, 2, 'Integration Tests', '.github/workflows/integration.yml', 'active', 'https://github.com/test/ci-main/actions/workflows/integration.yml', '2026-11-17 00:00Z'),
		 (101, 1, 'Deploy', '.github/workflows/deploy.yml', 'active', 'https://github.com/test/no-deploy/actions/workflows/deploy.yml', '2026-11-17 00:00Z')`,
		// Repo 100 runs:
		// 1001: success, queue=60s, duration=100s
		// 1002: success, queue=120s, duration=200s
		// 1003: success, queue=180s, duration=300s
		// 1004: failure (Integration Tests), queue=240s, duration=400s
		// 1005: in_progress, queue=300s
		// 1006: queued, run_started_at NULL
		`INSERT INTO workflow_runs (repository_id, source_id, workflow_id, workflow_name, run_number, event, status, conclusion, head_branch, head_sha, html_url, created_at, run_started_at, updated_at, pull_request_numbers) VALUES
		 (100, 1001, 1, 'CI', 1, 'push', 'completed', 'success', 'main', 'sha1', 'https://github.com/test/ci-main/actions/runs/1001', '2026-11-14 10:00:00Z', '2026-11-14 10:01:00Z', '2026-11-14 10:02:40Z', '{}'),
		 (100, 1002, 1, 'CI', 2, 'push', 'completed', 'success', 'main', 'sha2', 'https://github.com/test/ci-main/actions/runs/1002', '2026-11-15 10:00:00Z', '2026-11-15 10:02:00Z', '2026-11-15 10:05:20Z', '{}'),
		 (100, 1003, 1, 'CI', 3, 'push', 'completed', 'success', 'main', 'sha3', 'https://github.com/test/ci-main/actions/runs/1003', '2026-11-16 10:00:00Z', '2026-11-16 10:03:00Z', '2026-11-16 10:08:00Z', '{}'),
		 (100, 1004, 2, 'Integration Tests', 4, 'pull_request', 'completed', 'failure', 'feat', 'sha4', 'https://github.com/test/ci-main/actions/runs/1004', '2026-11-16 12:00:00Z', '2026-11-16 12:04:00Z', '2026-11-16 12:10:40Z', ARRAY[42]::bigint[]),
		 (100, 1005, 1, 'CI', 5, 'push', 'in_progress', '', 'main', 'sha5', 'https://github.com/test/ci-main/actions/runs/1005', '2026-11-17 10:00:00Z', '2026-11-17 10:05:00Z', '2026-11-17 10:06:00Z', '{}'),
		 (100, 1006, 1, 'CI', 6, 'push', 'queued', '', 'main', 'sha6', 'https://github.com/test/ci-main/actions/runs/1006', '2026-11-17 10:30:00Z', NULL, '2026-11-17 10:30:00Z', '{}'),
		 (101, 2001, 1, 'Deploy', 1, 'push', 'completed', 'success', 'main', 'sha-d1', 'https://github.com/test/no-deploy/actions/runs/2001', '2026-11-16 10:00:00Z', '2026-11-16 10:00:10Z', '2026-11-16 10:01:10Z', '{}')`,
		`INSERT INTO workflow_jobs (repository_id, source_id, run_id, name, status, conclusion, html_url, created_at, started_at, completed_at) VALUES
		 (100, 10041, 1004, 'integration', 'completed', 'failure', 'https://github.com/test/ci-main/actions/runs/1004/job/10041', '2026-11-16 12:00:00Z', '2026-11-16 12:04:00Z', '2026-11-16 12:10:40Z'),
		 (100, 10051, 1005, 'build', 'in_progress', '', 'https://github.com/test/ci-main/actions/runs/1005/job/10051', '2026-11-17 10:00:00Z', '2026-11-17 10:05:00Z', NULL),
		 (100, 10061, 1006, 'lint', 'queued', '', 'https://github.com/test/ci-main/actions/runs/1006/job/10061', '2026-11-17 10:30:00Z', NULL, NULL)`,
		`INSERT INTO deployments (repository_id, source_id, environment, sha, ref, state, description, html_url, created_at, updated_at) VALUES
		 (100, 5001, 'production', 'sha4', 'main', 'success', 'Prod deploy', 'https://github.com/test/ci-main/deployments', '2026-11-16 13:00:00Z', '2026-11-16 13:02:00Z')`,
		`INSERT INTO activity_issues (source_id, repository_id, number, title, state, html_url, created_at, updated_at, author_id, author_login, author_type) VALUES
		 (8001, 100, 7, 'Fix flaky test', 'open', 'https://github.com/test/ci-main/issues/7', '2026-11-10 00:00Z', '2026-11-16 00:00Z', 1, 'maya', 'User')`,
		`INSERT INTO activity_pull_requests (source_id, repository_id, number, title, state, html_url, created_at, updated_at, author_id, author_login, author_type, head_sha) VALUES
		 (8101, 100, 42, 'Fix flaky test PR', 'open', 'https://github.com/test/ci-main/pull/42', '2026-11-15 00:00Z', '2026-11-16 00:00Z', 1, 'maya', 'User', 'sha4')`,
		`INSERT INTO activity_reviews (source_id, repository_id, pull_request_id, state, body, html_url, submitted_at, author_id, author_login, author_type) VALUES
		 (8201, 100, 8101, 'APPROVED', '', 'https://github.com/test/ci-main/pull/42#pullrequestreview-8201', '2026-11-16 01:00Z', 2, 'sam', 'User')`,
		`INSERT INTO activity_children (repository_id, resource, parent_id, parent_number, complete, watermark) VALUES
		 (100, 'reviews', 8101, 42, true, '2026-11-17 11:00Z')`,
		`INSERT INTO issue_pull_request_links (repository_id, issue_number, pull_request_number, source, created_at) VALUES
		 (100, 7, 42, 'timeline', '2026-11-15 01:00Z')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	// AE2 + p50/p95 duration + queue wait + Deployments on repo 100
	act, err := LoadRepositoryActions(ctx, pool, 100, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if !act.ActionsCoverage.Covered || !act.DeploymentsCoverage.Covered {
		t.Fatalf("expected covered actions & deployments, got %+v %+v", act.ActionsCoverage, act.DeploymentsCoverage)
	}
	if act.BuildSuccessRate.Rate == nil || math.Abs(*act.BuildSuccessRate.Rate-0.75) > 1e-9 ||
		*act.BuildSuccessRate.Numerator != 3 || *act.BuildSuccessRate.Denominator != 4 {
		t.Fatalf("AE2 success rate: got %+v, want 3/4 (0.75)", act.BuildSuccessRate)
	}
	if *act.FailedJobs.Value != 1 || *act.RunningJobs.Value != 2 {
		t.Fatalf("jobs: failed=%v running=%v, want 1 and 2", act.FailedJobs.Value, act.RunningJobs.Value)
	}
	// Completed durations: [100, 200, 300, 400] => p50 = 250, p95 = floor(385) = 385
	if *act.DurationP50.Seconds != 250 || *act.DurationP95.Seconds != 385 || *act.DurationP50.SampleSize != 4 {
		t.Fatalf("durations: p50=%+v p95=%+v", act.DurationP50, act.DurationP95)
	}
	// Queue waits with run_started_at: [60, 120, 180, 240, 300] => p50 = 180
	if *act.QueueWaitP50.Seconds != 180 || *act.QueueWaitP50.SampleSize != 5 {
		t.Fatalf("queue wait: %+v", act.QueueWaitP50)
	}
	if len(act.FailuresByWorkflow) != 1 || act.FailuresByWorkflow[0].WorkflowName != "Integration Tests" || act.FailuresByWorkflow[0].FailedRuns != 1 {
		t.Fatalf("failures by workflow: %+v", act.FailuresByWorkflow)
	}
	if act.DeploymentUnavailable || len(act.Deployments) != 1 || act.Deployments[0].Environment != "production" {
		t.Fatalf("deployments: unavailable=%v items=%+v", act.DeploymentUnavailable, act.Deployments)
	}

	// AE4: passing "Deploy" workflow with zero deployments => DeploymentUnavailable = true
	noDep, err := LoadRepositoryActions(ctx, pool, 101, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if !noDep.DeploymentUnavailable || len(noDep.Deployments) != 0 {
		t.Fatalf("AE4: want DeploymentUnavailable=true and 0 deployments, got unavailable=%v len=%d", noDep.DeploymentUnavailable, len(noDep.Deployments))
	}

	// AE3: permission_denied on actions
	denied, err := LoadRepositoryActions(ctx, pool, 102, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if denied.ActionsCoverage.Covered || denied.ActionsCoverage.Reason == nil || *denied.ActionsCoverage.Reason != ReasonPermissionDenied {
		t.Fatalf("AE3: want permission_denied, got %+v", denied.ActionsCoverage)
	}

	// Run detail
	detail, err := LoadWorkflowRunDetail(ctx, pool, 100, 1004)
	if err != nil || detail == nil {
		t.Fatalf("LoadWorkflowRunDetail: err=%v detail=%+v", err, detail)
	}
	if detail.Run.WorkflowName != "Integration Tests" || len(detail.Jobs) != 1 || detail.Jobs[0].Name != "integration" {
		t.Fatalf("run detail mismatch: %+v", detail)
	}

	// AE1: IssueDelivery with confirmed PR, review state, failing check -> failing job log URL, and Deployment
	deliv, err := LoadIssueDelivery(ctx, pool, 100, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliv.PullRequests) != 1 {
		t.Fatalf("want 1 linked PR, got %+v", deliv.PullRequests)
	}
	pr := deliv.PullRequests[0]
	if pr.Number != 42 || pr.ReviewState == nil || *pr.ReviewState != "approved" {
		t.Fatalf("linked PR mismatch: %+v", pr)
	}
	if len(pr.Checks) != 1 || pr.Checks[0].FailingJobURL == nil || *pr.Checks[0].FailingJobURL != "https://github.com/test/ci-main/actions/runs/1004/job/10041" {
		t.Fatalf("checks mismatch: %+v", pr.Checks)
	}
	if len(pr.Deployments) != 1 || pr.Deployments[0].Environment != "production" {
		t.Fatalf("PR deployments mismatch: %+v", pr.Deployments)
	}

	// CrossOverview CI signals across repos 100 and 101 (both have issues, pull_requests, actions covered)
	cross, err := LoadCrossRepositoryOverview(ctx, pool, []int64{100, 101}, 1, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	// Repo 100 has 3/4 succeeded and 1 failing workflow; repo 101 has 1/1 succeeded and 0 failing workflows => 4/5 = 0.80, 1 failing workflow
	if !cross.FailingWorkflows.Coverage.Covered || *cross.FailingWorkflows.Value != 1 {
		t.Fatalf("cross failing workflows: %+v", cross.FailingWorkflows)
	}
	if !cross.BuildSuccessRate.Coverage.Covered || math.Abs(*cross.BuildSuccessRate.Rate-0.8) > 1e-9 ||
		*cross.BuildSuccessRate.Numerator != 4 || *cross.BuildSuccessRate.Denominator != 5 {
		t.Fatalf("cross build success rate: %+v", cross.BuildSuccessRate)
	}
	foundFailedRun := false
	for _, item := range cross.NeedsAttention {
		if item.Kind == "failed_workflow_run" && item.RunID != nil && *item.RunID == 1004 {
			foundFailedRun = true
		}
	}
	if !foundFailedRun {
		t.Fatalf("expected failed_workflow_run 1004 in NeedsAttention, got %+v", cross.NeedsAttention)
	}
}
