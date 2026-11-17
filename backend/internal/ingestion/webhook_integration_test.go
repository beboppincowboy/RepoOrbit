//go:build integration

package ingestion

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

type bumped struct {
	scope accessfence.Scope
	id    int64
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
			r.bumps = append(r.bumps, bumped{s, id})
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

func notImplemented(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not implemented")
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
		{accessfence.ScopeInstallation, 7}, {accessfence.ScopeInstallation, 8}, {accessfence.ScopeInstallation, 9},
		{accessfence.ScopeUser, uid},
		{accessfence.ScopeRepository, 42}, {accessfence.ScopeRepository, 43}, {accessfence.ScopeRepository, 44},
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
	h := WebhookHandler(d)
	if w := post(h, testSecret, "installation", "bf-1", `{"action":"deleted","installation":{"id":7}}`, ""); w.Code < 500 {
		t.Fatalf("status %d, want 5xx", w.Code)
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
		if notImplemented(err) {
			t.Skip("needs U3b: " + err.Error())
		}
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
			if notImplemented(errors.New(w.Body.String())) {
				t.Skip("needs U3b: " + w.Body.String())
			}
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
	if notImplemented(err) {
		t.Skip("needs U3a/U3b: " + err.Error())
	}
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
