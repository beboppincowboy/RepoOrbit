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
	h := Server(&Resolver{Queries: db.New(pool), Demo: true})
	run := func(query string) string {
		body, _ := json.Marshal(map[string]string{"query": query})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Body.String()
	}
	result := run(`{ repositories(first: 1) { nodes { id name } endCursor hasNextPage } issues(repositoryIds: ["1"], first: 1) { nodes { id title } hasNextPage } }`)
	for _, want := range []string{"orbit/example", "Improve onboarding documentation", `"hasNextPage":true`} {
		if !strings.Contains(result, want) {
			t.Fatalf("missing %s: %s", want, result)
		}
	}
	result = run(`{ repositories(first: 1, after: "1") { nodes { name } } issues(repositoryIds: ["1"], first: 1, after: "1") { nodes { title } } }`)
	if !strings.Contains(result, "orbit/launchpad") || !strings.Contains(result, "Add keyboard shortcuts") {
		t.Fatal(result)
	}
	for _, query := range []string{`{ repositories(first: 101) { nodes { name } } }`, `{ repositories(first: 100) { nodes { a:name b:name c:name d:name e:name f:name g:name h:name i:name j:name k:name } } }`, `{ repositories(after: "bad") { nodes { name } } }`, `{ ... on Query { ... on Query { ... on Query { ... on Query { ... on Query { ... on Query { repositories { nodes { name } } } } } } } } }`} {
		if got := run(query); !strings.Contains(got, `"errors"`) {
			t.Fatalf("accepted %s: %s", query, got)
		}
	}
	if _, err = pool.Exec(ctx, "UPDATE repositories SET name='changed/database' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if got := run(`{ repositories { nodes { name } } }`); !strings.Contains(got, "changed/database") {
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
	q := `{ repositories { nodes { name } } issues(repositoryIds: ["1","2","500"]) { nodes { title } } }`
	got := run(true, q)
	if strings.Contains(got, "real/") || strings.Contains(got, "real issue") || !strings.Contains(got, "orbit/example") {
		t.Fatalf("demo mode: %s", got)
	}
	got = run(false, q)
	if strings.Contains(got, "orbit/") || strings.Contains(got, "onboarding") || strings.Contains(got, "Document the release") {
		t.Fatalf("real mode shows demo rows: %s", got)
	}
	if !strings.Contains(got, "real/private-thing") || !strings.Contains(got, "real issue") {
		t.Fatalf("real mode lost real rows: %s", got)
	}
}
