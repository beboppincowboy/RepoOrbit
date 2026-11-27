//go:build integration

package readguard

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

type guardHarness struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	gh       *fake.Server
	repos    *repositories.Service
	guard    *Guard
	userID   int64
	ghCounts *atomic.Int64
}

func newGuardHarness(t *testing.T) *guardHarness {
	t.Helper()
	ctx := context.Background()
	_, pool := testutil.Database(t)
	gh := fake.New("cid", "secret")
	var ghCounts atomic.Int64
	ghHandler := gh.Handler()
	gs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/user/") {
			ghCounts.Add(1)
		}
		ghHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(gs.Close)

	client := githubapp.New("cid", "secret", gs.URL, gs.URL, "http://app.test/auth/github/callback")
	q := db.New(pool)
	authSvc := &auth.Service{Pool: pool, Queries: q, GitHub: client, Key: bytes.Repeat([]byte{1}, 32)}
	repos := &repositories.Service{Pool: pool, Queries: q, GitHub: client, TokenSource: authSvc, TTL: time.Nanosecond}

	// Sign in alice through auth.Handler using a code from fake GitHub.
	ah := &auth.Handler{Service: authSvc, AppURL: "http://app.test"}
	mux := http.NewServeMux()
	mux.Handle("/auth/", ah.Routes())
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)

	httpClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	loginResp, err := httpClient.Get(api.URL + "/auth/github/login")
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	stateCookie := loginResp.Cookies()[0]
	authorizeResp, err := httpClient.Get(loginResp.Header.Get("Location") + "&as=alice")
	if err != nil {
		t.Fatal(err)
	}
	authorizeResp.Body.Close()
	cbURL := strings.Replace(authorizeResp.Header.Get("Location"), "http://app.test", api.URL, 1)
	cbReq, err := http.NewRequest(http.MethodGet, cbURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	cbReq.AddCookie(stateCookie)
	cbResp, err := httpClient.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	cbResp.Body.Close()

	var userID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE login='alice'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	return &guardHarness{
		pool:     pool,
		q:        q,
		gh:       gh,
		repos:    repos,
		guard:    &Guard{Pool: pool, Queries: q, Access: repos},
		userID:   userID,
		ghCounts: &ghCounts,
	}
}

func TestReadGuardMidReadFenceMove(t *testing.T) {
	ctx := context.Background()
	h := newGuardHarness(t)
	repoID := h.gh.AddRepo("alice", "guarded", true)
	h.gh.Install("alice", 10, repoID)
	if err := h.repos.Connect(ctx, h.userID, repoID); err != nil {
		t.Fatal(err)
	}

	authRes, err := h.guard.AuthorizeSelected(ctx, h.userID, []int64{repoID})
	if err != nil {
		t.Fatalf("AuthorizeSelected: %v", err)
	}
	if len(authRes.Allowed) != 1 || authRes.Allowed[0] != repoID || authRes.Withheld != 0 {
		t.Fatalf("unexpected auth: %+v", authRes)
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE activity_issues IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		val   string
		stale bool
		err   error
	}
	ch := make(chan outcome, 1)
	go func() {
		staleCalled := false
		v, err := Run(ctx, h.pool, authRes,
			func() (string, error) {
				var count int
				if err := h.pool.QueryRow(ctx, `/* readguard_mid_read */ SELECT count(*) FROM activity_issues WHERE repository_id = $1`, repoID).Scan(&count); err != nil {
					return "", err
				}
				return "secret-payload", nil
			},
			func() (string, error) {
				staleCalled = true
				return "discarded", nil
			},
		)
		ch <- outcome{val: v, stale: staleCalled, err: err}
	}()

	blocked := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !blocked; time.Sleep(10 * time.Millisecond) {
		if err := h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%readguard_mid_read%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
	}
	if !blocked {
		t.Fatal("guarded read did not block on locked table")
	}

	fenceTx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := accessfence.InvalidateAccess(ctx, fenceTx, accessfence.ScopeUser, h.userID); err != nil {
		t.Fatal(err)
	}
	if err := fenceTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got := <-ch
	if got.err != nil || !got.stale || got.val != "discarded" {
		t.Fatalf("expected stale discard, got %+v", got)
	}
}

func TestReadGuardZeroAllowedStillRunsAndFences(t *testing.T) {
	ctx := context.Background()
	h := newGuardHarness(t)

	authRes, err := h.guard.AuthorizePreselected(ctx, h.userID, nil)
	if err != nil {
		t.Fatalf("AuthorizePreselected: %v", err)
	}
	if len(authRes.Allowed) != 0 || authRes.Withheld != 0 {
		t.Fatalf("unexpected auth: %+v", authRes)
	}

	readRan := false
	got, err := Run(ctx, h.pool, authRes,
		func() (string, error) {
			readRan = true
			return "empty-overview", nil
		},
		func() (string, error) {
			return "", errors.New("unexpected stale")
		},
	)
	if err != nil || !readRan || got != "empty-overview" {
		t.Fatalf("zero-allowed read: readRan=%v got=%q err=%v", readRan, got, err)
	}

	// Moving the fence during a zero-allowed read still triggers onStale.
	staleRan := false
	got, err = Run(ctx, h.pool, authRes,
		func() (string, error) {
			fenceTx, err := h.pool.Begin(ctx)
			if err != nil {
				return "", err
			}
			if err := accessfence.InvalidateAccess(ctx, fenceTx, accessfence.ScopeUser, h.userID); err != nil {
				_ = fenceTx.Rollback(ctx)
				return "", err
			}
			if err := fenceTx.Commit(ctx); err != nil {
				return "", err
			}
			return "should-discard", nil
		},
		func() (string, error) {
			staleRan = true
			return "stale", nil
		},
	)
	if err != nil || !staleRan || got != "stale" {
		t.Fatalf("zero-allowed stale check: staleRan=%v got=%q err=%v", staleRan, got, err)
	}
}

func TestReadGuardDatabaseError(t *testing.T) {
	ctx := context.Background()
	h := newGuardHarness(t)
	h.pool.Close()

	for name, fn := range map[string]func() (Auth, error){
		"AuthorizeSelected":    func() (Auth, error) { return h.guard.AuthorizeSelected(ctx, h.userID, []int64{1}) },
		"AuthorizePreselected": func() (Auth, error) { return h.guard.AuthorizePreselected(ctx, h.userID, []int64{1}) },
		"FenceOnly":            func() (Auth, error) { return h.guard.FenceOnly(ctx, h.userID) },
	} {
		t.Run(name, func(t *testing.T) {
			authRes, err := fn()
			if !errors.Is(err, ErrDatabaseUnavailable) || err.Error() != "database unavailable" {
				t.Fatalf("expected ErrDatabaseUnavailable, got %v", err)
			}
			if authRes.Withheld != 0 || len(authRes.Allowed) != 0 {
				t.Fatalf("expected zero auth on db error, got %+v", authRes)
			}
		})
	}
}

func TestReadGuardRequestVisibilityMemo(t *testing.T) {
	ctx := repositories.WithRequestVisibility(context.Background())
	h := newGuardHarness(t)
	repoID := h.gh.AddRepo("alice", "memo", true)
	h.gh.Install("alice", 11, repoID)
	if err := h.repos.Connect(ctx, h.userID, repoID); err != nil {
		t.Fatal(err)
	}

	h.ghCounts.Store(0)
	first, err := h.guard.AuthorizeSelected(ctx, h.userID, []int64{repoID})
	if err != nil || len(first.Allowed) != 1 {
		t.Fatalf("first auth: %+v err=%v", first, err)
	}
	afterFirst := h.ghCounts.Load()
	if afterFirst == 0 {
		t.Fatal("expected initial GitHub access check")
	}
	second, err := h.guard.AuthorizeSelected(ctx, h.userID, []int64{repoID})
	if err != nil || len(second.Allowed) != 1 {
		t.Fatalf("second auth: %+v err=%v", second, err)
	}
	if got := h.ghCounts.Load(); got != afterFirst {
		t.Fatalf("expected memoized visibility check (calls=%d), got %d", afterFirst, got)
	}
}
