//go:build integration

package analytics

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

type queryTrace []pgx.TraceQueryStartData

func (trace *queryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "-- name:") {
		*trace = append(*trace, data)
	}
	return ctx
}

func (*queryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRepositoryOverviewBaseline(t *testing.T) {
	if os.Getenv("REPOORBIT_ANALYTICS_BASELINE") != "1" {
		t.Skip("set REPOORBIT_ANALYTICS_BASELINE=1 to record the local performance baseline")
	}
	dsn, pool := testutil.Database(t)
	ctx := context.Background()
	fixture, err := os.ReadFile("testdata/analytics.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(fixture)+`
		INSERT INTO activity_issues (source_id, repository_id, number, title, state, html_url, created_at, updated_at)
		SELECT n, repo, n, 'Issue', 'open', '', ts, ts
		FROM generate_series(1001, 11000) n, LATERAL (SELECT CASE WHEN n <= 3000 THEN 10 ELSE 20 END AS repo,
			'2026-11-17'::timestamptz - (n % 89) * interval '1 day' AS ts) evidence;
		INSERT INTO activity_pull_requests (source_id, repository_id, number, title, state, html_url, created_at, updated_at, merged_at, author_id, author_login, author_type)
		SELECT source_id, repository_id, number + 20000, 'PR', 'closed', '', created_at, updated_at, updated_at, number % 60, 'author-' || (number % 60), 'User'
		FROM activity_issues WHERE source_id >= 1001;
		INSERT INTO activity_comments (source_id, repository_id, issue_number, body, html_url, created_at, updated_at, author_id, author_type)
		SELECT source_id + 12000*k, repository_id, number, '', '', created_at + k*interval '1 hour', updated_at, k, 'User'
		FROM activity_issues, generate_series(1, 3) k WHERE source_id >= 1001;
		INSERT INTO activity_reviews (source_id, repository_id, pull_request_id, state, body, html_url, submitted_at, author_id, author_login, author_type)
		SELECT source_id + 12000*k, repository_id, source_id, 'APPROVED', '', '', created_at+k*interval '1 hour', k, 'reviewer-' || k, 'User'
		FROM activity_pull_requests, generate_series(1, 2) k WHERE source_id >= 1001;
		INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark)
		SELECT DISTINCT repository_id, 'comments', '2026-08-18 00:00Z'::timestamptz, 'catchup', '2026-11-17 10:00Z'::timestamptz FROM activity_issues WHERE source_id >= 1001
		ON CONFLICT DO NOTHING;
		INSERT INTO activity_children (repository_id, resource, parent_id, parent_number, watermark)
		SELECT repository_id, 'reviews', source_id, number, '2026-11-17 10:00Z'::timestamptz FROM activity_pull_requests WHERE source_id >= 1001;
		ANALYZE;`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var trace queryTrace
	cfg.ConnConfig.Tracer = &trace
	probe, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for range 5 {
		if _, err := LoadRepositoryOverview(ctx, pool, 10, 90, now); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := make([]time.Duration, 100)
	for i := range elapsed {
		start := time.Now()
		if _, err := LoadRepositoryOverview(ctx, pool, 10, 90, now); err != nil {
			t.Fatal(err)
		}
		elapsed[i] = time.Since(start)
	}
	slices.Sort(elapsed)
	t.Logf("90-day overview P95 (100 serial warm runs): %s", elapsed[94])

	out, err := LoadRepositoryOverview(ctx, probe, 10, 90, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace) != 5 || len(out.Contributors) != 20 || len(out.Timeline) != 50 || len(out.Daily) != 90 {
		t.Fatalf("expected five queries, capped lists and 90 daily points, got %d/%d/%d/%d", len(trace), len(out.Contributors), len(out.Timeline), len(out.Daily))
	}
	for _, query := range trace {
		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+query.SQL, query.Args...)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s\n%s", strings.SplitN(query.SQL, "\n", 2)[0], strings.Join(plan, "\n"))
	}
}
