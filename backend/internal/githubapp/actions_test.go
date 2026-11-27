package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestActionsPagesAndPermissions(t *testing.T) {
	denied := false
	limited := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if denied {
			if limited {
				w.Header().Set("X-RateLimit-Remaining", "0")
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			w.Header().Set("Link", `</repos/a/b/actions/workflows?per_page=100&page=2>; rel="next"`)
			_, _ = w.Write([]byte(`{"total_count":1,"workflows":[{"id":1,"name":"CI","path":".github/workflows/ci.yml","state":"active","html_url":"url","updated_at":"2026-01-01T00:00:00Z"}]}`))
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":2,"workflow_id":1,"name":"CI","run_number":3,"event":"push","status":"completed","conclusion":"success","head_sha":"abc","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","pull_requests":[{"number":42,"base":{"repo":{"id":10}}},{"number":43,"base":{"repo":{"id":11}}}]}]}`))
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			_, _ = w.Write([]byte(`{"total_count":1,"jobs":[{"id":3,"run_id":2,"name":"test","status":"completed","conclusion":"success","created_at":"2026-01-01T00:00:00Z"}]}`))
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			_, _ = w.Write([]byte(`[{"id":6,"state":"success","description":"done","environment_url":"https://example.com","created_at":"2026-01-02T00:00:00Z","updated_at":"2026-01-03T00:00:00Z"}]`))
		default:
			_, _ = w.Write([]byte(`[{"id":5,"environment":"prod","sha":"abc","ref":"main","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`))
		}
	}))
	defer server.Close()
	c := New("", "", "", server.URL, "")
	ctx := context.Background()
	workflows, next, err := c.FetchWorkflows(ctx, "token", "a/b", "")
	if err != nil || len(workflows) != 1 || next == "" {
		t.Fatalf("workflows=%v next=%q err=%v", workflows, next, err)
	}
	runs, _, err := c.FetchWorkflowRuns(ctx, "token", "a/b", "")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	if got := runs[0].PullRequestNumbers(10); !slices.Equal(got, []int64{42}) {
		t.Fatalf("pull requests for repository 10 = %v, want the foreign base ignored", got)
	}
	jobs, _, err := c.FetchWorkflowJobs(ctx, "token", "a/b", 2, "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	deployments, _, err := c.FetchDeployments(ctx, "token", "a/b", "")
	if err != nil || len(deployments) != 1 {
		t.Fatalf("deployments=%v err=%v", deployments, err)
	}
	statuses, err := c.FetchDeploymentStatuses(ctx, "token", "a/b", 5)
	if err != nil || len(statuses) != 1 || statuses[0].EnvironmentURL != "https://example.com" {
		t.Fatalf("statuses=%v err=%v", statuses, err)
	}
	denied = true
	if _, _, err = c.FetchWorkflowRuns(ctx, "token", "a/b", ""); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("permission error=%v", err)
	}
	if _, _, err = c.FetchDeployments(ctx, "token", "a/b", ""); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("deployment permission error=%v", err)
	}
	limited = true
	_, _, err = c.FetchWorkflowRuns(ctx, "token", "a/b", "")
	var api *APIError
	if !errors.As(err, &api) || !api.RateLimited() || errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("rate limit error=%v", err)
	}
}

func TestIssueTimelineUsesOnlySameRepositoryPullRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"event":"cross-referenced","created_at":"2026-01-01T00:00:00Z","source":{"issue":{"number":7,"url":"https://api.github.com/repos/a/b/issues/7","pull_request":{}}}},{"event":"cross-referenced","created_at":"2026-01-01T00:00:00Z","source":{"issue":{"number":8,"url":"https://api.github.com/repos/other/repo/issues/8","pull_request":{}}}},{"event":"cross-referenced","created_at":"2026-01-01T00:00:00Z","source":{"issue":{"number":9,"url":"https://api.github.com/repos/a/b/issues/9"}}}]`))
	}))
	defer server.Close()
	links, err := New("", "", "", server.URL, "").FetchIssueTimelinePRLinks(context.Background(), "token", "a/b", 2)
	if err != nil || len(links) != 1 || links[0].Number != 7 {
		t.Fatalf("links=%+v err=%v", links, err)
	}
}
