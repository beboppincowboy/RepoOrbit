//go:build integration

package ingestion_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

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
	stars := &extStars{res: ingestion.StarFetch{Complete: true}}
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
