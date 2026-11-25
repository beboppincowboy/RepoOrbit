//go:build integration

package ingestion_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

// Rolling back 00012 must restore the 00011 "unknown author" sentinel (id 0, empty login, empty type) instead of failing on
// the NOT NULL constraint, and migrating forward again must turn it back into NULL.
func TestReviewEvidenceRollbackRestoresUnknownAuthorSentinel(t *testing.T) {
	ctx := context.Background()
	dbURL, pool := testutil.Database(t)
	if _, err := pool.Exec(ctx, `INSERT INTO repositories(id,name,description) VALUES(71,'a/c','x');
INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type)
VALUES(710,71,1,'ghost','open','u',now(),now(),NULL,NULL,NULL), (711,71,2,'real','open','u',now(),now(),5,'bob','User')`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	author := func(id int64) (a sql.NullInt64, l, k sql.NullString) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT author_id,author_login,author_type FROM activity_issues WHERE source_id=$1`, id).Scan(&a, &l, &k); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err = goose.DownToContext(ctx, db, "migrations", 11); err != nil {
		t.Fatal(err)
	}
	if a, l, k := author(710); a.Int64 != 0 || l.String != "" || k.String != "" || !a.Valid {
		t.Fatalf("ghost author after rollback = %v %v %v", a, l, k)
	}
	if a, l, k := author(711); a.Int64 != 5 || l.String != "bob" || k.String != "User" {
		t.Fatalf("real author changed by rollback: %v %v %v", a, l, k)
	}
	if err = goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatal(err)
	}
	if a, l, k := author(710); a.Valid || l.Valid || k.Valid {
		t.Fatalf("ghost author after re-migrate = %v %v %v", a, l, k)
	}
}

func TestActivityMigrationPreservesExistingDataOnRollback(t *testing.T) {
	ctx := context.Background()
	dbURL, pool := testutil.Database(t)
	if err := platform.SeedDemo(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories(id,name,description) VALUES(70,'a/b','legacy'); INSERT INTO issues(id,repository_id,title,state) VALUES(700,70,'existing','open')`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownContext(ctx, db, "migrations"); err != nil {
		t.Fatal(err)
	}
	assertData := func() {
		t.Helper()
		var demo, legacy int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM repositories WHERE is_demo), (SELECT count(*) FROM issues WHERE id=700 AND repository_id=70)`).Scan(&demo, &legacy); err != nil || demo == 0 || legacy != 1 {
			t.Fatalf("demo=%d legacy=%d err=%v", demo, legacy, err)
		}
	}
	assertData()
	if err = goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatal(err)
	}
	assertData()
}

// 00016 drops per-issue comment progress and queued per-issue comment jobs, keeps stored comments, and gives every
// repository with an issue stream a comment stream from the same boundary.
func TestRepoWideCommentMigration(t *testing.T) {
	ctx := context.Background()
	dbURL, pool := testutil.Database(t)
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = goose.DownToContext(ctx, db, "migrations", 15); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories(id,name,description) VALUES(72,'a/d',''),(73,'a/e','');
INSERT INTO users (github_id, login, access_token_enc, refresh_token_enc) VALUES (1072,'u','\x01','\x02');
INSERT INTO selections (user_id, repository_id, source, selected_at) SELECT id, 72, 'owned', now() FROM users WHERE login='u';
INSERT INTO activity_resources(repository_id,resource,boundary) VALUES(72,'issues','2026-08-01Z');
INSERT INTO activity_children(repository_id,resource,parent_id,parent_number) VALUES(72,'comments',4,4),(72,'reviews',100,5);
INSERT INTO activity_comments(source_id,repository_id,issue_number,body,html_url,created_at,updated_at) VALUES(9,72,4,'kept','u',now(),now());
INSERT INTO jobs(kind,key,state,run_at) VALUES('activity_comments','repo:72:parent:4','queued',now()),('activity_reviews','repo:72:parent:100','queued',now())`); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatal(err)
	}
	var children, commentJobs, reviewJobs, comments, streams int
	var boundary string
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM activity_children WHERE resource='comments'),
		(SELECT count(*) FROM jobs WHERE kind='activity_comments'),
		(SELECT count(*) FROM jobs WHERE kind='activity_reviews'),
		(SELECT count(*) FROM activity_comments),
		(SELECT count(*) FROM activity_resources WHERE resource='comments'),
		(SELECT to_char(boundary AT TIME ZONE 'UTC','YYYY-MM-DD') FROM activity_resources WHERE repository_id=72 AND resource='comments')`).Scan(&children, &commentJobs, &reviewJobs, &comments, &streams, &boundary); err != nil {
		t.Fatal(err)
	}
	if children != 0 || commentJobs != 0 || reviewJobs != 1 || comments != 1 || streams != 1 || boundary != "2026-08-01" {
		t.Fatalf("children=%d commentJobs=%d reviewJobs=%d comments=%d streams=%d boundary=%s", children, commentJobs, reviewJobs, comments, streams, boundary)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ingestion.SignalActivity(ctx, tx, 72, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind='activity_comments' AND key='repo:72'`).Scan(&commentJobs); err != nil || commentJobs != 1 {
		t.Fatalf("comment stream job=%d err=%v", commentJobs, err)
	}
	if err = goose.DownToContext(ctx, db, "migrations", 15); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity_resources WHERE resource='comments'`).Scan(&streams); err != nil || streams != 0 {
		t.Fatalf("rollback left %d comment streams (%v)", streams, err)
	}
}

func TestDemoActionsSeedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dbURL, pool := testutil.Database(t)
	for i := 0; i < 2; i++ {
		if err := platform.SeedDemo(ctx, dbURL); err != nil {
			t.Fatal(err)
		}
		var workflows, runs, jobs, deployments, launchpadDeployments, linkedFailures int
		err := pool.QueryRow(ctx, `SELECT
   (SELECT count(*) FROM workflows), (SELECT count(*) FROM workflow_runs), (SELECT count(*) FROM workflow_jobs),
   (SELECT count(*) FROM deployments WHERE repository_id=1), (SELECT count(*) FROM deployments WHERE repository_id=2),
   (SELECT count(*) FROM issue_pull_request_links l JOIN activity_issues i ON i.repository_id=l.repository_id AND i.number=l.issue_number
    JOIN activity_pull_requests p ON p.repository_id=l.repository_id AND p.number=l.pull_request_number
    JOIN workflow_runs r ON r.repository_id=p.repository_id AND p.number=ANY(r.pull_request_numbers)
    JOIN workflow_jobs j ON j.repository_id=r.repository_id AND j.run_id=r.source_id
    WHERE i.state='open' AND p.state='open' AND r.conclusion='failure' AND j.html_url <> '')`).Scan(&workflows, &runs, &jobs, &deployments, &launchpadDeployments, &linkedFailures)
		if err != nil {
			t.Fatal(err)
		}
		if workflows != 5 || runs != 8 || jobs != 8 || deployments != 2 || launchpadDeployments != 0 || linkedFailures != 2 {
			t.Fatalf("workflows=%d runs=%d jobs=%d deployments=%d launchpadDeployments=%d linkedFailures=%d", workflows, runs, jobs, deployments, launchpadDeployments, linkedFailures)
		}
	}
}
