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
