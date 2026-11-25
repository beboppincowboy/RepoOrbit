package testutil

import (
	"context"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"
)

type SQLRecorder struct {
	mu      sync.Mutex
	enabled bool
	queries []string
}

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

func (r *SQLRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	if r.enabled {
		r.queries = append(r.queries, data.SQL)
	}
	r.mu.Unlock()
	return ctx
}

func (*SQLRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
