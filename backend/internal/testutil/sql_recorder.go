package testutil

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SQLRecorder struct {
	mu      sync.Mutex
	enabled bool
	queries []string
}

var (
	_ pgx.QueryTracer = (*SQLRecorder)(nil)
	_ pgx.BatchTracer = (*SQLRecorder)(nil)
)

func (r *SQLRecorder) Start() {
	r.mu.Lock()
	r.queries = nil
	r.enabled = true
	r.mu.Unlock()
}

func (r *SQLRecorder) Stop() []string {
	r.mu.Lock()
	r.enabled = false
	out := slices.Clone(r.queries)
	r.mu.Unlock()
	return out
}

func (r *SQLRecorder) record(sql string) {
	r.mu.Lock()
	if r.enabled {
		r.queries = append(r.queries, sql)
	}
	r.mu.Unlock()
}

func (r *SQLRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.record(data.SQL)
	return ctx
}

func (*SQLRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (*SQLRecorder) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (r *SQLRecorder) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	r.record(data.SQL)
}

func (*SQLRecorder) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// OpenTracedPool opens a pgxpool.Pool against dbURL with tracer attached and closes it on test cleanup.
func OpenTracedPool(t *testing.T, dbURL string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse db url: %v", err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
