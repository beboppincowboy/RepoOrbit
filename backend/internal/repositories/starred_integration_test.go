//go:build integration

package repositories_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

func (u *user) starred(query string) repositories.StarredPage {
	u.e.t.Helper()
	var p repositories.StarredPage
	if r := u.json("GET", "/api/repositories/starred"+query, "", &p); r.StatusCode != 200 {
		u.e.t.Fatalf("starred%s: %d", query, r.StatusCode)
	}
	return p
}

func starredNames(p repositories.StarredPage) (names []string) {
	for _, r := range p.Repositories {
		names = append(names, r.FullName)
	}
	return names
}

// Public stars can be followed without an Installation and then appear in GraphQL; private ones need the user
// and an Installation; following needs the repository to be in the user's star snapshot; nothing else is stored.
func TestFollowStarredRepositories(t *testing.T) {
	e := setup(t, time.Nanosecond)
	pub := e.gh.AddRepo("bob", "lib", false)
	other := e.gh.AddRepo("bob", "unstarred", false)
	priv := e.gh.AddRepo("alice", "secret", true)
	e.gh.Star("alice", pub)
	e.gh.Star("alice", priv)
	alice := e.signIn("alice")
	follow := func(id int64) int {
		return alice.write("PUT", fmt.Sprintf("/api/repositories/%d/follow", id)).StatusCode
	}

	if got := follow(pub); got != 404 { // no snapshot yet: not known to be starred
		t.Fatalf("follow before any refresh: %d", got)
	}
	p := alice.starred("?refresh=1")
	if got := starredNames(p); len(got) != 1 || got[0] != "bob/lib" || p.RefreshedAt == nil || p.RefreshFailed {
		t.Fatalf("snapshot without installation (private star not readable yet): %+v", p)
	}
	if got := follow(other); got != 404 {
		t.Fatalf("follow of an unstarred repo: %d", got)
	}
	if got := follow(priv); got != 404 {
		t.Fatalf("follow of a private repo with no installation: %d", got)
	}
	if got := follow(pub); got != 200 {
		t.Fatalf("follow public star without installation: %d", got)
	}
	data, errs := alice.gql(repoQuery)
	if names, _, _ := repoNames(data); len(errs) != 0 || len(names) != 1 || names[0] != "bob/lib" {
		t.Fatalf("followed public repo in overview: %v %v", names, errs)
	}
	var rows int
	if err := e.pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM repositories) + (SELECT count(*) FROM selections WHERE source = 'starred')").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("stored rows: %d %v", rows, err)
	}

	e.gh.Install("alice", 1, priv)
	if got := starredNames(alice.starred("?refresh=1")); len(got) != 2 {
		t.Fatalf("after install: %v", got)
	}
	if got := follow(priv); got != 200 {
		t.Fatalf("follow private star with installation: %d", got)
	}
	// Unfollow reuses the selection endpoint and keeps the row soft-deleted.
	if got := alice.write("DELETE", fmt.Sprintf("/api/repositories/%d/selection", pub)).StatusCode; got != 200 {
		t.Fatalf("unfollow: %d", got)
	}
}

// A failed star refresh keeps the previous snapshot and its age. Public entries stay visible; private entries
// are hidden with a count until a live check passes. Paging reads the snapshot without calling GitHub.
func TestFailedStarRefreshKeepsSnapshot(t *testing.T) {
	e := setup(t, time.Nanosecond)
	for i := 0; i < 31; i++ {
		e.gh.Star("alice", e.gh.AddRepo("bob", fmt.Sprintf("r%02d", i), false))
	}
	priv := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 1, priv)
	e.gh.Star("alice", priv)
	alice := e.signIn("alice")

	first := alice.starred("?refresh=1")
	if len(first.Repositories) != 30 || first.NextPage != 2 || first.RefreshedAt == nil {
		t.Fatalf("page 1: %d next %d at %v", len(first.Repositories), first.NextPage, first.RefreshedAt)
	}
	calls := e.gh.Hits("/user/starred")
	if p2 := alice.starred("?page=2"); len(p2.Repositories) != 2 || p2.NextPage != 0 || e.gh.Hits("/user/starred") != calls {
		t.Fatalf("page 2 should come from the snapshot: %+v", p2)
	}

	e.gh.Fail(true)
	failed := alice.starred("?refresh=1")
	// alice/secret sorts first on page 1: it needs a live check, which fails, so it is withheld with a count.
	if !failed.RefreshFailed || len(failed.Repositories) != 29 || failed.Withheld != 1 || !failed.RefreshedAt.Equal(*first.RefreshedAt) {
		t.Fatalf("failed refresh: failed=%v n=%d withheld=%d at=%v want %v", failed.RefreshFailed, len(failed.Repositories), failed.Withheld, failed.RefreshedAt, first.RefreshedAt)
	}
	if starredNames(failed)[0] == "alice/secret" || failed.Repositories[0].Private {
		t.Fatalf("private star shown without a live check: %v", starredNames(failed))
	}
	e.gh.Fail(false)
	e.gh.LimitUntil(time.Now().Add(time.Hour))
	limited := alice.starred("?refresh=1")
	if !limited.RefreshFailed || limited.RetryAt == nil || limited.RefreshedAt == nil {
		t.Fatalf("rate limited refresh: %+v", limited)
	}
	e.gh.LimitUntil(time.Time{})
	if ok := alice.starred("?refresh=1"); ok.RefreshFailed || len(ok.Repositories) != 30 || ok.Withheld != 0 {
		t.Fatalf("recovered refresh: %+v", ok)
	}
}

// A user with no snapshot and a failing GitHub gets an explicit failure, not a claim of zero stars.
func TestFirstStarRefreshFailureIsNotEmptyList(t *testing.T) {
	e := setup(t, time.Nanosecond)
	alice := e.signIn("alice")
	e.gh.Fail(true)
	if p := alice.starred("?refresh=1"); !p.RefreshFailed || p.RefreshedAt != nil || len(p.Repositories) != 0 {
		t.Fatalf("%+v", p)
	}
}

// Snapshots are per user: one user's stars (and follow rights) never leak to another.
func TestStarSnapshotsAreIsolated(t *testing.T) {
	e := setup(t, time.Nanosecond)
	pub := e.gh.AddRepo("carol", "lib", false)
	e.gh.Star("alice", pub)
	alice, bob := e.signIn("alice"), e.signIn("bob")
	alice.starred("?refresh=1")
	if p := bob.starred("?refresh=1"); len(p.Repositories) != 0 || p.RefreshFailed {
		t.Fatalf("bob sees %v", starredNames(p))
	}
	if got := bob.write("PUT", fmt.Sprintf("/api/repositories/%d/follow", pub)).StatusCode; got != 404 {
		t.Fatalf("bob follows alice's star: %d", got)
	}
}

// Two refreshes at once (a double page open) both succeed and leave one consistent snapshot.
func TestConcurrentStarRefreshes(t *testing.T) {
	e := setup(t, time.Nanosecond)
	for i := 0; i < 5; i++ {
		e.gh.Star("alice", e.gh.AddRepo("bob", fmt.Sprintf("r%d", i), false))
	}
	alice := e.signIn("alice")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p := alice.starred("?refresh=1"); p.RefreshFailed || len(p.Repositories) != 5 {
				t.Errorf("concurrent refresh: %+v", p)
			}
		}()
	}
	wg.Wait()
}
