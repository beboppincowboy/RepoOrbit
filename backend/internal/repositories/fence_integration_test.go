//go:build integration

package repositories_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

// A process-local cached listing must not outlive an access-fence bump, even within its TTL.
func TestCachedPrivateReadHonorsFenceBump(t *testing.T) {
	e := setup(t, time.Hour)
	u := e.signIn("alice")
	id := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 9, id)
	ctx := context.Background()
	if d := e.repos.CheckOne(ctx, u.id, id, false); !d.Allowed {
		t.Fatalf("setup: %+v", d)
	}
	e.gh.Uninstall(9) // remote change nobody has told us about yet
	if d := e.repos.CheckOne(ctx, u.id, id, false); !d.Allowed {
		t.Fatal("cache should still serve inside the TTL without a bump")
	}
	tx, _ := e.pool.Begin(ctx)
	if err := accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeInstallation, 9); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if d := e.repos.CheckOne(ctx, u.id, id, false); d.Allowed {
		t.Fatal("cached private read survived an installation bump")
	}
}

func TestRevokeBumpsUserFence(t *testing.T) {
	e := setup(t, time.Hour)
	u := e.signIn("alice")
	ctx := context.Background()
	var before, after int64
	read := func(n *int64) {
		if err := e.pool.QueryRow(ctx, `SELECT COALESCE((SELECT epoch FROM access_fences WHERE scope = 'user' AND subject_id = $1), 0)`, u.id).Scan(n); err != nil {
			t.Fatal(err)
		}
	}
	read(&before)
	e.gh.RevokeUser("alice")
	if d := e.repos.CheckOne(ctx, u.id, 999999, true); d.Allowed {
		t.Fatal("revoked user allowed")
	}
	read(&after)
	if after <= before {
		t.Fatalf("epoch %d -> %d", before, after)
	}
}

// Selecting commits the scheduling step in the same transaction; a failing step leaves no selection behind.
func TestConnectRollsBackWhenSchedulingFails(t *testing.T) {
	e := setup(t, time.Hour)
	u := e.signIn("alice")
	id := e.gh.AddRepo("alice", "mine", false)
	boom := errors.New("enqueue failed")
	e.repos.OnSelect = func(context.Context, pgx.Tx, int64) error { return boom }
	if err := e.repos.Connect(context.Background(), u.id, id); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), "SELECT count(*) FROM selections").Scan(&n)
	if n != 0 {
		t.Fatal("selection survived")
	}
}

// A revocation committed while a fresh listing is still being fetched must withhold that very response, not
// only later cached reads.
func TestFreshReadWithholdsWhenFenceBumpsDuringFetch(t *testing.T) {
	e := setup(t, time.Hour)
	u := e.signIn("alice")
	id := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 9, id)
	ctx := context.Background()
	var once sync.Once
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/installations" {
			once.Do(func() {
				tx, _ := e.pool.Begin(ctx)
				if err := accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeUser, u.id); err != nil {
					t.Error(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Error(err)
				}
			})
		}
		e.gh.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(ghSrv.Close)
	e.repos.GitHub = githubapp.New("cid", "secret", ghSrv.URL, ghSrv.URL, appURL+"/auth/github/callback")
	if d := e.repos.CheckOne(ctx, u.id, id, true); d.Allowed {
		t.Fatal("fresh private read returned data fetched before a committed revocation")
	}
}
