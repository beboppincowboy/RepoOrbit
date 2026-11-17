//go:build integration

package ingestion_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

func TestPrivateRefreshNeedsLiveAccessAndFenceValid(t *testing.T) {
	e := setup(t)
	a := e.signIn("alice")
	id := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 9, id)
	e.store(a, id, true)
	if err := e.run(e.fence(), id); err != nil || e.name(id) != "alice/secret" {
		t.Fatalf("private refresh with access: %v %q", err, e.name(id))
	}
}

func TestPrivateCommitWithheldWhenFenceBumpedAfterFetch(t *testing.T) {
	e := setup(t)
	a := e.signIn("alice")
	id := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 9, id)
	e.store(a, id, true)
	f := txFence{pool: e.pool, before: func() { // revocation lands between fetch and commit
		tx, _ := e.pool.Begin(context.Background())
		_ = accessfence.InvalidateAccess(context.Background(), tx, accessfence.ScopeRepository, id)
		_ = tx.Commit(context.Background())
	}}
	var d *jobs.Defer
	if err := e.run(f, id); !errors.As(err, &d) {
		t.Fatalf("err = %v, want *jobs.Defer", err)
	}
	if e.name(id) != "stale/name" {
		t.Fatal("private metadata committed under a bumped fence")
	}
}

func TestPrivateAccessRemovedOrUnverifiedPreventsCommit(t *testing.T) {
	e := setup(t)
	a := e.signIn("alice")
	id := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 9, id)
	e.store(a, id, true)

	e.gh.Uninstall(9) // app access removed: denied, nothing readable
	var p *jobs.Pause
	if err := e.run(e.fence(), id); !errors.As(err, &p) || e.name(id) != "stale/name" {
		t.Fatalf("denied: err=%v name=%q", err, e.name(id))
	}

	e.gh.Install("alice", 9, id)
	e.gh.Fail(true) // GitHub down: access cannot be verified
	if err := e.run(e.fence(), id); err == nil || errors.As(err, &p) || e.name(id) != "stale/name" {
		t.Fatalf("unverified: err=%v name=%q", err, e.name(id))
	}
}

// A removal for an installation the system never saved still withholds a private commit (global permissions version).
func TestPrivateCommitWithheldWhenUnsavedInstallationIsRemovedAfterFetch(t *testing.T) {
	e := setup(t)
	a := e.signIn("alice")
	id := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 9, id)
	e.store(a, id, true)
	f := txFence{pool: e.pool, before: func() {
		tx, _ := e.pool.Begin(context.Background())
		_ = accessfence.InvalidateAccess(context.Background(), tx, accessfence.ScopeInstallation, 424242)
		_ = tx.Commit(context.Background())
	}}
	var d *jobs.Defer
	if err := e.run(f, id); !errors.As(err, &d) {
		t.Fatalf("err = %v, want *jobs.Defer", err)
	}
	if e.name(id) != "stale/name" {
		t.Fatal("private metadata committed after an unrelated-looking installation removal")
	}
}

// A deselection that is in flight (uncommitted) while the refresh commits cannot slip between the eligibility check
// and the metadata write: the commit takes a share lock on the credential owner's selection row and, when it cannot
// get it at once, defers instead of waiting (which is also what keeps it out of any lock-wait cycle).
func TestInFlightDeselectionWithholdsTheCommit(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	ctx := context.Background()
	var tx2 pgx.Tx
	f := txFence{pool: e.pool, before: func() {
		if tx2 != nil {
			return // later commits of this run (status recording) must not start a second deselection
		}
		var err error
		if tx2, err = e.pool.Begin(ctx); err != nil {
			t.Error(err)
			return
		}
		if _, err = tx2.Exec(ctx, "UPDATE selections SET deselected_at = now()"); err != nil {
			t.Error(err)
		}
	}}
	var d *jobs.Defer
	err := e.run(f, id)
	if tx2 != nil {
		defer tx2.Rollback(ctx)
	}
	if !errors.As(err, &d) {
		t.Fatalf("err = %v, want *jobs.Defer", err)
	}
	if e.name(id) != "stale/name" {
		t.Fatal("metadata committed under an in-flight deselection")
	}
	if ok, _ := e.syncRow(id); ok {
		t.Fatal("success recorded under an in-flight deselection")
	}
}

// The credential's owner deselects while another user still follows: the refresh used the owner's credential, so it
// must not commit under it (pick again), and nothing is written.
func TestOwnerDeselectionWithOtherFollowerStillWithholdsTheCommit(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	bob := e.signIn("bob")
	id := e.gh.AddRepo("carol", "tool", false)
	e.store(alice, id, false)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'starred', now() + interval '1 second')`, bob, id); err != nil {
		t.Fatal(err)
	}
	f := txFence{pool: e.pool, before: func() {
		_, _ = e.pool.Exec(context.Background(), "UPDATE selections SET deselected_at = now() WHERE user_id = $1", alice)
	}}
	var d *jobs.Defer
	if err := e.run(f, id); !errors.As(err, &d) {
		t.Fatalf("err = %v, want *jobs.Defer", err)
	}
	if e.name(id) != "stale/name" {
		t.Fatal("metadata committed under a credential whose owner stopped following")
	}
}

// jobLockFence takes the job row lock first (as the real claim check does) before running the commit.
type jobLockFence struct {
	pool *pgxpool.Pool
	key  string
}

func (f jobLockFence) Commit(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM jobs WHERE kind = $1 AND key = $2 FOR UPDATE`, string(jobs.KindRepoMetadataRefresh), f.key); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Lock order is job row, then selection row. A writer that suspends a selection and then resumes the repository's
// job (the star store) takes them in the other order; the refresh's NOWAIT selection lock means it can only fail and
// retry, never wait, so the pair cannot deadlock however they interleave.
func TestRefreshCommitAndStarStoreNeverDeadlock(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	ctx := context.Background()
	key := fmt.Sprintf("repo:%d", id)
	tx, _ := e.pool.Begin(ctx)
	if err := jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		errs := make(chan error, 2)
		go func() {
			err := e.run(jobLockFence{e.pool, key}, id)
			var d *jobs.Defer
			if errors.As(err, &d) {
				err = nil // a withheld commit is the expected, safe outcome
			}
			errs <- err
		}()
		go func() {
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `UPDATE selections SET follow_suspended_at = now() WHERE user_id = $1`, uid); err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Duration(i%4) * time.Millisecond)
			if err = jobs.Resume(ctx, tx, jobs.KindRepoMetadataRefresh, key, time.Now()); err != nil {
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
		if _, err := e.pool.Exec(ctx, `UPDATE selections SET follow_suspended_at = NULL`); err != nil {
			t.Fatal(err)
		}
	}
}

// A reselect locks the repository row (its upsert) and then the selection row; the refresh commit must take them in
// that same order (repository rows first, selection lock last), or the two wait on each other.
func TestRefreshCommitAndReselectNeverDeadlock(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	ctx := context.Background()
	reselect, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reselect.Rollback(ctx)
	if _, err := reselect.Exec(ctx, `UPDATE repositories SET description = description WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := e.run(e.fence(), id)
		var d *jobs.Defer
		if errors.As(err, &d) {
			err = nil
		}
		done <- err
	}()
	time.Sleep(400 * time.Millisecond) // the refresh is now inside its commit, waiting on or past the repository row
	if _, err := reselect.Exec(ctx, `INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'starred', now())
		ON CONFLICT (user_id, repository_id) DO UPDATE SET deselected_at = NULL`, uid, id); err != nil {
		t.Fatalf("reselect: %v", err)
	}
	if err := reselect.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("refresh: %v", err)
	}
}
