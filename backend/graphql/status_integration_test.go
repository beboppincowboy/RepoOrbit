//go:build integration

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

// Statuses attach only to repositories the viewer selected, and carry a safe code, not raw error text.
func TestAttachMetadataStatus(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	for _, sql := range []string{
		`INSERT INTO users (id, github_id, login, access_token_enc) VALUES (1, 1, 'u', '\x00')`,
		`INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES (10, 'o/a', '', false, false), (11, 'o/b', '', false, false)`,
		`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES (1, 10, 'owned', now())`,
		`INSERT INTO repository_sync (repository_id, last_success_at, last_error_code) VALUES (10, now(), 'token ghu_secret leaked')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	r := &Resolver{Queries: db.New(pool)}
	nodes := []*model.Repository{{ID: "10"}, {ID: "11"}}
	if err := r.loadMetadataStatus(ctx, 1, nodes); err != nil {
		t.Fatal(err)
	}
	if nodes[1].MetadataStatus != nil {
		t.Fatal("unselected repository got a status")
	}
	s := nodes[0].MetadataStatus
	if s == nil || s.LastSuccessAt == nil || s.FailureCode == nil || *s.FailureCode != "unknown" || s.State != "not-scheduled" {
		t.Fatalf("%+v", s)
	}
	tx, _ := pool.Begin(ctx)
	defer tx.Rollback(ctx)
	if err := jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, "repo:10", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state='paused'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.loadMetadataStatus(ctx, 1, nodes[:1]); err != nil || !nodes[0].MetadataStatus.Paused {
		t.Fatalf("%v %+v", err, nodes[0].MetadataStatus)
	}
}

func TestEvidenceCoverageRequiresParentsAndChildren(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	_, err := pool.Exec(ctx, `INSERT INTO repositories (id,name,description) VALUES (10,'o/a','');
 INSERT INTO activity_resources(repository_id,resource,boundary,phase,watermark) VALUES (10,'issues',now(),'current',now()),(10,'pull_requests',now(),'catchup',NULL),(10,'comments',now(),'backfill',NULL);
 INSERT INTO activity_children(repository_id,resource,parent_id,parent_number,complete,last_error_code,last_success_at) VALUES (10,'reviews',100,1,false,'rate_limited',now());`)
	if err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Queries: db.New(pool), RepoAccess: &repositories.Service{Pool: pool}}
	a := &model.ActivityStatus{Issues: &model.ActivityResourceStatus{Phase: "current", CatchupThrough: ptr("now")}}
	if err = r.attachEvidenceStatus(ctx, 10, a); err != nil {
		t.Fatal(err)
	}
	if a.Reviews.Complete || a.Comments.Complete || a.Reviews.PendingChildren != 1 || a.Reviews.FailureCode == nil || *a.Reviews.FailureCode != "rate_limited" || a.Reviews.LastSuccessAt == nil {
		t.Fatalf("incomplete %+v", a)
	}
	_, err = pool.Exec(ctx, `UPDATE activity_resources SET phase='current',watermark=now() WHERE resource IN ('pull_requests','comments'); UPDATE activity_children SET complete=true WHERE repository_id=10`)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.attachEvidenceStatus(ctx, 10, a); err != nil {
		t.Fatal(err)
	}
	if !a.Reviews.Complete || !a.Comments.Complete || a.Reviews.FailureCode == nil {
		t.Fatalf("completed evidence should retain later error: %+v", a)
	}
	// Comments are their own stream: a comment catch-up leaves review coverage alone, and its failure is reported.
	if _, err = pool.Exec(ctx, `UPDATE activity_resources SET phase='catchup',last_error_code='rate_limited' WHERE resource='comments'`); err != nil {
		t.Fatal(err)
	}
	if err = r.attachEvidenceStatus(ctx, 10, a); err != nil {
		t.Fatal(err)
	}
	if !a.Reviews.Complete || a.Comments.Complete || a.Comments.PendingChildren != 0 || a.Comments.FailureCode == nil || *a.Comments.FailureCode != "rate_limited" {
		t.Fatalf("independent coverage: %+v", a)
	}
}
func ptr(s string) *string { return &s }

type sqlRecorder struct {
	mu      sync.Mutex
	enabled bool
	queries []string
}

func (r *sqlRecorder) start() {
	r.mu.Lock()
	r.queries = nil
	r.enabled = true
	r.mu.Unlock()
}

func (r *sqlRecorder) stop() []string {
	r.mu.Lock()
	r.enabled = false
	out := append([]string(nil), r.queries...)
	r.mu.Unlock()
	return out
}

func (r *sqlRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	if r.enabled {
		r.queries = append(r.queries, data.SQL)
	}
	r.mu.Unlock()
	return ctx
}

func (*sqlRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func countSQL(queries []string, substr string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, substr) {
			n++
		}
	}
	return n
}

func countNonBoundaryWriteSQL(queries []string) int {
	n := 0
	for _, q := range queries {
		// RepoAccess.Check owns installation pruning and access_fences initialization at the security boundary.
		if strings.Contains(q, "access_fences") || strings.Contains(q, "PruneInstallations") {
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

func newTracedGraphQLHarness(t *testing.T) (*evidenceHarness, *sqlRecorder, func(c *http.Client, query string, vars map[string]any) string) {
	t.Helper()
	ctx := context.Background()
	h := newEvidenceHarness(t)
	rec := &sqlRecorder{}
	cfg, err := pgxpool.ParseConfig(h.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = rec
	tracedPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracedPool.Close)
	tracedQ := db.New(tracedPool)
	tracedAuth := &auth.Service{Pool: tracedPool, Queries: tracedQ, GitHub: h.client, Key: bytes.Repeat([]byte{1}, 32)}
	tracedRepos := &repositories.Service{Pool: tracedPool, Queries: tracedQ, GitHub: h.client, TokenSource: tracedAuth, TTL: time.Minute}
	h.mux.Handle("/graphql-traced", h.ah.Protect(Server(&Resolver{Queries: tracedQ, RepoAccess: tracedRepos})))

	runVars := func(c *http.Client, query string, vars map[string]any) string {
		t.Helper()
		me, err := c.Get(h.api.URL + "/auth/me")
		if err != nil {
			t.Fatal(err)
		}
		var session struct {
			CSRFToken string `json:"csrfToken"`
		}
		if err := json.NewDecoder(me.Body).Decode(&session); err != nil {
			me.Body.Close()
			t.Fatal(err)
		}
		me.Body.Close()
		payload := map[string]any{"query": query}
		if vars != nil {
			payload["variables"] = vars
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest("POST", h.api.URL+"/graphql-traced", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://app.test")
		req.Header.Set("X-CSRF-Token", session.CSRFToken)
		rec.start()
		resp, err := c.Do(req)
		if err != nil {
			rec.stop()
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			rec.stop()
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
		return string(b)
	}
	return h, rec, runVars
}

// The 20-row metadata-only Selection request executes zero activity-status SQL queries and zero database writes.
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
	// Warm the per-viewer access cache so the traced run observes steady-state query execution.
	_ = runVars(alice, metadataPageQuery, map[string]any{"after": nil})
	_ = rec.stop()

	raw := runVars(alice, metadataPageQuery, map[string]any{"after": nil})
	queries := rec.stop()

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
	if got := countSQL(queries, "ListActivityStatuses"); got != 0 {
		t.Fatalf("expected 0 ListActivityStatuses queries, got %d", got)
	}
	if got := countSQL(queries, "FROM activity_resources"); got != 0 {
		t.Fatalf("expected 0 activity_resources queries, got %d", got)
	}
	if got := countSQL(queries, "FROM activity_children"); got != 0 {
		t.Fatalf("expected 0 activity_children queries, got %d", got)
	}
	if got := countSQL(queries, "SELECT archived FROM repositories"); got != 0 {
		t.Fatalf("expected 0 archived queries, got %d", got)
	}
	if got := countSQL(queries, "ListMetadataStatuses"); got != 1 {
		t.Fatalf("expected 1 ListMetadataStatuses query, got %d", got)
	}
	if got := countNonBoundaryWriteSQL(queries); got != 0 {
		t.Fatalf("expected 0 DB write statements outside RepoAccess boundary during GraphQL query, got %d: %v", got, queries)
	}
}

// Demand-driven GraphQL loading covers metadata-only, activity-only, both, neither, aliases, fragments,
// directives, and repositoryOverview without running unselected status queries.
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
	// Warm access cache.
	_ = runVars(alice, `{ selectedRepositories(first: 1) { nodes { id } } }`, nil)
	_ = rec.stop()

	idStr := fmt.Sprintf("%d", repoID)

	t.Run("selectedRepositories_neither", func(t *testing.T) {
		raw := runVars(alice, `{ selectedRepositories(first: 1) { nodes { id fullName } } }`, nil)
		qs := rec.stop()
		if !strings.Contains(raw, `"fullName":"alice/demand"`) || strings.Contains(raw, `"errors"`) {
			t.Fatalf("unexpected response: %s", raw)
		}
		if countSQL(qs, "ListMetadataStatuses") != 0 || countSQL(qs, "ListActivityStatuses") != 0 || countSQL(qs, "FROM activity_resources") != 0 {
			t.Fatalf("unexpected status queries for neither: %v", qs)
		}
	})

	t.Run("selectedRepositories_activity_only", func(t *testing.T) {
		raw := runVars(alice, `{ selectedRepositories(first: 1) { nodes { id activity { archived pullRequests { phase } reviews { complete pendingChildren } comments { complete failureCode } } } } }`, nil)
		qs := rec.stop()
		for _, want := range []string{`"archived":true`, `"pullRequests":{"phase":"disabled"}`, `"pendingChildren":1`, `"comments":{"complete":false,"failureCode":"rate_limited"}`} {
			if !strings.Contains(raw, want) {
				t.Fatalf("activity-only missing %s: %s", want, raw)
			}
		}
		if countSQL(qs, "ListMetadataStatuses") != 0 {
			t.Fatalf("expected 0 ListMetadataStatuses for activity-only, got %d", countSQL(qs, "ListMetadataStatuses"))
		}
		if countSQL(qs, "ListActivityStatuses") == 0 {
			t.Fatalf("expected ListActivityStatuses for activity-only")
		}
	})

	t.Run("selectedRepositories_aliased_nodes_union", func(t *testing.T) {
		raw := runVars(alice, `{ selectedRepositories(first: 1) { a: nodes { metadataStatus { state } } b: nodes { activity { archived } } } }`, nil)
		qs := rec.stop()
		if !strings.Contains(raw, `"metadataStatus":{"state":"not-scheduled"}`) || !strings.Contains(raw, `"archived":true`) {
			t.Fatalf("aliased nodes missing fields: %s", raw)
		}
		if countSQL(qs, "ListMetadataStatuses") == 0 || countSQL(qs, "ListActivityStatuses") == 0 {
			t.Fatalf("expected both status loaders for aliased nodes union")
		}
	})

	t.Run("selectedRepositories_merged_fragment_on_nodes", func(t *testing.T) {
		raw := runVars(alice, `{ selectedRepositories(first: 1) { nodes { id } ... on RepositoryConnection { nodes { activity { archived } } } } }`, nil)
		qs := rec.stop()
		if !strings.Contains(raw, `"archived":true`) {
			t.Fatalf("merged fragment nodes missing activity: %s", raw)
		}
		if countSQL(qs, "ListMetadataStatuses") != 0 || countSQL(qs, "ListActivityStatuses") == 0 {
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
		qsSkipped := rec.stop()
		if !strings.Contains(rawSkipped, `"metadataStatus":{"state":"not-scheduled"}`) || strings.Contains(rawSkipped, `"archived"`) {
			t.Fatalf("skipped activity response: %s", rawSkipped)
		}
		if countSQL(qsSkipped, "ListMetadataStatuses") == 0 || countSQL(qsSkipped, "ListActivityStatuses") != 0 || countSQL(qsSkipped, "FROM activity_resources") != 0 {
			t.Fatalf("expected metadata only when activity skipped: %v", qsSkipped)
		}

		rawIncluded := runVars(alice, q, map[string]any{"id": idStr, "skipActivity": false})
		qsIncluded := rec.stop()
		if !strings.Contains(rawIncluded, `"metadataStatus":{"state":"not-scheduled"}`) || !strings.Contains(rawIncluded, `"archived":true`) {
			t.Fatalf("included activity response: %s", rawIncluded)
		}
		if countSQL(qsIncluded, "ListMetadataStatuses") == 0 || countSQL(qsIncluded, "ListActivityStatuses") == 0 {
			t.Fatalf("expected both loaders when activity included")
		}
	})

	t.Run("repositoryOverview_never_loads_metadata_and_gates_activity", func(t *testing.T) {
		rawNoActivity := runVars(alice, fmt.Sprintf(`{ repositoryOverview(id: "%s", days: 7) { fullName } }`, idStr), nil)
		qsNoActivity := rec.stop()
		if !strings.Contains(rawNoActivity, `"fullName":"alice/demand"`) {
			t.Fatalf("overview without activity: %s", rawNoActivity)
		}
		if countSQL(qsNoActivity, "ListMetadataStatuses") != 0 || countSQL(qsNoActivity, "ListActivityStatuses") != 0 || countSQL(qsNoActivity, "FROM activity_resources") != 0 {
			t.Fatalf("overview without activity ran unexpected status SQL: %v", qsNoActivity)
		}

		rawWithActivity := runVars(alice, fmt.Sprintf(`{ repositoryOverview(id: "%s", days: 7) { fullName activity { archived pullRequests { phase } } } }`, idStr), nil)
		qsWithActivity := rec.stop()
		if !strings.Contains(rawWithActivity, `"archived":true`) || !strings.Contains(rawWithActivity, `"phase":"disabled"`) {
			t.Fatalf("overview with activity: %s", rawWithActivity)
		}
		if countSQL(qsWithActivity, "ListMetadataStatuses") != 0 {
			t.Fatalf("overview with activity must never run ListMetadataStatuses, got %d", countSQL(qsWithActivity, "ListMetadataStatuses"))
		}
		if countSQL(qsWithActivity, "ListActivityStatuses") == 0 {
			t.Fatalf("overview with activity did not run ListActivityStatuses")
		}
	})
}
