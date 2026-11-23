//go:build integration

package ingestion

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func selectedRepo(t *testing.T, pool *pgxpool.Pool, repoID int64) {
	t.Helper()
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (github_id, login, access_token_enc, refresh_token_enc) VALUES ($1,'u','\x01','\x02') RETURNING id`, repoID+1000).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO repositories (id, name, description) VALUES (%d, 'o/r', '')`, repoID),
		fmt.Sprintf(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES (%d, %d, 'owned', now())`, uid, repoID),
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// Signed comment and release deliveries reopen caught-up streams exactly once and, without a metadata refresh,
// leave unselected repositories untouched.
func TestActivityEventsSignalCaughtUpStreamsOnce(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	selectedRepo(t, pool, 42)
	tx, _ := pool.Begin(ctx)
	if err := RequestActivity(ctx, tx, 42, time.Now()); err != nil {
		t.Fatal(err)
	}
	tx.Commit(ctx)
	if _, err := pool.Exec(ctx, `UPDATE activity_resources SET phase='current'; UPDATE jobs SET state='queued'`); err != nil {
		t.Fatal(err)
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	body := `{"action":"edited","repository":{"id":42},"comment":{"id":9}}`
	for i := 0; i < 2; i++ {
		if w := post(h, testSecret, "issue_comment", "d-1", body, ""); w.Code != 200 {
			t.Fatalf("status %d", w.Code)
		}
	}
	var reopened int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity_resources WHERE phase='catchup' AND generation=1`).Scan(&reopened); err != nil || reopened != 5 {
		t.Fatalf("reopened %d (%v), want 5 streams once", reopened, err)
	}
	if n := count(t, pool, "jobs WHERE kind='repo_metadata_refresh'"); n != 0 {
		t.Fatalf("%d metadata refreshes for an activity-only event", n)
	}
	// A repeated delivery id must not reopen anything even once the streams are caught up again.
	if _, err := pool.Exec(ctx, `UPDATE activity_resources SET phase='current'`); err != nil {
		t.Fatal(err)
	}
	if w := post(h, testSecret, "issue_comment", "d-1", body, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if n := count(t, pool, "activity_resources WHERE phase='current' AND generation=1"); n != 5 {
		t.Fatalf("repeated delivery reopened streams (%d still current, want 5)", n)
	}
	if w := post(h, testSecret, "release", "d-2", `{"action":"published","repository":{"id":99},"release":{"id":1}}`, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if n := count(t, pool, "activity_resources WHERE repository_id=99"); n != 0 {
		t.Fatalf("unselected repository scheduled %d streams", n)
	}
}

// A signed deletion removes the stored copy and a stale page can never bring it back; an unselected repository stores nothing.
func TestSignedDeletionTombstonesAndBlocksResurrection(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	selectedRepo(t, pool, 42)
	if _, err := pool.Exec(ctx, `INSERT INTO activity_releases(source_id,repository_id,name,tag_name,body,html_url,created_at,published_at,observed_at,prerelease) VALUES (7,42,'n','v1','','u',now(),now(),now(),false)`); err != nil {
		t.Fatal(err)
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	if w := post(h, testSecret, "release", "del-1", `{"action":"deleted","repository":{"id":42},"release":{"id":7}}`, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if n := count(t, pool, "activity_releases"); n != 0 {
		t.Fatalf("%d releases remain", n)
	}
	// A stale page replays the deleted release.
	if _, err := pool.Exec(ctx, `INSERT INTO activity_releases(source_id,repository_id,name,tag_name,body,html_url,created_at,published_at,observed_at,prerelease) VALUES (7,42,'n','v1','','u',now(),now(),now(),false)`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "activity_releases"); n != 0 {
		t.Fatal("tombstoned release was resurrected")
	}
	if w := post(h, testSecret, "release", "del-2", `{"action":"deleted","repository":{"id":50},"release":{"id":8}}`, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if n := count(t, pool, "activity_tombstones"); n != 1 {
		t.Fatalf("%d tombstones, want only the selected repository's", n)
	}
}

// Deselecting leaves the stored evidence behind, so a signed deletion must still remove it and tombstone the id;
// otherwise reselecting would serve a copy GitHub no longer has.
func TestSignedDeletionAppliesToDeselectedRepositoryWithStoredEvidence(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	selectedRepo(t, pool, 42)
	if _, err := pool.Exec(ctx, `INSERT INTO activity_releases(source_id,repository_id,name,tag_name,body,html_url,created_at,published_at,observed_at,prerelease) VALUES (7,42,'n','v1','','u',now(),now(),now(),false);
UPDATE selections SET deselected_at=now() WHERE repository_id=42`); err != nil {
		t.Fatal(err)
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	if w := post(h, testSecret, "release", "del-ds", `{"action":"deleted","repository":{"id":42},"release":{"id":7}}`, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if n := count(t, pool, "activity_releases"); n != 0 {
		t.Fatalf("%d releases remain for a deselected repository", n)
	}
	if n := count(t, pool, "activity_tombstones WHERE repository_id=42"); n != 1 {
		t.Fatalf("%d tombstones, want 1", n)
	}
	if n := count(t, pool, "jobs WHERE key='repo:42' AND kind LIKE 'activity_%'"); n != 0 {
		t.Fatalf("deselected repository scheduled %d activity jobs", n)
	}
}

// Deleting an issue removes its comments, events and child-history work, and the issue cannot be re-imported.
func TestSignedIssueDeletionRemovesDependents(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	selectedRepo(t, pool, 42)
	for _, q := range []string{
		`INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES (5,42,3,'t','open','u',now(),now(),1,'a','User')`,
		`INSERT INTO activity_comments(source_id,repository_id,issue_number,body,html_url,created_at,updated_at) VALUES (6,42,3,'b','u',now(),now())`,
		`INSERT INTO activity_issue_events(source_id,repository_id,issue_id,issue_number,event,url,created_at,observed_at) VALUES (7,42,5,3,'closed','u',now(),now())`,
		`INSERT INTO activity_children(repository_id,resource,parent_id,parent_number) VALUES (42,'comments',3,3)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	if w := post(h, testSecret, "issues", "del-i", `{"action":"deleted","repository":{"id":42},"issue":{"id":5,"number":3}}`, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, table := range []string{"activity_issues", "activity_comments", "activity_issue_events", "activity_children"} {
		if n := count(t, pool, table); n != 0 {
			t.Errorf("%s has %d rows", table, n)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES (5,42,3,'t','open','u',now(),now(),1,'a','User')`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "activity_issues"); n != 0 {
		t.Fatal("deleted issue was resurrected")
	}
}

// waitForLockWait blocks until some other backend is waiting on a lock, or the timeout passes.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A deletion that arrives while a sync commit is inserting the same release (checkpoint row locked, insert not yet
// visible) must wait for that commit and then remove the row: otherwise the row survives next to its tombstone.
func TestSignedDeletionSerializesWithInFlightSyncCommit(t *testing.T) {
	cases := []struct {
		name, resource, event, payload, insert string
	}{
		{"release", "releases", "release", `{"action":"deleted","repository":{"id":42},"release":{"id":7}}`,
			`INSERT INTO activity_releases(source_id,repository_id,name,tag_name,body,html_url,created_at,published_at,observed_at,prerelease) VALUES (7,42,'n','v1','','u',now(),now(),now(),false)`},
		{"issue event", "issue_events", "issues", `{"action":"deleted","repository":{"id":42},"issue":{"id":5,"number":3}}`,
			`INSERT INTO activity_issue_events(source_id,repository_id,issue_id,issue_number,event,url,created_at,observed_at) VALUES (9,42,5,3,'closed','u',now(),now())`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, pool := testutil.Database(t)
			ctx := context.Background()
			selectedRepo(t, pool, 42)
			tx, _ := pool.Begin(ctx)
			if err := RequestActivity(ctx, tx, 42, time.Now()); err != nil {
				t.Fatal(err)
			}
			tx.Commit(ctx)
			// The sync side: checkpoint row locked FOR UPDATE, tombstone check (none yet), insert, commit pending.
			sync, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer sync.Rollback(ctx)
			if _, err = sync.Exec(ctx, `SELECT 1 FROM activity_resources WHERE repository_id=42 AND resource=$1 FOR UPDATE`, tc.resource); err != nil {
				t.Fatal(err)
			}
			if _, err = sync.Exec(ctx, tc.insert); err != nil {
				t.Fatal(err)
			}
			h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
			done := make(chan int, 1)
			go func() { done <- post(h, testSecret, tc.event, "del-race", tc.payload, "").Code }()
			if !waitForLockWait(t, pool, 3*time.Second) {
				t.Log("deletion did not wait for the in-flight sync commit")
			}
			if err = sync.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if code := <-done; code != 200 {
				t.Fatalf("status %d", code)
			}
			if n := count(t, pool, "activity_tombstones WHERE repository_id=42"); n != 1 {
				t.Fatalf("%d tombstones, want 1", n)
			}
			for _, table := range []string{"activity_releases", "activity_issue_events"} {
				if n := count(t, pool, table); n != 0 {
					t.Fatalf("%s has %d rows next to the tombstone", table, n)
				}
			}
		})
	}
}

// An issue-write readback projects GitHub's confirmed copy under the per-issue lock (LockIssue), with no checkpoint
// lock. A signed deletion must serialize with it too, or each misses the other's uncommitted write and the deleted
// issue is stored again next to its tombstone.
func TestSignedDeletionSerializesWithInFlightConfirmedProjection(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	selectedRepo(t, pool, 42)
	write, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Rollback(ctx)
	now := time.Now()
	if err = LockIssue(ctx, write, 42, 3); err != nil {
		t.Fatal(err)
	}
	if err = ProjectConfirmedIssue(ctx, write, 42, githubapp.Issue{ID: 5, Number: 3, Title: "t", State: "open", HTMLURL: "u", CreatedAt: now, UpdatedAt: now,
		User: &githubapp.IssueUser{ID: 1, Login: "a", Type: "User"}}); err != nil {
		t.Fatal(err)
	}
	h := WebhookHandler(WebhookDeps{Pool: pool, Secret: testSecret})
	done := make(chan int, 1)
	go func() {
		done <- post(h, testSecret, "issues", "del-write", `{"action":"deleted","repository":{"id":42},"issue":{"id":5,"number":3}}`, "").Code
	}()
	if !waitForLockWait(t, pool, 3*time.Second) {
		t.Log("deletion did not wait for the in-flight readback projection")
	}
	if err = write.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 200 {
		t.Fatalf("status %d", code)
	}
	if n := count(t, pool, "activity_issues"); n != 0 {
		t.Fatalf("%d issues stored next to the tombstone", n)
	}
}
