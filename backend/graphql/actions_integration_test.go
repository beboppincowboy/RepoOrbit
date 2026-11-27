//go:build integration

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func TestActionsGraphQLDemoMode(t *testing.T) {
	ctx := context.Background()
	url, pool := testutil.Database(t)
	if err := platform.SeedDemo(ctx, url); err != nil {
		t.Fatal(err)
	}
	srv := Server(&Resolver{Demo: true, DemoPool: pool, Queries: db.New(pool)})
	exec := func(query string, vars map[string]any) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Body.String()
	}

	// orbit/example (id=1) has Deployments; orbit/launchpad (id=2) has zero Deployments (AE4).
	raw1 := exec(repositoryActionsQuery, map[string]any{"id": "1", "days": 30})
	if strings.Contains(raw1, `"errors"`) || !strings.Contains(raw1, `"deploymentUnavailable":false`) || !strings.Contains(raw1, `"production"`) {
		t.Fatalf("demo repo 1 actions: %s", raw1)
	}
	raw2 := exec(repositoryActionsQuery, map[string]any{"id": "2", "days": 30})
	if strings.Contains(raw2, `"errors"`) || !strings.Contains(raw2, `"deploymentUnavailable":true`) || !strings.Contains(raw2, `"Deploy"`) {
		t.Fatalf("demo repo 2 actions (AE4): %s", raw2)
	}

	// Run detail for Integration Tests failing run (1002 on repo 1).
	runRaw := exec(workflowRunQuery, map[string]any{"id": "1", "runId": "1002"})
	if strings.Contains(runRaw, `"errors"`) || !strings.Contains(runRaw, `"Integration Tests"`) || !strings.Contains(runRaw, `"integration"`) {
		t.Fatalf("demo workflowRun: %s", runRaw)
	}

	// Issue delivery for Board Task #2 on repo 2 (orbit/launchpad): linked PR #103, failed Integration Tests job, DeploymentUnavailable=true (AE1).
	delivRaw := exec(issueDeliveryQuery, map[string]any{"id": "2", "issueNumber": 2})
	if strings.Contains(delivRaw, `"errors"`) || !strings.Contains(delivRaw, `"deploymentUnavailable":true`) ||
		!strings.Contains(delivRaw, `"Integration Tests"`) || !strings.Contains(delivRaw, `"failingJobName":"integration"`) {
		t.Fatalf("demo issueDelivery (AE1): %s", delivRaw)
	}

	// Cross-repository overview surfaces failingWorkflows, buildSuccessRate, and failed_workflow_run NeedsAttention.
	crossRaw := exec(crossOverviewQuery, map[string]any{"days": 30})
	if strings.Contains(crossRaw, `"errors"`) || !strings.Contains(crossRaw, `"failed_workflow_run"`) || !strings.Contains(crossRaw, `"buildSuccessRate"`) {
		t.Fatalf("demo crossRepositoryOverview: %s", crossRaw)
	}
}

func TestActionsGraphQLAccessGuards(t *testing.T) {
	ctx := context.Background()
	h := newEvidenceHarness(t)
	pool, gh, run := h.pool, h.gh, h.run
	alice, bob := h.signIn("alice"), h.signIn("bob")
	aliceID := h.userID(ctx, "alice")

	repoID := gh.AddRepo("alice", "cicd-guarded", true)
	gh.Install("alice", 19, repoID)
	if err := h.repos.Connect(ctx, aliceID, repoID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark)
		 SELECT $1, res, now() - interval '90 days', 'current', now()
		 FROM unnest(ARRAY['issues','pull_requests','actions','deployments']) AS res`,
		`INSERT INTO workflows (repository_id, source_id, name, path, state, html_url, updated_at)
		 VALUES ($1, 10, 'CI', '.github/workflows/ci.yml', 'active', 'https://github.com/alice/cicd-guarded/actions/workflows/ci.yml', now())`,
		`INSERT INTO workflow_runs (repository_id, source_id, workflow_id, workflow_name, run_number, event, status, conclusion, head_branch, head_sha, html_url, created_at, run_started_at, updated_at)
		 VALUES ($1, 901, 10, 'CI', 1, 'push', 'completed', 'success', 'main', 'abc', 'https://github.com/alice/cicd-guarded/actions/runs/901', now() - interval '1 hour', now() - interval '59 minutes', now() - interval '55 minutes')`,
	} {
		if _, err := pool.Exec(ctx, stmt, repoID); err != nil {
			t.Fatal(err)
		}
	}

	// Authorized owner sees repositoryActions and workflowRun.
	qAct := fmt.Sprintf(`query { repositoryActions(id: %q, days: 30) { buildSuccessRate { numerator denominator } deploymentUnavailable } }`, fmt.Sprint(repoID))
	if got := run(alice, qAct); strings.Contains(got, `"errors"`) || !strings.Contains(got, `"numerator":1`) {
		t.Fatalf("alice repositoryActions: %s", got)
	}
	qRun := fmt.Sprintf(`query { workflowRun(repositoryId: %q, runId: "901") { run { workflowName } } }`, fmt.Sprint(repoID))
	if got := run(alice, qRun); strings.Contains(got, `"errors"`) || !strings.Contains(got, `"CI"`) {
		t.Fatalf("alice workflowRun: %s", got)
	}
	qDeliv := fmt.Sprintf(`query { issueDelivery(repositoryId: %q, issueNumber: 1) { issueNumber deploymentUnavailable } }`, fmt.Sprint(repoID))
	if got := run(alice, qDeliv); strings.Contains(got, `"errors"`) || !strings.Contains(got, `"issueNumber":1`) {
		t.Fatalf("alice issueDelivery: %s", got)
	}

	// Denied / unselected user (bob) receives null without errors.
	if got := run(bob, qAct); strings.Contains(got, `"errors"`) || !strings.Contains(got, `"repositoryActions":null`) {
		t.Fatalf("bob repositoryActions should be null: %s", got)
	}
	if got := run(bob, qRun); strings.Contains(got, `"errors"`) || !strings.Contains(got, `"workflowRun":null`) {
		t.Fatalf("bob workflowRun should be null: %s", got)
	}
	if got := run(bob, qDeliv); strings.Contains(got, `"errors"`) || !strings.Contains(got, `"issueDelivery":null`) {
		t.Fatalf("bob issueDelivery should be null: %s", got)
	}

	// Unverified access (rate-limited followed public repo) returns ACCESS_UNVERIFIED.
	pub := gh.AddRepo("carol", "cicd-followed", false)
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, 'carol/cicd-followed', '', false, false)`, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'starred', now())`, aliceID, pub); err != nil {
		t.Fatal(err)
	}
	h.limitRepo.Store(pub)
	defer h.limitRepo.Store(0)
	qPubAct := fmt.Sprintf(`query { repositoryActions(id: %q, days: 30) { deploymentUnavailable } }`, fmt.Sprint(pub))
	qPubRun := fmt.Sprintf(`query { workflowRun(repositoryId: %q, runId: "901") { run { workflowName } } }`, fmt.Sprint(pub))
	qPubDeliv := fmt.Sprintf(`query { issueDelivery(repositoryId: %q, issueNumber: 1) { issueNumber } }`, fmt.Sprint(pub))
	if got := run(alice, qPubAct); !strings.Contains(got, `"ACCESS_UNVERIFIED"`) {
		t.Fatalf("unverified repositoryActions: %s", got)
	}
	if got := run(alice, qPubRun); !strings.Contains(got, `"ACCESS_UNVERIFIED"`) {
		t.Fatalf("unverified workflowRun: %s", got)
	}
	if got := run(alice, qPubDeliv); !strings.Contains(got, `"ACCESS_UNVERIFIED"`) {
		t.Fatalf("unverified issueDelivery: %s", got)
	}
}
