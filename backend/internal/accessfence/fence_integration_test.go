//go:build integration

package accessfence_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func seed(t *testing.T, pool *pgxpool.Pool) (userID int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `INSERT INTO users (github_id, login, access_token_enc) VALUES (1, 'a', '\x01') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations (user_id, installation_id, account_login) VALUES ($1, 77, 'a')`, userID); err != nil {
		t.Fatal(err)
	}
	return userID
}

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	fn(tx)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func take(t *testing.T, pool *pgxpool.Pool, user, repo int64) (s accessfence.Snapshot) {
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if s, err = accessfence.Take(context.Background(), tx, user, repo); err != nil {
			t.Fatal(err)
		}
	})
	return s
}

func valid(t *testing.T, pool *pgxpool.Pool, s accessfence.Snapshot) (ok bool) {
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if ok, err = s.Valid(context.Background(), tx); err != nil {
			t.Fatal(err)
		}
	})
	return ok
}

func TestBumpInvalidatesSnapshotPerScope(t *testing.T) {
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	for _, tc := range []struct {
		scope accessfence.Scope
		id    int64
	}{{accessfence.ScopeUser, user}, {accessfence.ScopeInstallation, 77}, {accessfence.ScopeRepository, 500}} {
		s := take(t, pool, user, 500)
		if !valid(t, pool, s) {
			t.Fatalf("%s: fresh snapshot invalid", tc.scope)
		}
		inTx(t, pool, func(tx pgx.Tx) {
			if err := accessfence.Bump(context.Background(), tx, tc.scope, tc.id); err != nil {
				t.Fatal(err)
			}
		})
		if valid(t, pool, s) {
			t.Fatalf("%s: snapshot survived bump", tc.scope)
		}
	}
}

func TestUnrelatedBumpAndRollbackKeepSnapshotValid(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	s := take(t, pool, user, 500)
	inTx(t, pool, func(tx pgx.Tx) {
		_ = accessfence.Bump(ctx, tx, accessfence.ScopeRepository, 999)
		_ = accessfence.Bump(ctx, tx, accessfence.ScopeUser, user+1)
	})
	tx, _ := pool.Begin(ctx)
	_ = accessfence.Bump(ctx, tx, accessfence.ScopeUser, user)
	_ = tx.Rollback(ctx)
	if !valid(t, pool, s) {
		t.Fatal("unrelated bump or rolled-back bump invalidated the snapshot")
	}
}

// A commit transaction that checks Valid while a bump is in flight must wait for it and then see it.
func TestValidWaitsForConcurrentBump(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	s := take(t, pool, user, 500)
	bump, _ := pool.Begin(ctx)
	if err := accessfence.Bump(ctx, bump, accessfence.ScopeUser, user); err != nil {
		t.Fatal(err)
	}
	res := make(chan bool, 1)
	go func() {
		tx, _ := pool.Begin(ctx)
		defer tx.Rollback(ctx)
		ok, _ := s.Valid(ctx, tx)
		res <- ok
	}()
	select {
	case <-res:
		t.Fatal("Valid did not wait for the in-flight bump")
	case <-time.After(300 * time.Millisecond):
	}
	_ = bump.Commit(ctx)
	if ok := <-res; ok {
		t.Fatal("Valid returned true after the bump committed")
	}
}

func TestCurrentSeesBumpWithoutTransaction(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	s, err := accessfence.TakeForUser(ctx, pool, user)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Current(ctx, pool); !ok {
		t.Fatal("fresh cache snapshot invalid")
	}
	inTx(t, pool, func(tx pgx.Tx) { _ = accessfence.Bump(ctx, tx, accessfence.ScopeRepository, 12345) })
	if ok, _ := s.Current(ctx, pool); ok {
		t.Fatal("any repository bump must invalidate cached user reads")
	}
}
