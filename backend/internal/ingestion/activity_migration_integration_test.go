//go:build integration

package ingestion_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

// Rolling back 00012 must restore the 00011 "unknown author" sentinel (0,'','') instead of failing on
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
