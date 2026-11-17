//go:build integration

package ingestion

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

// barrier holds each party at its first access-fence write until every party has arrived, so both transactions
// have run everything that precedes the write (any lock they took before it) when either proceeds.
type barrier struct {
	wg sync.WaitGroup
}

func newBarrier(n int) *barrier {
	b := &barrier{}
	b.wg.Add(n)
	return b
}

func (b *barrier) arrive() {
	b.wg.Done()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

type barrierTx struct {
	pgx.Tx
	once sync.Once
	b    *barrier
}

func (t *barrierTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO access_fences") {
		t.once.Do(t.b.arrive)
	}
	return t.Tx.Exec(ctx, sql, args...)
}

type poolFence struct {
	pool interface {
		Begin(context.Context) (pgx.Tx, error)
	}
	b *barrier
}

func (f poolFence) Commit(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(&barrierTx{Tx: tx, b: f.b}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Two workers that each see a different public repository turn private at the same time must both finish: the
// commits may wait on each other's global-fence row lock but must never form a lock cycle (PostgreSQL 40P01).
func TestConcurrentTighteningNeverDeadlocks(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	for round := 0; round < 3; round++ {
		a, b := int64(1000+2*round), int64(1001+2*round)
		for _, id := range []int64{a, b} {
			if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO repositories(id,name,description) VALUES(%d,'o/r%d','')`, id, id)); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		bar := newBarrier(2)
		for _, id := range []int64{a, b} {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				if err := tightenToPrivate(ctx, poolFence{pool, bar}, id); err != nil && err != errRace {
					errs <- fmt.Errorf("repo %d: %w", id, err)
				}
			}(id)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
	}
}
