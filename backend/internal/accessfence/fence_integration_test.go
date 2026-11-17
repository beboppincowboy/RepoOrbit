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
		if s, err = accessfence.TakeSnapshot(context.Background(), tx, user, repo); err != nil {
			t.Fatal(err)
		}
	})
	return s
}

func valid(t *testing.T, pool *pgxpool.Pool, s accessfence.Snapshot) (ok bool) {
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if ok, err = s.StillCurrent(context.Background(), tx); err != nil {
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
			if err := accessfence.InvalidateAccess(context.Background(), tx, tc.scope, tc.id); err != nil {
				t.Fatal(err)
			}
		})
		if valid(t, pool, s) {
			t.Fatalf("%s: snapshot survived bump", tc.scope)
		}
	}
}

// A rolled-back bump changes nothing. A committed bump of any subject, related or not, withholds a private
// commit (one retry): the global permissions version cannot tell which subject a fetch depended on.
func TestRolledBackBumpKeepsSnapshotValidButAnyCommittedBumpWithholds(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	s := take(t, pool, user, 500)
	tx, _ := pool.Begin(ctx)
	_ = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeUser, user)
	_ = tx.Rollback(ctx)
	if !valid(t, pool, s) {
		t.Fatal("a rolled-back bump invalidated the snapshot")
	}
	inTx(t, pool, func(tx pgx.Tx) { _ = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeRepository, 999) })
	if valid(t, pool, s) {
		t.Fatal("a committed bump of an unrelated subject must still withhold a private commit")
	}
}

// Removal of an installation the system never saved (so the snapshot cannot track it) still withholds the commit.
func TestBumpOfUnsavedInstallationWithholds(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	s := take(t, pool, user, 500)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeInstallation, 424242); err != nil {
			t.Fatal(err)
		}
	})
	if valid(t, pool, s) {
		t.Fatal("bump of an installation absent from the local table did not withhold the commit")
	}
}

// A bump and a private commit check queue on the global row first, so repeated concurrent runs never deadlock.
func TestConcurrentBumpAndStillCurrentNeverDeadlock(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	for i := 0; i < 25; i++ {
		s := take(t, pool, user, 500)
		errs := make(chan error, 2)
		go func() {
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			if err := accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeInstallation, 77); err != nil {
				errs <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
			errs <- tx.Commit(ctx)
		}()
		go func() {
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			if _, err := s.StillCurrent(ctx, tx); err != nil {
				errs <- err
				return
			}
			errs <- tx.Commit(ctx)
		}()
		for j := 0; j < 2; j++ {
			if err := <-errs; err != nil {
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
	}
}

// A commit transaction that checks StillCurrent while a bump is in flight must wait for it and then see it.
func TestValidWaitsForConcurrentBump(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	s := take(t, pool, user, 500)
	bump, _ := pool.Begin(ctx)
	if err := accessfence.InvalidateAccess(ctx, bump, accessfence.ScopeUser, user); err != nil {
		t.Fatal(err)
	}
	res := make(chan bool, 1)
	go func() {
		tx, _ := pool.Begin(ctx)
		defer tx.Rollback(ctx)
		ok, _ := s.StillCurrent(ctx, tx)
		res <- ok
	}()
	select {
	case <-res:
		t.Fatal("StillCurrent did not wait for the in-flight bump")
	case <-time.After(300 * time.Millisecond):
	}
	_ = bump.Commit(ctx)
	if ok := <-res; ok {
		t.Fatal("StillCurrent returned true after the bump committed")
	}
}

func TestCurrentSeesBumpWithoutTransaction(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	user := seed(t, pool)
	s, err := accessfence.TakeSnapshotForUser(ctx, pool, user)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Current(ctx, pool); !ok {
		t.Fatal("fresh cache snapshot invalid")
	}
	inTx(t, pool, func(tx pgx.Tx) { _ = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeRepository, 12345) })
	if ok, _ := s.Current(ctx, pool); ok {
		t.Fatal("any repository bump must invalidate cached user reads")
	}
}

// The down migration must succeed with a global row present and restore the three-scope check.
func TestGlobalFenceDownMigrationStatements(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	seed(t, pool)
	inTx(t, pool, func(tx pgx.Tx) { _ = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeUser, 1) })
	tx, _ := pool.Begin(ctx)
	defer tx.Rollback(ctx)
	for _, q := range []string{
		`DELETE FROM access_fences WHERE scope = 'global'`,
		`ALTER TABLE access_fences DROP CONSTRAINT access_fences_scope_check`,
		`ALTER TABLE access_fences ADD CONSTRAINT access_fences_scope_check CHECK (scope IN ('user', 'installation', 'repository'))`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access_fences (scope, subject_id, epoch) VALUES ('global', 0, 1)`); err == nil {
		t.Fatal("restored constraint still accepts the global scope")
	}
}
