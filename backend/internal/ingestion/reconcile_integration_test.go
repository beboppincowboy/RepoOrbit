//go:build integration

package ingestion

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

// txFence is a stand-in jobs.ClaimFence over a real transaction (the real claim check belongs to U3a).
type txFence struct{ pool *pgxpool.Pool }

func (f txFence) Commit(ctx context.Context, fn func(pgx.Tx) error) error {
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

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *db.Queries
}

func newFixture(t *testing.T) *fixture {
	_, pool := testutil.Database(t)
	return &fixture{t, pool, db.New(pool)}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) user(id int64) {
	f.exec(`INSERT INTO users (id, github_id, login, access_token_enc) VALUES ($1, $1, 'u', '\x00')`, id)
}

func (f *fixture) repo(id int64) {
	f.exec(`INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, 'o/r', '', false, false)`, id)
}

func (f *fixture) selection(user, repo int64, source string) {
	f.exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1,$2,$3,now())`, user, repo, source)
}

func (f *fixture) star(user, repo int64) {
	f.exec(`INSERT INTO stars (user_id, repository_id, full_name, description, is_private) VALUES ($1,$2,'o/r','',false)`, user, repo)
}

func (f *fixture) suspended(user, repo int64) bool {
	f.t.Helper()
	var s bool
	if err := f.pool.QueryRow(context.Background(), `SELECT follow_suspended_at IS NOT NULL FROM selections WHERE user_id=$1 AND repository_id=$2`, user, repo).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) jobState(kind jobs.Kind, key string) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT state FROM jobs WHERE kind=$1 AND key=$2`, string(kind), key).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// Unstar semantics, queries only (no queue needed): suspend a starred selection that left the snapshot without
// deleting it; never touch owned ones; restore when starred again.
func TestUnstarSuspendsFollowWithoutDeletingSelection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(1)
	for _, id := range []int64{10, 11, 12} {
		f.repo(id)
	}
	f.selection(1, 10, "starred") // still starred
	f.selection(1, 11, "starred") // unstarred
	f.selection(1, 12, "owned")   // not a star at all
	f.star(1, 10)
	now := time.Now()
	if err := f.q.SuspendUnstarredFollows(ctx, db.SuspendUnstarredFollowsParams{UserID: 1, Now: sTs(now)}); err != nil {
		t.Fatal(err)
	}
	if f.suspended(1, 10) || !f.suspended(1, 11) || f.suspended(1, 12) {
		t.Fatalf("suspended: %v %v %v", f.suspended(1, 10), f.suspended(1, 11), f.suspended(1, 12))
	}
	ids, err := f.q.ListFollowedRepositoryIDs(ctx)
	if err != nil || len(ids) != 2 || ids[0] != 10 || ids[1] != 12 {
		t.Fatalf("poll set %v %v (the unstarred repo must leave the poll set)", ids, err)
	}
	f.star(1, 11)
	if err := f.q.RestoreStarredFollows(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if f.suspended(1, 11) {
		t.Fatal("restoring after a star came back failed")
	}
}

// Another eligible selector keeps the repository in the poll set when one user unstars.
func TestOtherEligibleSelectorKeepsRepositoryPolled(t *testing.T) {
	f := newFixture(t)
	f.user(1)
	f.user(2)
	f.repo(10)
	f.selection(1, 10, "starred")
	f.selection(2, 10, "starred")
	f.star(2, 10)
	if err := f.q.SuspendUnstarredFollows(context.Background(), db.SuspendUnstarredFollowsParams{UserID: 1, Now: sTs(time.Now())}); err != nil {
		t.Fatal(err)
	}
	ids, _ := f.q.ListFollowedRepositoryIDs(context.Background())
	if len(ids) != 1 || ids[0] != 10 || !f.suspended(1, 10) || f.suspended(2, 10) {
		t.Fatalf("poll set %v", ids)
	}
	if err := f.q.SuspendUnstarredFollows(context.Background(), db.SuspendUnstarredFollowsParams{UserID: 2, Now: sTs(time.Now())}); err != nil {
		t.Fatal(err) // idempotent for a still-starred follow
	}
}

// A failed or incomplete refresh never reaches the fence, so eligibility is preserved (fence is nil: it would panic).
func TestFailedRefreshPreservesEligibility(t *testing.T) {
	f := newFixture(t)
	f.user(1)
	f.repo(10)
	f.selection(1, 10, "starred") // not in any snapshot, yet must stay eligible after a failure
	for _, s := range []*fakeStars{{}, {err: errors.New("net")}, {err: ErrNoCredential}} {
		_ = StarReconcileHandler(s, &fakeClock{now: time.Now()}, nil)(context.Background(), jobs.Job{Key: "user:1"}, nil)
	}
	if f.suspended(1, 10) {
		t.Fatal("failed refresh suspended eligibility")
	}
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// realStore stores through the real repositories.Service (snapshot replacement, suspend/restore, eligibility hook),
// with a fake fetch.
type realStore struct {
	fakeStars
	svc *repositories.Service
}

func (r *realStore) StoreStars(ctx context.Context, tx pgx.Tx, userID int64, repos []githubapp.Repo) error {
	return r.svc.StoreStars(ctx, tx, userID, repos)
}

// Completed refresh: unstarred follow suspended, still-starred one kept, paused work for the user resumed, all in the
// one fenced commit; a lost claim leaves the snapshot, eligibility and paused work exactly as they were.
func TestCompletedRefreshCommitsSuspensionAndResumes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(1)
	f.repo(10)
	f.repo(11)
	f.selection(1, 10, "starred")
	f.selection(1, 11, "starred")
	f.star(1, 10)
	f.star(1, 11)
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	tx, _ := f.begin()
	noErr(t, jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, repoKey(10), now))
	noErr(t, tx.Commit(ctx))
	f.exec(`UPDATE jobs SET state='paused'`)
	svc := &repositories.Service{Queries: f.q, Pool: f.pool, Now: func() time.Time { return now }}
	svc.OnEligibilityChange = func(ctx context.Context, tx pgx.Tx, userID int64) error {
		return RecheckEligibility(ctx, tx, userID, now)
	}
	stars := &realStore{fakeStars: fakeStars{res: StarFetch{Complete: true, Repos: []githubapp.Repo{{ID: 10, FullName: "o/r"}}}}, svc: svc}
	// Lost claim after a successful fetch: nothing is written.
	if err := StarReconcileHandler(stars, &fakeClock{now: now}, nil)(ctx, jobs.Job{Key: "user:1"}, onceFence{lost: true}); !errors.Is(err, jobs.ErrLostClaim) {
		t.Fatalf("lost claim: %v", err)
	}
	if f.suspended(1, 11) || f.jobState(jobs.KindRepoMetadataRefresh, repoKey(10)) != "paused" {
		t.Fatal("a lost claim changed eligibility or resumed work")
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM stars WHERE user_id = 1`).Scan(&n)
	if n != 2 {
		t.Fatalf("a lost claim replaced the snapshot (%d rows)", n)
	}
	// Held claim: snapshot replaced, unstarred follow suspended, paused work resumed.
	err := StarReconcileHandler(stars, &fakeClock{now: now}, nil)(ctx, jobs.Job{Key: "user:1"}, txFence{f.pool})
	noErr(t, err)
	if f.suspended(1, 10) || !f.suspended(1, 11) {
		t.Fatal("suspension wrong")
	}
	if got := f.jobState(jobs.KindRepoMetadataRefresh, repoKey(10)); got != "queued" {
		t.Fatalf("paused job after recheck: %s", got)
	}
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM stars WHERE user_id = 1`).Scan(&n)
	if n != 1 {
		t.Fatalf("snapshot not replaced (%d rows)", n)
	}
}

// Polls and recheck must never revive a dead metadata refresh (the dead-work barrier).
func TestPollAndRecheckDoNotReviveDeadWork(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(1)
	f.repo(10)
	f.selection(1, 10, "owned")
	now := time.Now()
	tx, _ := f.begin()
	noErr(t, jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, repoKey(10), now))
	noErr(t, tx.Commit(ctx))
	f.exec(`UPDATE jobs SET state='dead'`)
	d := SchedulerDeps{Pool: f.pool, Rand: func() float64 { return 0 }}
	noErr(t, d.pollMetadata(ctx, now))
	tx, _ = f.begin()
	noErr(t, RecheckEligibility(ctx, tx, 1, now))
	noErr(t, tx.Commit(ctx))
	if got := f.jobState(jobs.KindRepoMetadataRefresh, repoKey(10)); got != "dead" {
		t.Fatalf("dead job became %s", got)
	}
}

// Paused work resumes after an eligibility change; a poll also resumes it (missed-signal recovery). A user with no
// followed repositories resumes nothing of someone else's.
func TestRecheckResumesPausedWorkOnlyForTheUsersRepositories(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(1)
	f.user(2)
	f.repo(10)
	f.repo(11)
	f.selection(1, 10, "owned")
	f.selection(2, 11, "owned")
	now := time.Now()
	tx, _ := f.begin()
	for _, id := range []int64{10, 11} {
		noErr(t, jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, repoKey(id), now))
	}
	noErr(t, tx.Commit(ctx))
	f.exec(`UPDATE jobs SET state='paused'`)
	tx, _ = f.begin()
	noErr(t, RecheckEligibility(ctx, tx, 1, now))
	noErr(t, tx.Commit(ctx))
	if a, b := f.jobState(jobs.KindRepoMetadataRefresh, repoKey(10)), f.jobState(jobs.KindRepoMetadataRefresh, repoKey(11)); a != "queued" || b != "paused" {
		t.Fatalf("states %s %s", a, b)
	}
	noErr(t, SchedulerDeps{Pool: f.pool, Rand: func() float64 { return 0 }}.pollMetadata(ctx, now))
	if b := f.jobState(jobs.KindRepoMetadataRefresh, repoKey(11)); b != "queued" {
		t.Fatalf("poll did not recover missed signal: %s", b)
	}
}

func TestLoadMetadataStatusesFromJobsAndSync(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(1)
	f.repo(10)
	f.repo(11)
	f.selection(1, 10, "owned")
	f.selection(1, 11, "starred")
	f.exec(`INSERT INTO repository_sync (repository_id, last_success_at, last_error_code) VALUES (10, now(), 'github_unavailable')`)
	got, err := LoadMetadataStatuses(ctx, f.q, 1, []int64{10, 11, 99})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if s := got[10]; s.State != StatusNotScheduled || s.LastSuccessAt == nil || s.FailureCode == nil || *s.FailureCode != "github_unavailable" {
		t.Fatalf("%+v", s)
	}
	tx, _ := f.begin()
	noErr(t, jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, repoKey(11), time.Now()))
	noErr(t, tx.Commit(ctx))
	f.exec(`UPDATE jobs SET state='paused'`)
	got, _ = LoadMetadataStatuses(ctx, f.q, 1, []int64{11})
	if !got[11].Paused || got[11].State != "paused" {
		t.Fatalf("%+v", got[11])
	}
}

// begin opens a transaction rolled back at test end, so a skipped test never pins a pooled connection.
func (f *fixture) begin() (pgx.Tx, error) {
	tx, err := f.pool.Begin(context.Background())
	if err == nil {
		f.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	}
	return tx, err
}

// The stored retry-after only moves later, and a past one never throttles.
func TestCredentialThrottleKeepsTheLaterTimeAndIgnoresThePast(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(1)
	now := time.Now().Truncate(time.Second)
	if !credentialThrottledUntil(ctx, f.q, 1).IsZero() {
		t.Fatal("no row must mean not throttled")
	}
	throttleCredential(ctx, f.q, 1, now.Add(2*time.Hour))
	throttleCredential(ctx, f.q, 1, now.Add(time.Hour)) // earlier: must not shorten the pause
	if got := credentialThrottledUntil(ctx, f.q, 1); !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("until = %v, want the later time", got)
	}
	throttleCredential(ctx, f.q, 1, now.Add(3*time.Hour))
	if got := credentialThrottledUntil(ctx, f.q, 1); !got.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("until = %v, want the newer later time", got)
	}
	throttleCredential(ctx, f.q, 1, now.Add(-time.Hour))
	f.exec(`UPDATE credential_throttle SET retry_after = $1`, now.Add(-time.Hour))
	if until := credentialThrottledUntil(ctx, f.q, 1); until.After(now) {
		t.Fatalf("a past retry-after throttles: %v", until)
	}
}

// Terminal issue-write operations are deleted after the 30-day retention period. An unresolved create is kept past
// it, redacted, so a retry of the same content still finds it and cannot create a second issue.
func TestWriteOperationRetention(t *testing.T) {
	f := newFixture(t)
	f.user(1)
	f.repo(10)
	now := time.Now()
	old, recent, freshPending, stalePending := now.Add(-31*24*time.Hour), now.Add(-29*24*time.Hour), now.Add(-50*time.Second), now.Add(-65*time.Second)
	f.exec(`INSERT INTO write_operations (user_id, repository_id, idempotency_key, kind, request_hash, content_hash, draft, state, outcome, created_at) VALUES
	 (1, 10, 'old-unknown', 'create', '\x00', '\x01', '{"title":"secret"}', 'unknown', '{"issue":{"number":1,"title":"secret"}}', $1),
	 (1, 10, 'old-failed', 'create', '\x00', '\x02', NULL, 'failed', '{}', $1),
	 (1, 10, 'old-succeeded', 'update', '\x00', NULL, NULL, 'succeeded', '{}', $1),
	 (1, 10, 'recent', 'create', '\x00', '\x03', '{"title":"recent"}', 'failed', '{}', $2),
	 (1, 10, 'fresh-pending', 'update', '\x00', NULL, '{"state":"closed"}', 'pending', '{}', $3),
	 (1, 10, 'stale-pending', 'update', '\x00', NULL, '{"state":"closed"}', 'pending', '{}', $4)`, old, recent, freshPending, stalePending)
	noErr(t, SchedulerDeps{Pool: f.pool}.expireWriteOperations(context.Background(), now))
	rows, err := f.pool.Query(context.Background(), `SELECT idempotency_key || ':' || state || ':' || (draft IS NULL)::text || ':' || (outcome ? 'redacted')::text || ':' || (outcome::text LIKE '%secret%')::text || ':' || (content_hash IS NOT NULL)::text FROM write_operations ORDER BY idempotency_key`)
	noErr(t, err)
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	noErr(t, err)
	if want := []string{
		"fresh-pending:pending:false:false:false:false",
		"old-unknown:unknown:true:true:false:true",
		"recent:failed:false:false:false:true",
		"stale-pending:failed:false:false:false:false",
	}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
}
