//go:build integration

package ingestion_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

func TestDeliveryIngestionFencesAndPermissionRecovery(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	id := e.gh.AddRepo("alice", "delivery", false)
	e.store(uid, id, false)
	signalActivity(t, e, id, true)
	now := time.Now().UTC().Truncate(time.Second)
	run := githubapp.WorkflowRun{ID: 2, WorkflowID: 1, Name: "CI", RunNumber: 1, Event: "push", Status: "completed", Conclusion: "failure", HeadSHA: "abc", CreatedAt: now, UpdatedAt: now}
	run.PullRequests = []githubapp.WorkflowRunPullRequest{{Number: 42}, {Number: 43}}
	run.PullRequests[1].Base.Repo.ID = id + 1 // another repository's pull request is not stored
	e.gh.SetWorkflows(id, githubapp.Workflow{ID: 1, Name: "CI", Path: "ci.yml", State: "active", HTMLURL: "url", UpdatedAt: now})
	e.gh.SetWorkflowRuns(id, run, githubapp.WorkflowRun{ID: 99, WorkflowID: 1, CreatedAt: now.Add(-100 * 24 * time.Hour), UpdatedAt: now})
	e.gh.SetWorkflowJobs(id, 2, githubapp.WorkflowJob{ID: 3, RunID: 2, Name: "test", Status: "completed", Conclusion: "failure", CreatedAt: now})
	e.gh.SetDeployments(id, githubapp.Deployment{ID: 4, Environment: "production", SHA: "abc", Ref: "main", CreatedAt: now, UpdatedAt: now})
	e.gh.SetDeploymentStatuses(id, 4, githubapp.DeploymentStatus{ID: 5, State: "success", Description: "ok", EnvironmentURL: "https://example.com", CreatedAt: now, UpdatedAt: now})
	job := jobs.Job{Key: fmt.Sprintf("repo:%d", id)}
	e.gh.SetDeliveryEndpointDenied(id, "/actions/runs", true)
	if _, ok := e.deps.SyncActions(ctx, job, e.fence()).(*jobs.Defer); !ok {
		t.Fatal("workflow page did not advance to runs")
	}
	if err := e.deps.SyncActions(ctx, job, lostFence{}); !errors.Is(err, jobs.ErrLostClaim) {
		t.Fatalf("lost claim: %v", err)
	}
	var code *string
	if err := e.pool.QueryRow(ctx, `SELECT last_error_code FROM activity_resources WHERE repository_id=$1 AND resource='actions'`, id).Scan(&code); err != nil || code != nil {
		t.Fatalf("lost claim wrote code=%v err=%v", code, err)
	}
	var once sync.Once
	fence := txFence{pool: e.pool, before: func() {
		once.Do(func() {
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = accessfence.BumpAccess(ctx, tx, accessfence.ScopeRepository, id); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}}
	if err := e.deps.SyncActions(ctx, job, fence); err == nil {
		t.Fatal("access change accepted permission result")
	}
	if err := e.pool.QueryRow(ctx, `SELECT last_error_code FROM activity_resources WHERE repository_id=$1 AND resource='actions'`, id).Scan(&code); err != nil || code == nil || *code != ingestion.CodeAccessMoved {
		t.Fatalf("want the access race recorded, not the stale permission result: %v %v", code, err)
	}
	if err := e.deps.SyncActions(ctx, job, e.fence()); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT last_error_code FROM activity_resources WHERE repository_id=$1 AND resource='actions'`, id).Scan(&code); err != nil || code == nil || *code != "permission_denied" {
		t.Fatalf("code=%v err=%v", code, err)
	}
	e.gh.SetDeliveryPermissionDenied(id, false, true)
	if err := e.deps.SyncDeployments(ctx, job, e.fence()); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT last_error_code FROM activity_resources WHERE repository_id=$1 AND resource='deployments'`, id).Scan(&code); err != nil || code == nil || *code != "permission_denied" {
		t.Fatalf("deployment permission code=%v err=%v", code, err)
	}
	drain(t, func() error { return e.deps.SyncIssues(ctx, job, e.fence()) })
	drain(t, func() error { return e.deps.SyncPullRequests(ctx, job, e.fence()) })
	e.gh.SetDeliveryPermissionDenied(id, false, false)
	e.gh.SetDeliveryEndpointDenied(id, "/actions/runs", false)
	signalActivity(t, e, id, false)
	drain(t, func() error { return e.deps.SyncActions(ctx, job, e.fence()) })
	drain(t, func() error { return e.deps.SyncDeployments(ctx, job, e.fence()) })
	var workflows, runs, jobsCount, deployments int
	var prs []int64
	var state, url string
	err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflows WHERE repository_id=$1),(SELECT count(*) FROM workflow_runs WHERE repository_id=$1),(SELECT count(*) FROM workflow_jobs WHERE repository_id=$1),(SELECT count(*) FROM deployments WHERE repository_id=$1),(SELECT pull_request_numbers FROM workflow_runs WHERE repository_id=$1 AND source_id=2),(SELECT state FROM deployments WHERE repository_id=$1 AND source_id=4),(SELECT html_url FROM deployments WHERE repository_id=$1 AND source_id=4)`, id).Scan(&workflows, &runs, &jobsCount, &deployments, &prs, &state, &url)
	if err != nil || workflows != 1 || runs != 1 || jobsCount != 1 || deployments != 1 || len(prs) != 1 || prs[0] != 42 || state != "success" || url != "https://example.com" {
		t.Fatalf("workflows=%d runs=%d jobs=%d deployments=%d prs=%v state=%s url=%s err=%v", workflows, runs, jobsCount, deployments, prs, state, url, err)
	}
	if err = e.pool.QueryRow(ctx, `SELECT last_error_code FROM activity_resources WHERE repository_id=$1 AND resource='actions'`, id).Scan(&code); err != nil || code != nil {
		t.Fatalf("permission not cleared: %v %v", code, err)
	}
	e.gh.SetWorkflowJobs(id, 2)
	signalActivity(t, e, id, false)
	drain(t, func() error { return e.deps.SyncActions(ctx, job, e.fence()) })
	if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflow_runs WHERE repository_id=$1),(SELECT count(*) FROM workflow_jobs WHERE repository_id=$1)`, id).Scan(&runs, &jobsCount); err != nil || runs != 1 || jobsCount != 0 {
		t.Fatalf("deleted job reconciliation runs=%d jobs=%d err=%v", runs, jobsCount, err)
	}
	e.gh.SetWorkflows(id)
	e.gh.SetWorkflowRuns(id)
	e.gh.SetDeployments(id)
	signalActivity(t, e, id, false)
	drain(t, func() error { return e.deps.SyncActions(ctx, job, e.fence()) })
	drain(t, func() error { return e.deps.SyncDeployments(ctx, job, e.fence()) })
	if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflows WHERE repository_id=$1)+(SELECT count(*) FROM workflow_runs WHERE repository_id=$1)+(SELECT count(*) FROM workflow_jobs WHERE repository_id=$1)+(SELECT count(*) FROM deployments WHERE repository_id=$1)`, id).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("deleted delivery records remain=%d err=%v", runs, err)
	}
}

func TestIssueTimelineAndPullRequestHeadAreStored(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	id := e.gh.AddRepo("alice", "links", false)
	e.store(uid, id, false)
	signalActivity(t, e, id, true)
	now := time.Now().UTC().Truncate(time.Second)
	actor := fake.IssueActor{ID: 1, Login: "alice", Type: "User"}
	e.gh.SetIssues(id, fake.IssueFixture{ID: 1, Number: 2, Title: "issue", State: "open", HTMLURL: "url", CreatedAt: now, UpdatedAt: now, User: actor})
	pr := fake.PullRequestFixture{ID: 3, Number: 7, Title: "pr", State: "open", HTMLURL: "url", CreatedAt: now, UpdatedAt: now, User: actor}
	pr.Head.SHA = "head-sha"
	e.gh.SetPullRequests(id, pr)
	link := fake.TimelineEventFixture{Event: "cross-referenced", CreatedAt: now}
	link.Source.Issue.Number = 7
	link.Source.Issue.URL = "https://api.github.com/repos/alice/links/issues/7"
	link.Source.Issue.PullRequest = &struct{}{}
	e.gh.SetIssueTimeline(id, 2, link)
	job := jobs.Job{Key: fmt.Sprintf("repo:%d", id)}
	drain(t, func() error { return e.deps.SyncIssues(ctx, job, e.fence()) })
	drain(t, func() error { return e.deps.SyncPullRequests(ctx, job, e.fence()) })
	var sha string
	var number int64
	if err := e.pool.QueryRow(ctx, `SELECT p.head_sha,l.pull_request_number FROM activity_pull_requests p JOIN issue_pull_request_links l ON l.repository_id=p.repository_id AND l.pull_request_number=p.number WHERE p.repository_id=$1 AND l.issue_number=2`, id).Scan(&sha, &number); err != nil || sha != "head-sha" || number != 7 {
		t.Fatalf("sha=%q number=%d err=%v", sha, number, err)
	}
	// A timeline that fails for one issue (e.g. deleted) keeps its stored links and does not block the page.
	e.deps.GitHub = missingTimeline{e.deps.GitHub.(*githubapp.Client)}
	signalActivity(t, e, id, false)
	drain(t, func() error { return e.deps.SyncIssues(ctx, job, e.fence()) })
	var links int
	var phase string
	if err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM issue_pull_request_links WHERE repository_id=$1 AND issue_number=2),phase FROM activity_resources WHERE repository_id=$1 AND resource='issues'`, id).Scan(&links, &phase); err != nil || links != 1 || phase != "current" {
		t.Fatalf("links=%d phase=%q err=%v", links, phase, err)
	}
}

type missingTimeline struct{ *githubapp.Client }

func (missingTimeline) FetchIssueTimelinePRLinks(context.Context, string, string, int64) ([]githubapp.IssuePRLink, error) {
	return nil, &githubapp.APIError{Status: 404}
}

func TestDeliveryPaginationReconcilesDeletedRecords(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	id := e.gh.AddRepo("alice", "pages", false)
	e.store(uid, id, false)
	signalActivity(t, e, id, true)
	now := time.Now().UTC().Truncate(time.Second)
	workflows := make([]githubapp.Workflow, 101)
	for i := range workflows {
		workflows[i] = githubapp.Workflow{ID: int64(i + 1), Name: "CI", UpdatedAt: now}
	}
	runs := make([]githubapp.WorkflowRun, 101)
	for i := range runs {
		runs[i] = githubapp.WorkflowRun{ID: int64(i + 1), WorkflowID: 1, CreatedAt: now, UpdatedAt: now}
	}
	e.gh.SetWorkflows(id, workflows...)
	e.gh.SetWorkflowRuns(id, runs...)
	jobPages := make([]githubapp.WorkflowJob, 101)
	for i := range jobPages {
		jobPages[i] = githubapp.WorkflowJob{ID: int64(i + 1), RunID: 1, Name: "test", CreatedAt: now}
	}
	e.gh.SetWorkflowJobs(id, 1, jobPages...)
	job := jobs.Job{Key: fmt.Sprintf("repo:%d", id)}
	drainPages := func() {
		t.Helper()
		for i := 0; i < 30; i++ {
			err := e.deps.SyncActions(ctx, job, e.fence())
			if err == nil {
				return
			}
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
		t.Fatal("paginated stream did not settle")
	}
	drainPages()
	var workflowCount, runCount, jobCount int
	check := func(wantWorkflows, wantRuns, wantJobs int) {
		t.Helper()
		if err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflows WHERE repository_id=$1),(SELECT count(*) FROM workflow_runs WHERE repository_id=$1),(SELECT count(*) FROM workflow_jobs WHERE repository_id=$1)`, id).Scan(&workflowCount, &runCount, &jobCount); err != nil || workflowCount != wantWorkflows || runCount != wantRuns || jobCount != wantJobs {
			t.Fatalf("workflows=%d runs=%d jobs=%d err=%v", workflowCount, runCount, jobCount, err)
		}
	}
	check(101, 101, 101)
	e.gh.SetWorkflows(id, workflows[5:]...)
	e.gh.SetWorkflowRuns(id, runs[5:]...)
	signalActivity(t, e, id, false)
	drainPages()
	check(96, 96, 0)
}
