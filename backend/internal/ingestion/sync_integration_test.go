//go:build integration

package ingestion_test

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

type issueFixture struct {
	pages    map[int][]githubapp.Issue
	accesses int
	flip     bool
	onFetch  func()
}

func (f *issueFixture) FetchRepo(context.Context, string, int64) (githubapp.Repo, error) {
	return githubapp.Repo{ID: 70, FullName: "a/b"}, nil
}
func (f *issueFixture) FetchIssuesCursor(_ context.Context, _ string, _ string, cursor string) ([]githubapp.Issue, string, error) {
	if f.onFetch != nil {
		f.onFetch()
	}
	if cursor == "" {
		return f.pages[1], "page=2", nil
	}
	return f.pages[2], "", nil
}

func (f *issueFixture) CheckOne(context.Context, int64, int64, bool) repositories.Decision {
	f.accesses++
	return repositories.Decision{Allowed: true, Repo: githubapp.Repo{ID: 70, FullName: "a/b", Private: f.flip && f.accesses >= 2}}
}

func TestIssueBackfillAndVisibilityFence(t *testing.T) {
	for _, flip := range []bool{false, true} {
		t.Run(fmt.Sprint(flip), func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			uid := e.signIn("alice")
			e.store(uid, 70, false)
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = ingestion.RequestActivity(ctx, tx, 70, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			old := now.Add(-365 * 24 * time.Hour)
			pr := json.RawMessage(`{}`)
			fixture := &issueFixture{flip: flip, pages: map[int][]githubapp.Issue{1: {{ID: 11, Number: 1, Title: "new", State: "open", CreatedAt: now, UpdatedAt: now}, {ID: 99, PullRequest: &pr}}, 2: {{ID: 12, Number: 2, Title: "old open", State: "open", CreatedAt: old, UpdatedAt: old}}}}
			e.deps.GitHub = fixture
			e.deps.Access = fixture
			for i := 0; i < 7; i++ {
				err = e.deps.SyncIssues(ctx, jobs.Job{Key: "repo:70"}, txFence{pool: e.pool})
				if flip {
					break
				}
				if err != nil {
					if _, ok := err.(*jobs.Defer); !ok {
						t.Fatal(err)
					}
				}
			}
			var count int
			var phase string
			var private bool
			if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues WHERE repository_id=70), phase, is_private FROM activity_resources JOIN repositories ON id=repository_id WHERE repository_id=70 AND resource='issues'`).Scan(&count, &phase, &private); err != nil {
				t.Fatal(err)
			}
			if flip {
				if count != 0 || !private || phase != "backfill" {
					t.Fatalf("visibility transition committed page: count=%d private=%v phase=%s", count, private, phase)
				}
			} else if count != 2 || phase != "current" {
				t.Fatalf("count=%d phase=%s", count, phase)
			}
		})
	}
}

type lostFence struct{}

func (lostFence) Commit(context.Context, func(pgx.Tx) error) error { return jobs.ErrLostClaim }

func TestIssueResumeReconcilesChangedPagesAndRejectsLostClaim(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	e.store(uid, 70, false)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ingestion.RequestActivity(ctx, tx, 70, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fixture := &issueFixture{pages: map[int][]githubapp.Issue{1: {{ID: 11, Number: 1, Title: "original", State: "open", CreatedAt: now, UpdatedAt: now}}, 2: {{ID: 12, Number: 2, Title: "second", State: "open", CreatedAt: now, UpdatedAt: now}}}}
	e.deps.GitHub = fixture
	e.deps.Access = fixture
	job := jobs.Job{Key: "repo:70"}
	if err = e.deps.SyncIssues(ctx, job, lostFence{}); !errors.Is(err, jobs.ErrLostClaim) {
		t.Fatalf("claim loss: %v", err)
	}
	var count int
	var page int
	if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues),page FROM activity_resources WHERE repository_id=70 AND resource='issues'`).Scan(&count, &page); err != nil || count != 0 || page != 1 {
		t.Fatalf("lost claim wrote: %d %d %v", count, page, err)
	}
	for i := 0; i < 4; i++ {
		if err = e.deps.SyncIssues(ctx, job, txFence{pool: e.pool}); err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
	}
	fixture.pages[1] = []githubapp.Issue{{ID: 13, Number: 3, Title: "inserted", State: "open", CreatedAt: now, UpdatedAt: now}, {ID: 11, Number: 1, Title: "updated", State: "open", CreatedAt: now, UpdatedAt: now.Add(time.Minute)}}
	for i := 0; i < 2; i++ {
		_ = e.deps.SyncIssues(ctx, job, txFence{pool: e.pool})
	}
	var phase string
	if err = e.pool.QueryRow(ctx, `SELECT phase FROM activity_resources WHERE repository_id=70 AND resource='issues'`).Scan(&phase); err != nil || phase != "catchup" {
		t.Fatalf("shift reported complete: %s %v", phase, err)
	}
	for i := 0; i < 2; i++ {
		_ = e.deps.SyncIssues(ctx, job, txFence{pool: e.pool})
	}
	var title string
	if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues),phase,(SELECT title FROM activity_issues WHERE source_id=11) FROM activity_resources WHERE repository_id=70 AND resource='issues'`).Scan(&count, &phase, &title); err != nil || count != 3 || phase != "current" || title != "updated" {
		t.Fatalf("count=%d phase=%s title=%s err=%v", count, phase, title, err)
	}
}

func TestPurgeRebuildPreservesBoundaryAndRejectsOldPage(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	e.store(uid, 70, true)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ingestion.RequestActivity(ctx, tx, 70, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fixture := &issueFixture{pages: map[int][]githubapp.Issue{1: {{ID: 11, Number: 1, Title: "unchanged", State: "open", CreatedAt: now, UpdatedAt: now}}, 2: {}}}
	e.deps.GitHub = fixture
	e.deps.Access = fixture
	job := jobs.Job{Key: "repo:70"}
	for i := 0; i < 6; i++ {
		_ = e.deps.SyncIssues(ctx, job, txFence{pool: e.pool})
	}
	var boundary time.Time
	if err = e.pool.QueryRow(ctx, `SELECT boundary FROM activity_resources WHERE repository_id=70 AND resource='issues'`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	if _, err = e.pool.Exec(ctx, `INSERT INTO activity_cleanup(repository_id) VALUES(70)`); err != nil {
		t.Fatal(err)
	}
	e.deps.Inventory = &inventories{results: []map[int64][]int64{{}, {}}}
	e.deps.AppKey = &rsa.PrivateKey{}
	if err = e.deps.CleanupPrivate(ctx, job, txFence{pool: e.pool}); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	var phase string
	var generation int64
	if err = e.pool.QueryRow(ctx, `SELECT boundary,phase,generation FROM activity_resources WHERE repository_id=70 AND resource='issues'`).Scan(&after, &phase, &generation); err != nil || !after.Equal(boundary) || phase != "backfill" || generation != 1 {
		t.Fatalf("reset %v %s %d %v", after, phase, generation, err)
	}
	// A page fetched before the generation changed cannot commit after that change.
	var once sync.Once
	fence := txFence{pool: e.pool, before: func() {
		once.Do(func() {
			if _, err := e.pool.Exec(ctx, `UPDATE activity_resources SET generation=generation+1 WHERE repository_id=70`); err != nil {
				t.Fatal(err)
			}
		})
	}}
	_ = e.deps.SyncIssues(ctx, job, fence)
	var count int
	if err = e.pool.QueryRow(ctx, `SELECT count(*) FROM activity_issues WHERE repository_id=70`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old generation wrote %d: %v", count, err)
	}
	_ = e.deps.SyncIssues(ctx, job, txFence{pool: e.pool})
	if err = e.pool.QueryRow(ctx, `SELECT phase FROM activity_resources WHERE repository_id=70 AND resource='issues'`).Scan(&phase); err != nil || phase != "backfill" {
		t.Fatalf("interrupted rebuild reported complete: %s %v", phase, err)
	}
	for i := 0; i < 5; i++ {
		_ = e.deps.SyncIssues(ctx, job, txFence{pool: e.pool})
	}
	if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues WHERE repository_id=70),phase FROM activity_resources WHERE repository_id=70 AND resource='issues'`).Scan(&count, &phase); err != nil || count != 1 || phase != "current" {
		t.Fatalf("rebuild: %d %s %v", count, phase, err)
	}
}

func TestIssueSyncWithFakeGitHubAndNoInstallation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	repoID := e.gh.AddRepo("other", "public", false)
	e.store(uid, repoID, false)
	now := time.Now().UTC()
	items := make([]fake.IssueFixture, 101)
	for i := range items {
		items[i] = fake.IssueFixture{ID: int64(10000 + i), Number: int64(i + 1), Title: fmt.Sprintf("issue %d", i), State: "open", CreatedAt: now, UpdatedAt: now, User: fake.IssueActor{ID: 9, Login: "bot", Type: "Bot"}}
	}
	e.gh.SetIssues(repoID, items...)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ingestion.RequestActivity(ctx, tx, repoID, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		err = e.deps.SyncIssues(ctx, jobs.Job{Key: fmt.Sprintf("repo:%d", repoID)}, txFence{pool: e.pool})
		if err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
	}
	var count int
	var phase string
	if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues WHERE repository_id=$1),phase FROM activity_resources WHERE repository_id=$1 AND resource='issues'`, repoID).Scan(&count, &phase); err != nil || count != 101 || phase != "current" {
		t.Fatalf("count=%d phase=%s err=%v", count, phase, err)
	}
}

// Removing the eligibility lock or access snapshot check would let a fetched page advance.
func TestIssueFetchFencedByDeselectionAndRevocation(t *testing.T) {
	for _, scenario := range []string{"deselection", "revocation"} {
		t.Run(scenario, func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			uid := e.signIn("alice")
			e.store(uid, 70, true)
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = ingestion.RequestActivity(ctx, tx, 70, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			fixture := &issueFixture{pages: map[int][]githubapp.Issue{1: {{ID: 11, Number: 1, Title: "fetched", State: "open", CreatedAt: now, UpdatedAt: now}}}}
			fixture.onFetch = func() {
				if scenario == "deselection" {
					_, err = e.pool.Exec(ctx, `UPDATE selections SET deselected_at=now() WHERE user_id=$1 AND repository_id=70`, uid)
				} else {
					var tx pgx.Tx
					tx, err = e.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(ctx)
					err = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeUser, uid)
					if err == nil {
						err = tx.Commit(ctx)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			e.deps.GitHub, e.deps.Access = fixture, fixture
			err = e.deps.SyncIssues(ctx, jobs.Job{Key: "repo:70"}, txFence{pool: e.pool})
			var deferred *jobs.Defer
			if !errors.As(err, &deferred) {
				t.Fatalf("expected defer, got %v", err)
			}
			var count, page int
			var phase, cursor string
			err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues WHERE repository_id=70), page, phase, cursor FROM activity_resources WHERE repository_id=70 AND resource='issues'`).Scan(&count, &page, &phase, &cursor)
			if err != nil || count != 0 || page != 1 || phase != "backfill" || cursor != "" {
				t.Fatalf("page or checkpoint committed: count=%d page=%d phase=%s cursor=%q err=%v", count, page, phase, cursor, err)
			}
		})
	}
}

// GitHub's updated_at has one-second resolution. A confirmed write read back within the same second as the stored
// copy still lands; a sync fetch with the same timestamp never replaces what is stored.
func TestConfirmedProjectionAcceptsSameSecondUpdate(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO repositories(id,name,description,is_private) VALUES(70,'a/b','',false)`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	issue := githubapp.Issue{ID: 77, Number: 1, Title: "t", State: "open", CreatedAt: at, UpdatedAt: at, User: &githubapp.IssueUser{ID: 1, Login: "u", Type: "User"}}
	state := func() string {
		var s string
		if err := e.pool.QueryRow(ctx, `SELECT state FROM activity_issues WHERE source_id = 77`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	project := func(f func(context.Context, pgx.Tx, int64, githubapp.Issue) error, i githubapp.Issue) {
		if err := db.InTx(ctx, e.pool, func(tx pgx.Tx) error { return f(ctx, tx, 70, i) }); err != nil {
			t.Fatal(err)
		}
	}
	project(ingestion.ProjectIssue, issue)
	closed := issue
	closed.State = "closed"
	project(ingestion.ProjectConfirmedIssue, closed)
	if got := state(); got != "closed" {
		t.Fatalf("confirmed same-second update lost: %s", got)
	}
	project(ingestion.ProjectIssue, issue) // an older sync fetch with the same timestamp
	if got := state(); got != "closed" {
		t.Fatalf("same-second sync fetch overwrote the confirmed copy: %s", got)
	}
}
