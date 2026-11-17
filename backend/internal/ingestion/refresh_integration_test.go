//go:build integration

package ingestion_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// txFence is a fake jobs.ClaimFence: it commits fn in a real transaction. before runs just ahead of it, standing
// in for something that happens while the job was fetching.
type txFence struct {
	pool   *pgxpool.Pool
	before func()
}

func (f txFence) Commit(ctx context.Context, fn func(tx pgx.Tx) error) error {
	if f.before != nil {
		f.before()
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	gh   *fake.Server
	ghc  *githubapp.Client
	auth *auth.Service
	repo *repositories.Service
	deps *ingestion.Deps
}

func setup(t *testing.T) *env {
	t.Helper()
	_, pool := testutil.Database(t)
	gh := fake.New("cid", "secret")
	srv := httptest.NewServer(gh.Handler())
	t.Cleanup(srv.Close)
	client := githubapp.New("cid", "secret", srv.URL, srv.URL, "http://app.test/cb")
	q := db.New(pool)
	svc := &auth.Service{Pool: pool, Queries: q, GitHub: client, Key: bytes.Repeat([]byte{1}, 32)}
	repo := &repositories.Service{Queries: q, Pool: pool, GitHub: client, TokenSource: svc, TTL: time.Nanosecond}
	deps := &ingestion.Deps{Pool: pool, GitHub: client, Tokens: svc, Access: repo, Clock: wallClock{}}
	return &env{t: t, pool: pool, gh: gh, ghc: client, auth: svc, repo: repo, deps: deps}
}

// signIn completes OAuth against the fake GitHub and returns the new user's id.
func (e *env) signIn(login string) int64 {
	e.t.Helper()
	ctx := context.Background()
	authURL, state, err := e.auth.BeginLogin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	q.Set("as", login)
	u.RawQuery = q.Encode()
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(u.String())
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	cb, _ := url.Parse(resp.Header.Get("Location"))
	if _, err := e.auth.CompleteLogin(ctx, state, state, cb.Query().Get("code")); err != nil {
		e.t.Fatal(err)
	}
	var id int64
	if err := e.pool.QueryRow(ctx, "SELECT id FROM users WHERE login = $1", login).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// store writes a stale local copy of the repository and the user's active selection of it.
func (e *env) store(userID, repoID int64, private bool) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO repositories (id, name, description, is_private) VALUES ($1, 'stale/name', 'stale', $2) ON CONFLICT DO NOTHING`, repoID, private); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'starred', now())`, userID, repoID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) run(fence jobs.ClaimFence, repoID int64) error {
	return e.deps.RefreshMetadata(context.Background(), jobs.Job{Kind: jobs.KindRepoMetadataRefresh, Key: fmt.Sprintf("repo:%d", repoID)}, fence)
}

func (e *env) name(repoID int64) string {
	var n string
	if err := e.pool.QueryRow(context.Background(), "SELECT name FROM repositories WHERE id = $1", repoID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) syncRow(repoID int64) (success bool, code string) {
	var at *time.Time
	var c *string
	err := e.pool.QueryRow(context.Background(), "SELECT last_success_at, last_error_code FROM repository_sync WHERE repository_id = $1", repoID).Scan(&at, &c)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		e.t.Fatal(err)
	}
	if c != nil {
		code = *c
	}
	return at != nil, code
}

func hits(e *env, repoID int64) int { return e.gh.Hits(fmt.Sprintf("/repositories/%d", repoID)) }

func (e *env) fence() txFence { return txFence{pool: e.pool} }

func TestRefreshCommitsPublicMetadataAndRecordsSuccess(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	if err := e.run(e.fence(), id); err != nil {
		t.Fatal(err)
	}
	if got := e.name(id); got != "bob/tool" {
		t.Fatalf("name = %q", got)
	}
	if ok, code := e.syncRow(id); !ok || code != "" {
		t.Fatalf("sync = %v %q", ok, code)
	}
}

func TestSharedRefreshIsOneFetchForManySelectorsAndSurvivesOneDeselection(t *testing.T) {
	e := setup(t)
	a, b := e.signIn("alice"), e.signIn("carol")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(a, id, false)
	e.store(b, id, false)
	if err := e.run(e.fence(), id); err != nil {
		t.Fatal(err)
	}
	if hits(e, id) != 1 {
		t.Fatalf("fetches = %d, want 1 shared", hits(e, id))
	}
	if _, err := e.pool.Exec(context.Background(), "UPDATE selections SET deselected_at = now() WHERE user_id = $1", a); err != nil {
		t.Fatal(err)
	}
	e.gh.Fail(false)
	if _, err := e.pool.Exec(context.Background(), "UPDATE repositories SET name = 'stale/name' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	if err := e.run(e.fence(), id); err != nil || e.name(id) != "bob/tool" {
		t.Fatalf("remaining selector must keep sync running: %v %q", err, e.name(id))
	}
}

func TestFinalDeselectionPreventsOrdinaryCommit(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	if _, err := e.pool.Exec(context.Background(), "UPDATE selections SET deselected_at = now()"); err != nil {
		t.Fatal(err)
	}
	if err := e.run(e.fence(), id); err != nil {
		t.Fatal(err)
	}
	if e.name(id) != "stale/name" || hits(e, id) != 0 {
		t.Fatalf("deselected repo was refreshed: %q, %d fetches", e.name(id), hits(e, id))
	}
	// Deselection that lands after the fetch but before commit also prevents the commit.
	if _, err := e.pool.Exec(context.Background(), "UPDATE selections SET deselected_at = NULL"); err != nil {
		t.Fatal(err)
	}
	f := txFence{pool: e.pool, before: func() {
		_, _ = e.pool.Exec(context.Background(), "UPDATE selections SET deselected_at = now()")
	}}
	if err := e.run(f, id); err != nil || e.name(id) != "stale/name" {
		t.Fatalf("commit must recheck selection: %v %q", err, e.name(id))
	}
}

func TestSuspendedFollowIsNotACredentialOrReason(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	if _, err := e.pool.Exec(context.Background(), "UPDATE selections SET follow_suspended_at = now()"); err != nil {
		t.Fatal(err)
	}
	if err := e.run(e.fence(), id); err != nil || hits(e, id) != 0 {
		t.Fatalf("suspended selection must not drive work: %v, %d fetches", err, hits(e, id))
	}
}

func TestPublicCredentialFallsBackWhenFirstUserIsRevoked(t *testing.T) {
	e := setup(t)
	a, b := e.signIn("alice"), e.signIn("carol")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(a, id, false)
	e.store(b, id, false)
	e.gh.RevokeUser("alice") // GitHub rejects alice's token
	if err := e.run(e.fence(), id); err != nil {
		t.Fatal(err)
	}
	if e.name(id) != "bob/tool" {
		t.Fatal("fallback credential was not used")
	}
	var enc []byte
	_ = e.pool.QueryRow(context.Background(), "SELECT access_token_enc FROM users WHERE id = $1", a).Scan(&enc)
	if len(enc) != 0 {
		t.Fatal("rejected credential must be disabled")
	}
}

func TestNoUsableCredentialPausesWithoutRemoteCalls(t *testing.T) {
	e := setup(t)
	a := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(a, id, false)
	if err := e.auth.Revoke(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var p *jobs.Pause
	if err := e.run(e.fence(), id); !errors.As(err, &p) {
		t.Fatalf("err = %v, want *jobs.Pause", err)
	}
	if hits(e, id) != 0 {
		t.Fatal("paused work made a remote call")
	}
	if _, code := e.syncRow(id); code != "no_credential" {
		t.Fatalf("code = %q", code)
	}
}

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

func TestThrottledCredentialIsPacedNotRotated(t *testing.T) {
	e := setup(t)
	a, b := e.signIn("alice"), e.signIn("carol")
	id1 := e.gh.AddRepo("bob", "one", false)
	id2 := e.gh.AddRepo("bob", "two", false)
	e.store(a, id1, false)
	e.store(b, id1, false)
	e.store(a, id2, false)
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	e.gh.LimitUntil(until)

	var d *jobs.Defer
	if err := e.run(e.fence(), id1); !errors.As(err, &d) || !d.RetryAt.Equal(until) {
		t.Fatalf("err = %v, want Defer until %v", err, until)
	}
	if hits(e, id1) != 1 {
		t.Fatalf("fetches = %d: second user must not be tried to dodge the limit", hits(e, id1))
	}
	d = nil
	if err := e.run(e.fence(), id2); !errors.As(err, &d) || hits(e, id2) != 0 {
		t.Fatalf("other work on the throttled credential must wait without calling: %v, %d", err, hits(e, id2))
	}
}

// --- scheduling (needs the real queue; skipped until U3a is merged) ---------------------------

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(context.Background())
}

func TestRequestRefreshOnlyForEligibleSelections(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	for _, stmt := range []string{"UPDATE selections SET deselected_at = now()", "UPDATE selections SET deselected_at = NULL, follow_suspended_at = now()"} {
		if _, err := e.pool.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
		var scheduled bool
		if err := inTx(t, e.pool, func(tx pgx.Tx) (err error) {
			scheduled, err = ingestion.RequestMetadataRefresh(context.Background(), tx, id)
			return
		}); err != nil || scheduled {
			t.Fatalf("%s: scheduled=%v err=%v", stmt, scheduled, err)
		}
	}
	if _, err := e.pool.Exec(context.Background(), "UPDATE selections SET follow_suspended_at = NULL"); err != nil {
		t.Fatal(err)
	}
	var scheduled bool
	err := inTx(t, e.pool, func(tx pgx.Tx) (err error) {
		scheduled, err = ingestion.RequestMetadataRefresh(context.Background(), tx, id)
		return
	})
	if errors.Is(err, jobs.ErrNotImplemented) {
		t.Skip("needs U3a")
	}
	if err != nil || !scheduled {
		t.Fatalf("scheduled=%v err=%v", scheduled, err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE kind = 'repo_metadata_refresh' AND key = $1", fmt.Sprintf("repo:%d", id)).Scan(&n)
	if n != 1 {
		t.Fatalf("jobs = %d", n)
	}
}

func TestSelectionAndScheduleRollBackTogether(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("alice", "mine", false)
	boom := errors.New("enqueue failed")
	e.repo.OnSelect = func(ctx context.Context, tx pgx.Tx, repoID int64) error { return boom }
	e.repo.GitHub = e.ghc
	if err := e.repo.Connect(context.Background(), uid, id); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM selections) + (SELECT count(*) FROM repositories WHERE id = $1)", id).Scan(&n)
	if n != 0 {
		t.Fatal("selection or repository row survived a failed scheduling step")
	}
	// And with the real hook: success commits both (when the queue exists).
	e.repo.OnSelect = func(ctx context.Context, tx pgx.Tx, repoID int64) error {
		_, err := ingestion.RequestMetadataRefresh(ctx, tx, repoID)
		return err
	}
	err := e.repo.Connect(context.Background(), uid, id)
	if errors.Is(err, jobs.ErrNotImplemented) {
		t.Skip("needs U3a")
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(context.Background(), "SELECT count(*) FROM jobs WHERE key = $1", fmt.Sprintf("repo:%d", id)).Scan(&n)
	if n != 1 {
		t.Fatalf("jobs = %d", n)
	}
}

func TestStartupReconciliationSchedulesPreU3Selections(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id1 := e.gh.AddRepo("bob", "one", false)
	id2 := e.gh.AddRepo("bob", "two", false)
	e.store(uid, id1, false)
	e.store(uid, id2, false)
	if _, err := e.pool.Exec(context.Background(), "UPDATE selections SET deselected_at = now() WHERE repository_id = $1", id2); err != nil {
		t.Fatal(err)
	}
	n, err := ingestion.ReconcileSelections(context.Background(), e.pool)
	if errors.Is(err, jobs.ErrNotImplemented) {
		t.Skip("needs U3a")
	}
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestOverlappingRefreshProducesFollowUp(t *testing.T) {
	e := setup(t)
	uid := e.signIn("alice")
	id := e.gh.AddRepo("bob", "tool", false)
	e.store(uid, id, false)
	err := inTx(t, e.pool, func(tx pgx.Tx) error {
		_, err := ingestion.RequestMetadataRefresh(context.Background(), tx, id)
		return err
	})
	if errors.Is(err, jobs.ErrNotImplemented) {
		t.Skip("needs U3a")
	}
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("repo:%d", id)
	if _, err := e.pool.Exec(context.Background(), "UPDATE jobs SET state = 'running' WHERE key = $1", key); err != nil {
		t.Fatal(err)
	}
	if err := inTx(t, e.pool, func(tx pgx.Tx) error {
		_, err := ingestion.RequestMetadataRefresh(context.Background(), tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var pending bool
	_ = e.pool.QueryRow(context.Background(), "SELECT pending FROM jobs WHERE key = $1", key).Scan(&pending)
	if !pending {
		t.Fatal("refresh requested during execution was not retained")
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

// extStars is a fake ingestion.StarRefresher for tests outside the package.
type extStars struct {
	res   ingestion.StarFetch
	calls int
}

func (f *extStars) FetchStars(context.Context, int64) (ingestion.StarFetch, error) {
	f.calls++
	return f.res, nil
}

func (f *extStars) StoreStars(context.Context, pgx.Tx, int64, []githubapp.Repo) error { return nil }

// heldFence is a claim fence that always holds: it just runs the commit function.
type heldFence struct{}

func (heldFence) Commit(_ context.Context, fn func(pgx.Tx) error) error { return fn(nil) }

// One durable per-credential retry-after paces both job kinds: a rate limit seen by the metadata refresh holds back
// the same user's star job (no GitHub call), a star rate limit holds back that user's metadata refresh, other users
// are unaffected, and the throttle survives a new Deps (a restart).
func TestCredentialThrottleIsSharedAcrossJobKindsAndRestarts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a, b := e.signIn("alice"), e.signIn("carol")
	id := e.gh.AddRepo("bob", "one", false)
	e.store(a, id, false)
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	e.gh.LimitUntil(until)
	var d *jobs.Defer
	if err := e.run(e.fence(), id); !errors.As(err, &d) {
		t.Fatalf("metadata rate limit: %v", err)
	}
	stars := &extStars{res: ingestion.StarFetch{Completed: true}}
	q := db.New(e.pool)
	h := ingestion.StarReconcileHandler(stars, wallClock{}, q)
	d = nil
	if err := h(ctx, jobs.Job{Key: fmt.Sprintf("user:%d", a)}, nil); !errors.As(err, &d) || !d.RetryAt.Equal(until) || stars.calls != 0 {
		t.Fatalf("star job for the throttled user: err %v, fetches %d", err, stars.calls)
	}
	if err := h(ctx, jobs.Job{Key: fmt.Sprintf("user:%d", b)}, heldFence{}); err != nil || stars.calls != 1 {
		t.Fatalf("other user's star job: err %v, fetches %d", err, stars.calls)
	}
	// A star rate limit holds back that user's metadata refresh (even through a fresh Deps).
	retry := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	limited := &extStars{res: ingestion.StarFetch{RetryAt: &retry}}
	d = nil
	if err := ingestion.StarReconcileHandler(limited, wallClock{}, q)(ctx, jobs.Job{Key: fmt.Sprintf("user:%d", b)}, nil); !errors.As(err, &d) {
		t.Fatalf("star rate limit: %v", err)
	}
	e.gh.LimitUntil(time.Time{})
	e.store(b, id+1, false)
	e.gh.AddRepo("bob", "two", false)
	fresh := &ingestion.Deps{Pool: e.pool, GitHub: e.deps.GitHub, Tokens: e.deps.Tokens, Access: e.deps.Access, Clock: wallClock{}}
	before := hits(e, id+1)
	d = nil
	if err := fresh.RefreshMetadata(ctx, jobs.Job{Kind: jobs.KindRepoMetadataRefresh, Key: fmt.Sprintf("repo:%d", id+1)}, e.fence()); !errors.As(err, &d) || !d.RetryAt.Equal(retry) || hits(e, id+1) != before {
		t.Fatalf("metadata refresh for a star-throttled user: err %v", err)
	}
}
