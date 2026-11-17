//go:build integration

package ingestion

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity_resources WHERE phase='catchup' AND generation=1`).Scan(&reopened); err != nil || reopened != 4 {
		t.Fatalf("reopened %d (%v), want 4 streams once", reopened, err)
	}
	if n := count(t, pool, "jobs WHERE kind='repo_metadata_refresh'"); n != 0 {
		t.Fatalf("%d metadata refreshes for an activity-only event", n)
	}
	if w := post(h, testSecret, "release", "d-2", `{"action":"published","repository":{"id":99},"release":{"id":1}}`, ""); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
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
