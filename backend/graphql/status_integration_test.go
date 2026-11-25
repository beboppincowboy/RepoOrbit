//go:build integration

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func TestEvidenceCoverageRequiresParentsAndChildren(t *testing.T) {
	ctx := context.Background()
	h, rec, runVars := newTracedGraphQLHarness(t)
	alice := h.signIn("alice")
	aliceID := h.userID(ctx, "alice")
	repoID := h.gh.AddRepo("alice", "evidence", false)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, 'alice/evidence', '', false, false)`, []any{repoID}},
		{`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($2, $1, 'owned', now())`, []any{repoID, aliceID}},
		{`INSERT INTO activity_resources(repository_id, resource, boundary, phase, watermark) VALUES
			($1, 'issues', now(), 'current', now()),
			($1, 'pull_requests', now(), 'catchup', NULL),
			($1, 'comments', now(), 'backfill', NULL)`, []any{repoID}},
		{`INSERT INTO activity_children(repository_id, resource, parent_id, parent_number, complete, last_error_code, last_success_at) VALUES
			($1, 'reviews', 100, 1, false, 'rate_limited', now())`, []any{repoID}},
	} {
		if _, err := h.pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}

	const q = `query RepoActivity($id: ID!) {
		repository(id: $id) {
			activity {
				reviews { complete pendingChildren failureCode lastSuccessAt }
				comments { complete pendingChildren failureCode }
			}
		}
	}`
	vars := map[string]any{"id": fmt.Sprintf("%d", repoID)}
	fetch := func() (reviews, comments struct {
		Complete        bool    `json:"complete"`
		PendingChildren int     `json:"pendingChildren"`
		FailureCode     *string `json:"failureCode"`
		LastSuccessAt   *string `json:"lastSuccessAt"`
	}) {
		t.Helper()
		raw := runVars(alice, q, vars)
		_ = rec.Stop()
		var out struct {
			Data struct {
				Repository struct {
					Activity struct {
						Reviews struct {
							Complete        bool    `json:"complete"`
							PendingChildren int     `json:"pendingChildren"`
							FailureCode     *string `json:"failureCode"`
							LastSuccessAt   *string `json:"lastSuccessAt"`
						} `json:"reviews"`
						Comments struct {
							Complete        bool    `json:"complete"`
							PendingChildren int     `json:"pendingChildren"`
							FailureCode     *string `json:"failureCode"`
							LastSuccessAt   *string `json:"lastSuccessAt"`
						} `json:"comments"`
					} `json:"activity"`
				} `json:"repository"`
			} `json:"data"`
			Errors []any `json:"errors"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out.Errors) > 0 {
			t.Fatalf("unexpected GraphQL response: %v %s", err, raw)
		}
		return out.Data.Repository.Activity.Reviews, out.Data.Repository.Activity.Comments
	}

	reviews, comments := fetch()
	if reviews.Complete || comments.Complete || reviews.PendingChildren != 1 || reviews.FailureCode == nil || *reviews.FailureCode != "rate_limited" || reviews.LastSuccessAt == nil {
		t.Fatalf("incomplete reviews=%+v comments=%+v", reviews, comments)
	}

	if _, err := h.pool.Exec(ctx, `UPDATE activity_resources SET phase='current', watermark=now() WHERE repository_id=$1 AND resource IN ('pull_requests','comments')`, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE activity_children SET complete=true WHERE repository_id=$1`, repoID); err != nil {
		t.Fatal(err)
	}
	reviews, comments = fetch()
	if !reviews.Complete || !comments.Complete || reviews.FailureCode == nil || *reviews.FailureCode != "rate_limited" {
		t.Fatalf("completed evidence should retain later error: reviews=%+v comments=%+v", reviews, comments)
	}

	if _, err := h.pool.Exec(ctx, `UPDATE activity_resources SET phase='catchup', last_error_code='rate_limited' WHERE repository_id=$1 AND resource='comments'`, repoID); err != nil {
		t.Fatal(err)
	}
	reviews, comments = fetch()
	if !reviews.Complete || comments.Complete || comments.PendingChildren != 0 || comments.FailureCode == nil || *comments.FailureCode != "rate_limited" {
		t.Fatalf("independent coverage: reviews=%+v comments=%+v", reviews, comments)
	}
}

func countMetadataStatusSQL(queries []string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, "repository_sync") || strings.Contains(q, "repo_metadata_refresh") {
			n++
		}
	}
	return n
}

func countActivityStatusSQL(queries []string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, "FROM activity_resources") || strings.Contains(q, "FROM activity_children") ||
			(strings.Contains(q, "archived") && strings.Contains(q, "FROM repositories")) {
			n++
		}
	}
	return n
}

func isRepoAccessBoundarySQL(q string) bool {
	return strings.Contains(q, "access_fences") || strings.Contains(q, "PruneInstallations")
}

func countNonBoundaryWriteSQL(queries []string) int {
	n := 0
	for _, q := range queries {
		if isRepoAccessBoundarySQL(q) {
			continue
		}
		upper := strings.ToUpper(q)
		for _, line := range strings.Split(upper, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			if strings.HasPrefix(trimmed, "INSERT ") || strings.HasPrefix(trimmed, "UPDATE ") || strings.HasPrefix(trimmed, "DELETE ") {
				n++
			}
			break
		}
	}
	return n
}

func warmAccessCache(runVars func(c *http.Client, query string, vars map[string]any) string, rec *testutil.SQLRecorder, c *http.Client, query string, vars map[string]any) {
	_ = runVars(c, query, vars)
	_ = rec.Stop()
}

func newTracedGraphQLHarness(t *testing.T) (*evidenceHarness, *testutil.SQLRecorder, func(c *http.Client, query string, vars map[string]any) string) {
	t.Helper()
	h := newEvidenceHarness(t)
	rec := &testutil.SQLRecorder{}
	tracedPool := testutil.OpenTracedPool(t, h.dbURL, rec)
	tracedQ := db.New(tracedPool)
	tracedAuth := &auth.Service{Pool: tracedPool, Queries: tracedQ, GitHub: h.client, Key: bytes.Repeat([]byte{1}, 32)}
	tracedRepos := &repositories.Service{Pool: tracedPool, Queries: tracedQ, GitHub: h.client, TokenSource: tracedAuth, TTL: time.Minute}
	h.mux.Handle("/graphql-traced", h.ah.Protect(Server(&Resolver{Queries: tracedQ, RepoAccess: tracedRepos})))

	runVars := func(c *http.Client, query string, vars map[string]any) string {
		t.Helper()
		rec.Start()
		defer rec.Stop()
		return h.runAtVars(c, "/graphql-traced", query, vars)
	}
	return h, rec, runVars
}

func TestSelectedRepositoriesMetadataOnlySkipsActivitySQL(t *testing.T) {
	ctx := context.Background()
	h, rec, runVars := newTracedGraphQLHarness(t)
	alice := h.signIn("alice")
	aliceID := h.userID(ctx, "alice")

	var ids []int64
	for i := 0; i < 21; i++ {
		id := h.gh.AddRepo("alice", fmt.Sprintf("repo-%02d", i), false)
		ids = append(ids, id)
		if _, err := h.pool.Exec(ctx, `INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, $2, 'desc', false, false)`, id, fmt.Sprintf("alice/repo-%02d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.pool.Exec(ctx, `INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', now())`, aliceID, id); err != nil {
			t.Fatal(err)
		}
		if _, err := h.pool.Exec(ctx, `INSERT INTO repository_sync (repository_id, last_success_at) VALUES ($1, now())`, id); err != nil {
			t.Fatal(err)
		}
	}

	const metadataPageQuery = `query SelectedRepositories($after: ID) {
		selectedRepositories(first: 20, after: $after) {
			nodes {
				id fullName description syncState
				metadataStatus { state lastSuccessAt nextAttemptAt paused failureCode followSuspended }
			}
			endCursor hasNextPage withheld retryAt
		}
	}`
	warmAccessCache(runVars, rec, alice, metadataPageQuery, map[string]any{"after": nil})

	raw := runVars(alice, metadataPageQuery, map[string]any{"after": nil})
	queries := rec.Stop()

	var resp struct {
		Data struct {
			SelectedRepositories struct {
				Nodes []struct {
					ID             string
					FullName       string
					MetadataStatus *struct {
						State         string
						LastSuccessAt *string
					}
				}
				EndCursor   *string
				HasNextPage bool
				Withheld    int
			}
		}
		Errors []any
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	sr := resp.Data.SelectedRepositories
	if len(resp.Errors) != 0 || len(sr.Nodes) != 20 || !sr.HasNextPage || sr.Withheld != 0 || sr.EndCursor == nil || *sr.EndCursor != fmt.Sprintf("%d", ids[19]) {
		t.Fatalf("unexpected 20-row metadata response: %s", raw)
	}
	if sr.Nodes[0].MetadataStatus == nil || sr.Nodes[0].MetadataStatus.LastSuccessAt == nil {
		t.Fatalf("missing metadataStatus on first node: %+v", sr.Nodes[0])
	}
	if got := countActivityStatusSQL(queries); got != 0 {
		t.Fatalf("expected 0 activity-status queries, got %d: %v", got, queries)
	}
	if got := countMetadataStatusSQL(queries); got != 1 {
		t.Fatalf("expected 1 metadata-status query, got %d: %v", got, queries)
	}
	if got := countNonBoundaryWriteSQL(queries); got != 0 {
		t.Fatalf("expected 0 DB write statements outside RepoAccess boundary during GraphQL query, got %d: %v", got, queries)
	}
}

func TestGraphQLStatusDemandDrivenSelection(t *testing.T) {
	ctx := context.Background()
	h, rec, runVars := newTracedGraphQLHarness(t)
	alice := h.signIn("alice")
	aliceID := h.userID(ctx, "alice")

	repoID := h.gh.AddRepo("alice", "demand", false)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO repositories (id, name, description, is_private, is_demo, archived) VALUES ($1, 'alice/demand', 'd', false, false, true)`, []any{repoID}},
		{`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($2, $1, 'owned', now())`, []any{repoID, aliceID}},
		{`INSERT INTO repository_sync (repository_id, last_success_at) VALUES ($1, now())`, []any{repoID}},
		{`INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark, last_success_at, last_error_code) VALUES
			($1, 'issues', now()-interval '90 days', 'current', now(), now(), NULL),
			($1, 'pull_requests', now()-interval '90 days', 'current', now(), now(), $2),
			($1, 'comments', now()-interval '90 days', 'catchup', NULL, NULL, 'rate_limited')`, []any{repoID, ingestion.CodePullRequestsDisabled}},
		{`INSERT INTO activity_children (repository_id, resource, parent_id, parent_number, complete, last_error_code) VALUES
			($1, 'reviews', 50, 1, false, 'rate_limited')`, []any{repoID}},
	} {
		if _, err := h.pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	warmAccessCache(runVars, rec, alice, `{ selectedRepositories(first: 1) { nodes { id } } }`, nil)

	idStr := fmt.Sprintf("%d", repoID)

	t.Run("selectedRepositories_neither", func(t *testing.T) {
		raw := runVars(alice, `{ selectedRepositories(first: 1) { nodes { id fullName } } }`, nil)
		qs := rec.Stop()
		if !strings.Contains(raw, `"fullName":"alice/demand"`) || strings.Contains(raw, `"errors"`) {
			t.Fatalf("unexpected response: %s", raw)
		}
		if countMetadataStatusSQL(qs) != 0 || countActivityStatusSQL(qs) != 0 {
			t.Fatalf("unexpected status queries for neither: %v", qs)
		}
	})

	t.Run("selectedRepositories_activity_only", func(t *testing.T) {
		raw := runVars(alice, `{ selectedRepositories(first: 1) { nodes { id activity { archived pullRequests { phase } reviews { complete pendingChildren } comments { complete failureCode } } } } }`, nil)
		qs := rec.Stop()
		for _, want := range []string{`"archived":true`, `"pullRequests":{"phase":"disabled"}`, `"pendingChildren":1`, `"comments":{"complete":false,"failureCode":"rate_limited"}`} {
			if !strings.Contains(raw, want) {
				t.Fatalf("activity-only missing %s: %s", want, raw)
			}
		}
		if got := countMetadataStatusSQL(qs); got != 0 {
			t.Fatalf("expected 0 metadata-status queries for activity-only, got %d", got)
		}
		if countActivityStatusSQL(qs) == 0 {
			t.Fatalf("expected activity-status SQL for activity-only")
		}
	})

	t.Run("selectedRepositories_aliased_nodes_union", func(t *testing.T) {
		raw := runVars(alice, `{ selectedRepositories(first: 1) { a: nodes { metadataStatus { state } } b: nodes { activity { archived } } } }`, nil)
		qs := rec.Stop()
		if !strings.Contains(raw, `"metadataStatus":{"state":"not-scheduled"}`) || !strings.Contains(raw, `"archived":true`) {
			t.Fatalf("aliased nodes missing fields: %s", raw)
		}
		if countMetadataStatusSQL(qs) == 0 || countActivityStatusSQL(qs) == 0 {
			t.Fatalf("expected both status loaders for aliased nodes union")
		}
	})

	t.Run("selectedRepositories_merged_fragment_on_nodes", func(t *testing.T) {
		raw := runVars(alice, `{ selectedRepositories(first: 1) { nodes { id } ... on RepositoryConnection { nodes { activity { archived } } } } }`, nil)
		qs := rec.Stop()
		if !strings.Contains(raw, `"archived":true`) {
			t.Fatalf("merged fragment nodes missing activity: %s", raw)
		}
		if countMetadataStatusSQL(qs) != 0 || countActivityStatusSQL(qs) == 0 {
			t.Fatalf("expected activity only for merged fragment on nodes")
		}
	})

	t.Run("repository_named_fragment_and_skip_directive", func(t *testing.T) {
		q := `query Repo($id: ID!, $skipActivity: Boolean!) {
			repository(id: $id) {
				...RepoMeta
				... on Repository {
					activity @skip(if: $skipActivity) { archived }
				}
			}
		}
		fragment RepoMeta on Repository { metadataStatus { state } }`

		rawSkipped := runVars(alice, q, map[string]any{"id": idStr, "skipActivity": true})
		qsSkipped := rec.Stop()
		if !strings.Contains(rawSkipped, `"metadataStatus":{"state":"not-scheduled"}`) || strings.Contains(rawSkipped, `"archived"`) {
			t.Fatalf("skipped activity response: %s", rawSkipped)
		}
		if countMetadataStatusSQL(qsSkipped) == 0 || countActivityStatusSQL(qsSkipped) != 0 {
			t.Fatalf("expected metadata only when activity skipped: %v", qsSkipped)
		}

		rawIncluded := runVars(alice, q, map[string]any{"id": idStr, "skipActivity": false})
		qsIncluded := rec.Stop()
		if !strings.Contains(rawIncluded, `"metadataStatus":{"state":"not-scheduled"}`) || !strings.Contains(rawIncluded, `"archived":true`) {
			t.Fatalf("included activity response: %s", rawIncluded)
		}
		if countMetadataStatusSQL(qsIncluded) == 0 || countActivityStatusSQL(qsIncluded) == 0 {
			t.Fatalf("expected both loaders when activity included")
		}
	})

	t.Run("repositoryOverview_never_loads_metadata_and_gates_activity", func(t *testing.T) {
		rawNoActivity := runVars(alice, fmt.Sprintf(`{ repositoryOverview(id: "%s", days: 7) { fullName } }`, idStr), nil)
		qsNoActivity := rec.Stop()
		if !strings.Contains(rawNoActivity, `"fullName":"alice/demand"`) {
			t.Fatalf("overview without activity: %s", rawNoActivity)
		}
		if countMetadataStatusSQL(qsNoActivity) != 0 || countActivityStatusSQL(qsNoActivity) != 0 {
			t.Fatalf("overview without activity ran unexpected status SQL: %v", qsNoActivity)
		}

		rawWithActivity := runVars(alice, fmt.Sprintf(`{ repositoryOverview(id: "%s", days: 7) { fullName activity { archived pullRequests { phase } } } }`, idStr), nil)
		qsWithActivity := rec.Stop()
		if !strings.Contains(rawWithActivity, `"archived":true`) || !strings.Contains(rawWithActivity, `"phase":"disabled"`) {
			t.Fatalf("overview with activity: %s", rawWithActivity)
		}
		if got := countMetadataStatusSQL(qsWithActivity); got != 0 {
			t.Fatalf("overview with activity must never run metadata-status SQL, got %d", got)
		}
		if countActivityStatusSQL(qsWithActivity) == 0 {
			t.Fatalf("overview with activity did not run activity-status SQL")
		}
	})
}

func TestSelectedRepositoriesActivityBatchesAcrossRepos(t *testing.T) {
	ctx := context.Background()
	h, rec, runVars := newTracedGraphQLHarness(t)
	alice := h.signIn("alice")
	aliceID := h.userID(ctx, "alice")

	repoA := h.gh.AddRepo("alice", "alpha", false)
	repoB := h.gh.AddRepo("alice", "beta", false)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO repositories (id, name, description, is_private, is_demo, archived) VALUES
			($1, 'alice/alpha', 'a', false, false, true),
			($2, 'alice/beta', 'b', false, false, false)`, []any{repoA, repoB}},
		{`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES
			($3, $1, 'owned', now()),
			($3, $2, 'owned', now())`, []any{repoA, repoB, aliceID}},
		{`INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark, last_success_at, last_error_code) VALUES
			($1, 'issues', now()-interval '90 days', 'current', now(), now(), 'token ghu_secret leaked'),
			($1, 'pull_requests', now()-interval '90 days', 'current', now(), now(), $3),
			($1, 'comments', now()-interval '90 days', 'catchup', NULL, NULL, 'rate_limited'),
			($2, 'issues', now()-interval '90 days', 'current', now(), now(), NULL),
			($2, 'pull_requests', now()-interval '90 days', 'current', now(), now(), NULL),
			($2, 'comments', now()-interval '90 days', 'current', now(), now(), NULL)`, []any{repoA, repoB, ingestion.CodePullRequestsDisabled}},
		{`INSERT INTO activity_children (repository_id, resource, parent_id, parent_number, complete, last_error_code) VALUES
			($1, 'reviews', 50, 1, false, 'rate_limited')`, []any{repoA}},
	} {
		if _, err := h.pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}

	const q = `query SelectedActivity($first: Int!) {
		selectedRepositories(first: $first) {
			nodes {
				id fullName
				activity {
					archived
					issues { phase failureCode }
					pullRequests { phase }
					reviews { complete pendingChildren failureCode }
					comments { complete failureCode }
				}
			}
		}
	}`
	warmAccessCache(runVars, rec, alice, q, map[string]any{"first": 2})

	_ = runVars(alice, q, map[string]any{"first": 1})
	qs1 := rec.Stop()
	count1 := countActivityStatusSQL(qs1)

	raw := runVars(alice, q, map[string]any{"first": 2})
	qs2 := rec.Stop()
	count2 := countActivityStatusSQL(qs2)
	if count1 == 0 || count2 != count1 {
		t.Fatalf("expected constant batched activity SQL count across 1 vs 2 repos, got first=1: %d (%v), first=2: %d (%v)", count1, qs1, count2, qs2)
	}
	var out struct {
		Data struct {
			SelectedRepositories struct {
				Nodes []struct {
					ID       string `json:"id"`
					FullName string `json:"fullName"`
					Activity struct {
						Archived bool `json:"archived"`
						Issues   *struct {
							Phase       string  `json:"phase"`
							FailureCode *string `json:"failureCode"`
						} `json:"issues"`
						PullRequests *struct {
							Phase string `json:"phase"`
						} `json:"pullRequests"`
						Reviews *struct {
							Complete        bool    `json:"complete"`
							PendingChildren int     `json:"pendingChildren"`
							FailureCode     *string `json:"failureCode"`
						} `json:"reviews"`
						Comments *struct {
							Complete    bool    `json:"complete"`
							FailureCode *string `json:"failureCode"`
						} `json:"comments"`
					} `json:"activity"`
				} `json:"nodes"`
			} `json:"selectedRepositories"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out.Errors) > 0 {
		t.Fatalf("unexpected GraphQL response: %v %s", err, raw)
	}
	if len(out.Data.SelectedRepositories.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d: %s", len(out.Data.SelectedRepositories.Nodes), raw)
	}
	byName := map[string]int{}
	for i, n := range out.Data.SelectedRepositories.Nodes {
		byName[n.FullName] = i
	}
	a := out.Data.SelectedRepositories.Nodes[byName["alice/alpha"]].Activity
	if !a.Archived || a.Issues == nil || a.Issues.FailureCode == nil || *a.Issues.FailureCode != "unknown" ||
		a.PullRequests == nil || a.PullRequests.Phase != "disabled" ||
		a.Reviews == nil || a.Reviews.Complete || a.Reviews.PendingChildren != 1 || a.Reviews.FailureCode == nil || *a.Reviews.FailureCode != "rate_limited" ||
		a.Comments == nil || a.Comments.Complete || a.Comments.FailureCode == nil || *a.Comments.FailureCode != "rate_limited" {
		t.Fatalf("unexpected alpha activity: %+v", a)
	}
	b := out.Data.SelectedRepositories.Nodes[byName["alice/beta"]].Activity
	if b.Archived || b.Issues == nil || b.Issues.FailureCode != nil ||
		b.PullRequests == nil || b.PullRequests.Phase != "current" ||
		b.Reviews == nil || !b.Reviews.Complete || b.Reviews.PendingChildren != 0 || b.Reviews.FailureCode != nil ||
		b.Comments == nil || !b.Comments.Complete || b.Comments.FailureCode != nil {
		t.Fatalf("unexpected beta activity: %+v", b)
	}
}
