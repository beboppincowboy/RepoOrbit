//go:build integration

package ingestion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

type bumped struct {
	scope accessfence.Scope
	id    int64
	grant bool // InvalidateInstallationGrant rather than InvalidateAccess
}

type recorder struct {
	refreshed  []int64
	bumps      []bumped
	refreshErr error
}

func (r *recorder) deps(pool *pgxpool.Pool) WebhookDeps {
	return WebhookDeps{
		Pool:   pool,
		Secret: testSecret,
		refresh: func(_ context.Context, _ pgx.Tx, id int64) (bool, error) {
			if r.refreshErr != nil {
				return false, r.refreshErr
			}
			r.refreshed = append(r.refreshed, id)
			return true, nil
		},
		bump: func(_ context.Context, _ pgx.Tx, s accessfence.Scope, id int64) error {
			r.bumps = append(r.bumps, bumped{scope: s, id: id})
			return nil
		},
		grant: func(_ context.Context, _ pgx.Tx, id int64) error {
			r.bumps = append(r.bumps, bumped{scope: accessfence.ScopeInstallation, id: id, grant: true})
			return nil
		},
	}
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const pushBody = `{"repository":{"id":42},"installation":{"id":7}}`

func TestDuplicateSignedDeliveryEnqueuesOnce(t *testing.T) {
	_, pool := testutil.Database(t)
	rec := &recorder{}
	h := WebhookHandler(rec.deps(pool))
	for i := 0; i < 2; i++ {
		if w := post(h, testSecret, "push", "dup-1", pushBody, ""); w.Code != 200 {
			t.Fatalf("delivery %d: status %d", i, w.Code)
		}
	}
	if len(rec.refreshed) != 1 || rec.refreshed[0] != 42 {
		t.Fatalf("refreshes %v, want exactly [42]", rec.refreshed)
	}
	if n := count(t, pool, "webhook_deliveries"); n != 1 {
		t.Fatalf("%d delivery rows", n)
	}
}

func TestInvalidOrOversizedStoresNothing(t *testing.T) {
	_, pool := testutil.Database(t)
	rec := &recorder{}
	h := WebhookHandler(rec.deps(pool))
	post(h, testSecret, "push", "bad-1", pushBody, "sha256="+strings.Repeat("0", 64))
	post(h, testSecret, "push", "big-1", strings.Repeat("x", maxWebhookBody+1), "")
	if len(rec.refreshed) != 0 || count(t, pool, "webhook_deliveries") != 0 {
		t.Fatal("rejected input produced work")
	}
}

func TestEnqueueFailureIsNotAcknowledgedAndRedeliveryWorks(t *testing.T) {
	_, pool := testutil.Database(t)
	rec := &recorder{refreshErr: errors.New("queue down")}
	h := WebhookHandler(rec.deps(pool))
	if w := post(h, testSecret, "push", "retry-1", pushBody, ""); w.Code < 500 {
		t.Fatalf("status %d, want 5xx", w.Code)
	}
	if n := count(t, pool, "webhook_deliveries"); n != 0 {
		t.Fatalf("failed delivery was recorded (%d rows); redelivery would be dropped as duplicate", n)
	}
	rec.refreshErr = nil
	if w := post(h, testSecret, "push", "retry-1", pushBody, ""); w.Code != 200 {
		t.Fatalf("redelivery status %d", w.Code)
	}
	if len(rec.refreshed) != 1 {
		t.Fatalf("refreshes %v", rec.refreshed)
	}
}

func TestUnsupportedAcknowledgedWithoutWorkOrRow(t *testing.T) {
	_, pool := testutil.Database(t)
	rec := &recorder{}
	h := WebhookHandler(rec.deps(pool))
	if w := post(h, testSecret, "star", "u-1", `{"repository":{"id":42}}`, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	// repository events that are not metadata or security changes (e.g. created) are also work-free.
	if w := post(h, testSecret, "repository", "u-2", `{"action":"created","repository":{"id":42}}`, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if len(rec.refreshed)+len(rec.bumps) != 0 || count(t, pool, "webhook_deliveries") != 0 {
		t.Fatal("unsupported delivery produced work")
	}
}

// Security events bump durably with no selections and no job at all, and never write access.
func TestSecurityEventsBumpWithoutSelectionsOrGrantingAccess(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO users (github_id, login, access_token_enc) VALUES (555, 'octo', '\x')`); err != nil {
		t.Fatal(err)
	}
	var uid int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE github_id = 555`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	h := WebhookHandler(rec.deps(pool))
	for i, tc := range []struct{ event, body string }{
		{"installation", `{"action":"created","installation":{"id":7},"sender":{"id":555}}`},
		{"installation", `{"action":"deleted","installation":{"id":8}}`},
		{"installation_repositories", `{"action":"removed","installation":{"id":9}}`},
		{"github_app_authorization", `{"action":"revoked","sender":{"id":555}}`},
		{"github_app_authorization", `{"action":"revoked","sender":{"id":999999}}`}, // unknown user: ack, nothing to bump
		{"repository", `{"action":"privatized","repository":{"id":42}}`},
		{"repository", `{"action":"deleted","repository":{"id":43}}`},
		{"repository", `{"action":"transferred","repository":{"id":44}}`},
	} {
		if w := post(h, testSecret, tc.event, "sec-"+string(rune('a'+i)), tc.body, ""); w.Code != 200 {
			t.Fatalf("%s %s: status %d", tc.event, tc.body, w.Code)
		}
	}
	want := []bumped{
		{accessfence.ScopeInstallation, 7, true}, {accessfence.ScopeInstallation, 8, false}, {accessfence.ScopeInstallation, 9, false},
		{accessfence.ScopeUser, uid, false},
		{accessfence.ScopeRepository, 42, false}, {accessfence.ScopeRepository, 43, false}, {accessfence.ScopeRepository, 44, false},
	}
	if len(rec.bumps) != len(want) {
		t.Fatalf("bumps %v, want %v", rec.bumps, want)
	}
	for i := range want {
		if rec.bumps[i] != want[i] {
			t.Fatalf("bump %d = %v, want %v", i, rec.bumps[i], want[i])
		}
	}
	// A (possibly delayed) installation "created" never grants anything.
	if n := count(t, pool, "installations") + count(t, pool, "selections"); n != 0 {
		t.Fatalf("webhook wrote %d access rows", n)
	}
}

func TestSecurityBumpFailureIsNotAcknowledged(t *testing.T) {
	_, pool := testutil.Database(t)
	d := (&recorder{}).deps(pool)
	d.bump = func(context.Context, pgx.Tx, accessfence.Scope, int64) error { return errors.New("db down") }
	d.grant = func(context.Context, pgx.Tx, int64) error { return errors.New("db down") }
	h := WebhookHandler(d)
	for i, action := range []string{"deleted", "created"} {
		if w := post(h, testSecret, "installation", fmt.Sprintf("bf-%d", i), `{"action":"`+action+`","installation":{"id":7}}`, ""); w.Code < 500 {
			t.Fatalf("%s: status %d, want 5xx", action, w.Code)
		}
	}
	if n := count(t, pool, "webhook_deliveries"); n != 0 {
		t.Fatalf("%d delivery rows", n)
	}
}

// Real fence: skipped until U3b merges. A webhook after TakeSnapshot (revocation during a fetch) makes Valid false,
// even for an installation "created" event (delayed events cannot grant, and still invalidate).
func TestRealFenceBumpBlocksInFlightCommit(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	var uid, repoID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (github_id, login, access_token_enc) VALUES (556, 'octo', '\x') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	repoID = 42
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, name, description, is_private) VALUES (42, 'o/r', '', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations (user_id, installation_id, account_login) VALUES ($1, 7, 'octo')`, uid); err != nil {
		t.Fatal(err)
	}
	take := func() accessfence.Snapshot {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		s, err := accessfence.TakeSnapshot(ctx, tx, uid, repoID)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	valid := func(s accessfence.Snapshot) bool {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		ok, err := s.StillCurrent(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	for i, tc := range []struct{ event, body string }{
		{"installation", `{"action":"created","installation":{"id":7}}`},
		{"installation", `{"action":"deleted","installation":{"id":7}}`},
		{"repository", `{"action":"privatized","repository":{"id":42}}`},
		{"github_app_authorization", `{"action":"revoked","sender":{"id":556}}`},
	} {
		s := take()
		if !valid(s) {
			t.Fatalf("case %d: snapshot invalid before any event", i)
		}
		if w := post(h, testSecret, tc.event, "fence-"+string(rune('a'+i)), tc.body, ""); w.Code != 200 {
			t.Fatalf("case %d: status %d %s", i, w.Code, w.Body.String())
		}
		if valid(s) {
			t.Fatalf("case %d (%s): commit not blocked after webhook", i, tc.event)
		}
	}
}

// Real queue + selection scheduling: skipped until U3a/U3b merge. An unselected repository is never scheduled.
func TestRealRefreshOnlyForSelectedRepositories(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, name, description) VALUES (42, 'o/r', '')`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RequestMetadataRefresh(ctx, tx, 42)
	tx.Rollback(ctx)
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	w := post(h, testSecret, "push", "real-1", pushBody, "")
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d jobs for an unselected repository", n)
	}
}

func revocationFixture(t *testing.T) (pool *pgxpool.Pool, uid int64) {
	t.Helper()
	_, pool = testutil.Database(t)
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `INSERT INTO users (github_id, login, access_token_enc, refresh_token_enc) VALUES (555, 'octo', '\x0102', '\x0304') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ('\x01', $1, now(), now() + interval '1 day')`, uid); err != nil {
		t.Fatal(err)
	}
	return pool, uid
}

func tokenAndSessionState(t *testing.T, pool *pgxpool.Pool, uid int64) (tokenLen, sessions int) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT octet_length(access_token_enc) FROM users WHERE id = $1`, uid).Scan(&tokenLen); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, uid).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	return
}

// A signed revocation ends the user's credential and sessions, not only their cached reads, in the same transaction
// as the fence bump; redelivery changes nothing and an unknown sender is acknowledged with no effect.
func TestAuthorizationRevocationClearsCredentialAndSessions(t *testing.T) {
	pool, uid := revocationFixture(t)
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	body := `{"action":"revoked","sender":{"id":555}}`
	if w := post(h, testSecret, "github_app_authorization", "rev-1", body, ""); w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if tok, sess := tokenAndSessionState(t, pool, uid); tok != 0 || sess != 0 {
		t.Fatalf("token bytes %d, sessions %d; want both cleared", tok, sess)
	}
	var epoch int64
	if err := pool.QueryRow(context.Background(), `SELECT epoch FROM access_fences WHERE scope = 'user' AND subject_id = $1`, uid).Scan(&epoch); err != nil || epoch != 1 {
		t.Fatalf("user epoch %d err %v", epoch, err)
	}
	if w := post(h, testSecret, "github_app_authorization", "rev-1", body, ""); w.Code != 200 {
		t.Fatalf("redelivery status %d", w.Code)
	}
	if err := pool.QueryRow(context.Background(), `SELECT epoch FROM access_fences WHERE scope = 'user' AND subject_id = $1`, uid).Scan(&epoch); err != nil || epoch != 1 {
		t.Fatalf("redelivery bumped again: epoch %d", epoch)
	}
	if w := post(h, testSecret, "github_app_authorization", "rev-2", `{"action":"revoked","sender":{"id":999999}}`, ""); w.Code != 200 {
		t.Fatalf("unknown sender status %d", w.Code)
	}
}

// A failing cleanup rolls the whole delivery back: no bump without cleanup, no delivery row, so GitHub can redeliver.
func TestAuthorizationRevocationCleanupFailureRollsBack(t *testing.T) {
	pool, uid := revocationFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_clear() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER fail_clear BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION fail_clear()`); err != nil {
		t.Fatal(err)
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	if w := post(h, testSecret, "github_app_authorization", "rev-fail", `{"action":"revoked","sender":{"id":555}}`, ""); w.Code != 500 {
		t.Fatalf("status %d, want 500 so GitHub redelivers", w.Code)
	}
	if tok, sess := tokenAndSessionState(t, pool, uid); tok == 0 || sess != 1 {
		t.Fatalf("cleanup partly landed: token bytes %d, sessions %d", tok, sess)
	}
	if n := count(t, pool, "webhook_deliveries") + count(t, pool, "access_fences"); n != 0 {
		t.Fatalf("bump or delivery row survived the rollback (%d rows)", n)
	}
}

// A repository security webhook (fence bump, then enqueue on the repo's job row) must take locks in the same order as
// a private refresh commit (job row, then fence rows), or the two deadlock. The refresh side is replayed by hand:
// it holds the job row FOR SHARE (the claim check), lets the webhook run, then checks the permissions version.
func TestRepositoryWebhookAndRefreshCommitNeverDeadlock(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (github_id, login, access_token_enc) VALUES (555, 'octo', '\x01') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO repositories (id, name, description, is_private) VALUES (42, 'o/r', '', true)`,
		`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES (` + "1" + `, 42, 'owned', now())`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := pool.Begin(ctx)
	if err := jobs.Enqueue(ctx, tx, jobs.KindRepoMetadataRefresh, "repo:42", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	for i := 0; i < 5; i++ {
		var snap accessfence.Snapshot
		snapTx, _ := pool.Begin(ctx)
		snap, err := accessfence.TakeSnapshot(ctx, snapTx, uid, 42)
		if err != nil {
			t.Fatal(err)
		}
		_ = snapTx.Commit(ctx)
		refresh := make(chan error, 1)
		held := make(chan struct{})
		go func() {
			tx, err := pool.Begin(ctx)
			if err != nil {
				refresh <- err
				return
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SELECT 1 FROM jobs WHERE kind = 'repo_metadata_refresh' AND key = 'repo:42' FOR SHARE`); err != nil {
				refresh <- err
				return
			}
			close(held)
			time.Sleep(300 * time.Millisecond) // the webhook runs now
			if _, err := snap.StillCurrent(ctx, tx); err != nil {
				refresh <- err
				return
			}
			refresh <- tx.Commit(ctx)
		}()
		<-held
		w := post(h, testSecret, "repository", "dl-"+string(rune('a'+i)), `{"action":"privatized","repository":{"id":42}}`, "")
		if w.Code != 200 {
			t.Fatalf("round %d: webhook status %d %s", i, w.Code, w.Body.String())
		}
		if err := <-refresh; err != nil {
			t.Fatalf("round %d: refresh commit: %v", i, err)
		}
	}
}

// Only an installation event that can end access records the installation's holders as fallback draft-wipe targets;
// one that can only grant access (created, unsuspend, repositories added) still bumps the fence but records nobody.
func TestOnlyRevokingInstallationEventsRecordWipeTargets(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (github_id, login, access_token_enc) VALUES (557, 'octo', '\x') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, name, description, is_private) VALUES (42, 'o/r', '', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations (user_id, installation_id, account_login) VALUES ($1, 7, 'octo')`, uid); err != nil {
		t.Fatal(err)
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	for i, tc := range []struct {
		event, body string
		records     bool
	}{
		{"installation", `{"action":"created","installation":{"id":7}}`, false},
		{"installation", `{"action":"unsuspend","installation":{"id":7}}`, false},
		{"installation_repositories", `{"action":"added","installation":{"id":7},"repositories_removed":[]}`, false},
		{"installation_repositories", `{"action":"added","installation":{"id":7},"repositories_removed":[{"id":42}]}`, true},
		{"installation", `{"action":"new_permissions_accepted","installation":{"id":7}}`, true},
		{"installation_repositories", `{"action":"removed","installation":{"id":7}}`, true},
		{"installation", `{"action":"deleted","installation":{"id":7}}`, true},
	} {
		if _, err := pool.Exec(ctx, `DELETE FROM activity_cleanup`); err != nil {
			t.Fatal(err)
		}
		var before int64
		_ = pool.QueryRow(ctx, `SELECT COALESCE((SELECT epoch FROM access_fences WHERE scope = 'installation' AND subject_id = 7), 0)`).Scan(&before)
		if w := post(h, testSecret, tc.event, fmt.Sprintf("grant-%d", i), tc.body, ""); w.Code != 200 {
			t.Fatalf("case %d: status %d %s", i, w.Code, w.Body.String())
		}
		var epoch int64
		var wipe []int64
		if err := pool.QueryRow(ctx, `SELECT (SELECT epoch FROM access_fences WHERE scope = 'installation' AND subject_id = 7), wipe_users FROM activity_cleanup WHERE repository_id = 42`).Scan(&epoch, &wipe); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if epoch != before+1 {
			t.Fatalf("case %d: fence epoch %d, want %d", i, epoch, before+1)
		}
		if got := len(wipe) > 0; got != tc.records || tc.records && wipe[0] != uid {
			t.Fatalf("case %d %s %s: wipe_users %v", i, tc.event, tc.body, wipe)
		}
	}
}
