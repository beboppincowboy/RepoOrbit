//go:build integration

package ingestion_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// access answers CheckOne from a fixed table: a listed user is allowed (or unverified), anyone else denied.
type access struct {
	allowed    map[int64]bool
	unverified bool
}

func (a access) CheckOne(_ context.Context, userID, _ int64, _ bool) repositories.Decision {
	return repositories.Decision{Allowed: !a.unverified && a.allowed[userID], Unverified: a.unverified}
}

type inventories struct {
	calls   int
	results []map[int64][]int64
	err     error
}

func (i *inventories) AppInventory(context.Context, *rsa.PrivateKey) (map[int64][]int64, error) {
	if i.err != nil {
		return nil, i.err
	}
	n := i.calls
	i.calls++
	if n >= len(i.results) {
		n = len(i.results) - 1
	}
	return i.results[n], nil
}

func TestCleanupInventoryFencesAndPreservesBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		inv                        []map[int64][]int64
		fail, bump, restore, purge bool
	}{
		{name: "confirmed absence", inv: []map[int64][]int64{{}, {}}, purge: true},
		{name: "other installation retains", inv: []map[int64][]int64{{3: {70}}, {3: {70}}}},
		{name: "unstable inventory", inv: []map[int64][]int64{{}, {3: {70}}}},
		{name: "inventory error", fail: true},
		{name: "concurrent invalidation", inv: []map[int64][]int64{{}, {}}, bump: true},
		{name: "concurrent restoration", inv: []map[int64][]int64{{}, {}}, restore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			repoID := int64(70)
			_, err := e.pool.Exec(ctx, `INSERT INTO repositories(id,name,description,is_private) VALUES(70,'a/b','',true); INSERT INTO activity_resources(repository_id,resource,boundary,phase,generation) VALUES(70,'issues','2025-01-01','current',4); INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES(77,70,1,'issue','open','',now(),now(),1,'u','User'); INSERT INTO activity_pull_requests(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES(78,70,2,'pr','open','',now(),now(),1,'u','User'); INSERT INTO activity_reviews(source_id,repository_id,pull_request_id,state,body,html_url,submitted_at,author_id,author_login,author_type) VALUES(79,70,78,'APPROVED','','',now(),1,'u','User'); INSERT INTO activity_comments(source_id,repository_id,issue_number,body,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES(80,70,1,'','',now(),now(),1,'u','User'); INSERT INTO activity_children(repository_id,resource,parent_id,parent_number) VALUES(70,'reviews',78,2); INSERT INTO activity_cleanup(repository_id) VALUES(70)`)
			if err != nil {
				t.Fatal(err)
			}
			inv := &inventories{results: tc.inv}
			if tc.fail {
				inv.err = errors.New("unavailable")
			}
			e.deps.Inventory = inv
			e.deps.AppKey = &rsa.PrivateKey{}
			f := txFence{pool: e.pool}
			if tc.bump || tc.restore {
				f.before = func() {
					tx, err := e.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(ctx)
					if tc.restore {
						err = accessfence.BumpAccess(ctx, tx, accessfence.ScopeRepository, repoID)
					} else {
						err = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeRepository, repoID)
					}
					if err != nil {
						t.Fatal(err)
					}
					if err = tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = e.deps.CleanupPrivate(ctx, jobs.Job{Key: fmt.Sprintf("repo:%d", repoID)}, f)
			if (tc.fail || tc.bump || tc.restore || tc.name == "unstable inventory") && err == nil {
				t.Fatal("uncertainty must defer")
			}
			var count, evidence int
			if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_pull_requests WHERE repository_id=70) + (SELECT count(*) FROM activity_reviews WHERE repository_id=70) + (SELECT count(*) FROM activity_comments WHERE repository_id=70) + (SELECT count(*) FROM activity_children WHERE repository_id=70)`).Scan(&evidence); err != nil {
				t.Fatal(err)
			}
			if (tc.purge && evidence != 0) || (!tc.purge && evidence != 4) {
				t.Fatalf("evidence count %d", evidence)
			}
			var phase string
			var generation int64
			var boundary time.Time
			if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues WHERE repository_id=70),phase,generation,boundary FROM activity_resources WHERE repository_id=70`).Scan(&count, &phase, &generation, &boundary); err != nil {
				t.Fatal(err)
			}
			if boundary.UTC().Format("2006-01-02") != "2025-01-01" {
				t.Fatalf("boundary moved: %v", boundary)
			}
			if tc.purge {
				if count != 0 || phase != "backfill" || generation != 5 {
					t.Fatalf("purge state %d %s %d", count, phase, generation)
				}
			} else if count != 1 || phase != "current" || generation != 4 {
				t.Fatalf("retained state %d %s %d", count, phase, generation)
			}
		})
	}
}

// A review tagged with the purged repository must go even when its parent PR belongs to another
// repository: the PR cascade alone would leave it behind, and the other repository's PR must survive.
func TestCleanupPurgesReviewsTaggedWithRepositoryWhosePullRequestIsElsewhere(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO repositories(id,name,description,is_private) VALUES(70,'a/b','',true),(71,'a/c','',false);
INSERT INTO activity_pull_requests(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES(78,71,2,'pr','open','',now(),now(),1,'u','User');
INSERT INTO activity_reviews(source_id,repository_id,pull_request_id,state,body,html_url,submitted_at,author_id,author_login,author_type) VALUES(79,70,78,'APPROVED','','',now(),1,'u','User');
INSERT INTO activity_cleanup(repository_id) VALUES(70)`); err != nil {
		t.Fatal(err)
	}
	e.deps.Inventory = &inventories{results: []map[int64][]int64{{}, {}}}
	e.deps.AppKey = &rsa.PrivateKey{}
	if err := e.deps.CleanupPrivate(ctx, jobs.Job{Key: "repo:70"}, txFence{pool: e.pool}); err != nil {
		t.Fatal(err)
	}
	var reviews, prs int
	if err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_reviews WHERE repository_id=70),(SELECT count(*) FROM activity_pull_requests WHERE repository_id=71)`).Scan(&reviews, &prs); err != nil {
		t.Fatal(err)
	}
	if reviews != 0 || prs != 1 {
		t.Fatalf("reviews tagged 70 = %d (want 0), repository 71 PRs = %d (want 1)", reviews, prs)
	}
}

// Cleanup wipes and redacts issue-write drafts of exactly the actors whose installations no longer cover the private
// repository: everyone when no installation does, only the uncovered when one still does. The audit row stays.
func TestCleanupWipesWriteDrafts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inv      map[int64][]int64
		access   access
		kept     []int64 // users whose drafts survive
		deferred bool
	}{
		{name: "no installation covers", inv: map[int64][]int64{}, access: access{allowed: map[int64]bool{5: true, 6: true}}, kept: nil},
		{name: "one installation still covers", inv: map[int64][]int64{3: {70}, 4: {}}, access: access{allowed: map[int64]bool{5: true}}, kept: []int64{5}},
		// GitHub grants an installation member only the covered repositories they can see themselves.
		{name: "covered but no longer a collaborator", inv: map[int64][]int64{3: {70}, 4: {}}, access: access{}, kept: nil},
		{name: "covered actor unverifiable", inv: map[int64][]int64{3: {70}, 4: {}}, access: access{unverified: true}, kept: []int64{5, 6}, deferred: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			if _, err := e.pool.Exec(ctx, `INSERT INTO repositories(id,name,description,is_private) VALUES(70,'a/b','',true);
INSERT INTO users(id,github_id,login,access_token_enc) VALUES(5,5,'u','\x00'),(6,6,'v','\x00');
INSERT INTO installations(user_id,installation_id,account_login) VALUES(5,3,'o'),(6,4,'o');
INSERT INTO write_operations(user_id,repository_id,idempotency_key,kind,request_hash,draft,state,outcome) VALUES
 (5,70,'k','create','\x00','{"title":"t"}','failed','{"issue":{"number":1,"title":"secret","labels":["x"]}}'),
 (6,70,'k','create','\x00','{"title":"t"}','failed','{"issue":{"number":2,"title":"secret","labels":["x"]}}');
INSERT INTO activity_cleanup(repository_id) VALUES(70)`); err != nil {
				t.Fatal(err)
			}
			e.deps.Inventory = &inventories{results: []map[int64][]int64{tc.inv, tc.inv}}
			e.deps.AppKey = &rsa.PrivateKey{}
			e.deps.Access = tc.access
			err := e.deps.CleanupPrivate(ctx, jobs.Job{Key: "repo:70"}, txFence{pool: e.pool})
			var deferral *jobs.Defer
			if tc.deferred != errors.As(err, &deferral) || err != nil && deferral == nil {
				t.Fatalf("cleanup: %v (deferred: want %v)", err, tc.deferred)
			}
			rows, err := e.pool.Query(ctx, `SELECT user_id FROM write_operations WHERE draft IS NOT NULL AND outcome::text LIKE '%secret%' ORDER BY user_id`)
			if err != nil {
				t.Fatal(err)
			}
			kept, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				t.Fatal(err)
			}
			var total, redacted int
			if err := e.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE draft IS NULL AND outcome::text NOT LIKE '%secret%' AND outcome->'issue'->>'number' IS NOT NULL) FROM write_operations`).Scan(&total, &redacted); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(kept) != fmt.Sprint(tc.kept) || total != 2 || redacted != 2-len(tc.kept) {
				t.Fatalf("kept=%v total=%d redacted=%d", kept, total, redacted)
			}
		})
	}
}

// While no complete inventory can be had (here: an installation is suspended, which fails the whole inventory), an
// installation revocation still removes its holders' drafts on the private repository, failing closed; other actors'
// drafts and the shared projection wait for a complete inventory.
func TestCleanupWipesRevokedInstallationDraftsWithoutInventory(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO repositories(id,name,description,is_private) VALUES(70,'a/b','',true);
INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES(77,70,1,'issue','open','',now(),now(),1,'u','User');
INSERT INTO users(id,github_id,login,access_token_enc) VALUES(5,5,'u','\x00'),(6,6,'v','\x00');
INSERT INTO installations(user_id,installation_id,account_login) VALUES(5,3,'o'),(6,4,'o');
INSERT INTO write_operations(user_id,repository_id,idempotency_key,kind,request_hash,draft,state,outcome) VALUES
 (5,70,'k','create','\x00','{"title":"t"}','failed','{"issue":{"number":1,"title":"secret"}}'),
 (6,70,'k','create','\x00','{"title":"t"}','failed','{"issue":{"number":2,"title":"secret"}}')`); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeInstallation, 3); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	e.deps.Inventory = &inventories{err: errors.New("github app: invalid or suspended installation")}
	e.deps.AppKey = &rsa.PrivateKey{}
	if err := e.deps.CleanupPrivate(ctx, jobs.Job{Key: "repo:70"}, txFence{pool: e.pool}); err == nil {
		t.Fatal("an unavailable inventory must defer the purge")
	}
	rows, err := e.pool.Query(ctx, `SELECT user_id FROM write_operations WHERE draft IS NOT NULL OR outcome::text LIKE '%secret%' ORDER BY user_id`)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	var issues, total int
	if err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues WHERE repository_id=70), (SELECT count(*) FROM write_operations)`).Scan(&issues, &total); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(kept) != "[6]" || total != 2 || issues != 1 {
		t.Fatalf("kept drafts of %v, %d operations, %d projected issues", kept, total, issues)
	}
}

// A security event must leave a job that a new worker can process without a selector.
func TestCleanupInvalidationSurvivesWorkerRestart(t *testing.T) {
	for _, scope := range []accessfence.Scope{accessfence.ScopeUser, accessfence.ScopeInstallation} {
		t.Run(string(scope), func(t *testing.T) {
			e := setup(t)
			ctx := context.Background()
			_, err := e.pool.Exec(ctx, `INSERT INTO repositories(id,name,description,is_private) VALUES(70,'a/b','',true); INSERT INTO activity_resources(repository_id,resource,boundary,phase) VALUES(70,'issues','2025-01-01','current'); INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES(77,70,1,'issue','open','',now(),now(),1,'u','User')`)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = accessfence.InvalidateAccess(ctx, tx, scope, 123); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			// Only persisted database state crosses this worker boundary.
			e.deps.Inventory = &inventories{results: []map[int64][]int64{{}, {}}}
			e.deps.AppKey = &rsa.PrivateKey{}
			worker := &jobs.Worker{Pool: e.pool, Poll: 10 * time.Millisecond}
			done := make(chan error, 1)
			worker.Register(jobs.KindActivityCleanup, func(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
				err := e.deps.CleanupPrivate(ctx, job, fence)
				done <- err
				return err
			})
			runCtx, cancel := context.WithCancel(ctx)
			exited := make(chan error, 1)
			go func() { exited <- worker.Run(runCtx) }()
			select {
			case err = <-done:
				if err != nil {
					t.Errorf("cleanup: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("worker did not resume cleanup")
			}
			cancel()
			<-exited
			var issues, pending int
			err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues WHERE repository_id=70), (SELECT count(*) FROM activity_cleanup WHERE repository_id=70)`).Scan(&issues, &pending)
			if err != nil || issues != 0 || pending != 0 {
				t.Fatalf("issues=%d pending=%d err=%v", issues, pending, err)
			}
		})
	}
}

// The fallback draft wipe is a cleanup effect like any other: a worker that lost its claim wipes nothing and leaves
// wipe_users for the worker that reclaimed the job.
func TestCleanupFallbackWipeNeedsTheClaim(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO repositories(id,name,description,is_private) VALUES(70,'a/b','',true);
INSERT INTO users(id,github_id,login,access_token_enc) VALUES(5,5,'u','\x00');
INSERT INTO installations(user_id,installation_id,account_login) VALUES(5,3,'o');
INSERT INTO write_operations(user_id,repository_id,idempotency_key,kind,request_hash,draft,state) VALUES (5,70,'k','create','\x00','{"title":"t"}','failed')`); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeInstallation, 3); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	e.deps.Inventory = &inventories{err: errors.New("github app: invalid or suspended installation")}
	e.deps.AppKey = &rsa.PrivateKey{}
	if err := e.deps.CleanupPrivate(ctx, jobs.Job{Key: "repo:70"}, lostFence{}); !errors.Is(err, jobs.ErrLostClaim) {
		t.Fatalf("lost claim: %v", err)
	}
	var drafts, targets int
	if err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM write_operations WHERE draft IS NOT NULL), (SELECT cardinality(wipe_users) FROM activity_cleanup WHERE repository_id=70)`).Scan(&drafts, &targets); err != nil {
		t.Fatal(err)
	}
	if drafts != 1 || targets != 1 {
		t.Fatalf("after a lost claim: %d drafts, %d wipe targets", drafts, targets)
	}
}
