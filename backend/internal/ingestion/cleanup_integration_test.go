//go:build integration

package ingestion_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

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
