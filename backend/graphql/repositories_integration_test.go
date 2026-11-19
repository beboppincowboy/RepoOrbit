//go:build integration

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func TestDemoDatabaseGraphQL(t *testing.T) {
	ctx := context.Background()
	raw := os.Getenv("DATABASE_URL")
	if raw == "" {
		t.Fatal("DATABASE_URL required")
	}
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("repoorbit_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Chdir("..")
	for i := 0; i < 2; i++ {
		if err = platform.Migrate(ctx, u.String()); err != nil {
			t.Fatal(err)
		}
		if err = platform.SeedDemo(ctx, u.String()); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := platform.OpenDB(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var repos, issues int
	if err = pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM repositories), (SELECT count(*) FROM issues)").Scan(&repos, &issues); err != nil || repos != 2 || issues != 3 {
		t.Fatalf("seed counts %d %d: %v", repos, issues, err)
	}
	h := Server(&Resolver{Queries: db.New(pool), Demo: true, DemoPool: pool})
	run := func(query string) string {
		body, _ := json.Marshal(map[string]string{"query": query})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Body.String()
	}
	result := run(`{ selectedRepositories(first: 1) { nodes { id fullName } endCursor hasNextPage } issues(repositoryIds: ["1"], first: 1) { nodes { id title } hasNextPage } }`)
	for _, want := range []string{"orbit/example", "Improve onboarding documentation", `"hasNextPage":true`} {
		if !strings.Contains(result, want) {
			t.Fatalf("missing %s: %s", want, result)
		}
	}
	result = run(`{ selectedRepositories(first: 1, after: "1") { nodes { fullName } } issues(repositoryIds: ["1"], first: 1, after: "1") { nodes { title } } }`)
	if !strings.Contains(result, "orbit/launchpad") || !strings.Contains(result, "Add keyboard shortcuts") {
		t.Fatal(result)
	}
	for _, query := range []string{`{ selectedRepositories(first: 101) { nodes { fullName } } }`, `{ selectedRepositories(after: "bad") { nodes { fullName } } }`, `{ ... on Query { ... on Query { ... on Query { ... on Query { ... on Query { ... on Query { selectedRepositories { nodes { fullName } } } } } } } } }`} {
		if got := run(query); !strings.Contains(got, `"errors"`) {
			t.Fatalf("accepted %s: %s", query, got)
		}
	}
	if got := run(`{ selectedRepositories(first: 100) { nodes { a:fullName b:fullName c:fullName d:fullName e:fullName f:fullName g:fullName h:fullName i:fullName j:fullName k:fullName } } }`); !strings.Contains(got, "exceeds the limit of 1200") {
		t.Fatalf("expected the alias-heavy query to exceed the existing complexity budget: %s", got)
	}
	// Demo analytics are populated from the seeded evidence, and the overview sums the same per-repository metrics.
	var demo struct {
		Data struct {
			CrossRepositoryOverview struct {
				RepositoriesCounted int
				OpenIssues          struct{ Value *int64 }
				NeedsAttention      []struct{ Kind string }
				AssignedToMe        []struct{ FullName string }
			}
			A, B struct{ OpenIssues struct{ Value *int64 } }
		}
	}
	got := run(`{ crossRepositoryOverview(days: 30) { repositoriesCounted openIssues { value } needsAttention { kind } assignedToMe { fullName } } }`)
	perRepo := run(`{ A: repositoryOverview(id: "1", days: 30) { openIssues { value } } }`)
	perRepo2 := run(`{ B: repositoryOverview(id: "2", days: 30) { openIssues { value } contributors { login } } }`)
	for _, part := range []string{got, perRepo, perRepo2} {
		if err := json.Unmarshal([]byte(part), &demo); err != nil {
			t.Fatal(err)
		}
	}
	c := demo.Data.CrossRepositoryOverview
	if c.RepositoriesCounted != 2 || c.OpenIssues.Value == nil || demo.Data.A.OpenIssues.Value == nil || demo.Data.B.OpenIssues.Value == nil ||
		*c.OpenIssues.Value != *demo.Data.A.OpenIssues.Value+*demo.Data.B.OpenIssues.Value || len(c.NeedsAttention) == 0 || len(c.AssignedToMe) == 0 ||
		!strings.Contains(perRepo2, `"login"`) {
		t.Fatalf("demo analytics: %s %s %s", got, perRepo, perRepo2)
	}
	if _, err = pool.Exec(ctx, "UPDATE repositories SET name='changed/database' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if got := run(`{ selectedRepositories { nodes { fullName } } }`); !strings.Contains(got, "changed/database") {
		t.Fatal(got)
	}
}

// Demo mode returns only demo rows and real mode never returns them, so a database that holds both
// cannot leak real rows into the demo or demo rows into real use.
func TestModesSeeOnlyTheirOwnRows(t *testing.T) {
	ctx := context.Background()
	url, pool := testutil.Database(t)
	if err := platform.SeedDemo(ctx, url); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, name, description) VALUES (500, 'real/private-thing', 'x');
		INSERT INTO issues (id, repository_id, title, state) VALUES (500, 500, 'real issue', 'open')`); err != nil {
		t.Fatal(err)
	}
	run := func(demo bool, query string) string {
		body, _ := json.Marshal(map[string]string{"query": query})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		Server(&Resolver{Queries: db.New(pool), Demo: demo}).ServeHTTP(w, req)
		return w.Body.String()
	}
	q := `{ selectedRepositories { nodes { fullName } } issues(repositoryIds: ["1","2","500"]) { nodes { title } } }`
	got := run(true, q)
	if strings.Contains(got, "real/") || strings.Contains(got, "real issue") || !strings.Contains(got, "orbit/example") {
		t.Fatalf("demo mode: %s", got)
	}
	// Real mode is scoped to the signed-in user's selections (U2b), so a call with no viewer, or an unselected
	// real row, returns nothing: neither demo rows nor other people's real rows leak.
	got = run(false, q)
	for _, leaked := range []string{"orbit/", "onboarding", "Document the release", "real/private-thing", "real issue"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("real mode leaked %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "sign in required") {
		t.Fatalf("real mode served a request with no viewer: %s", got)
	}
}
