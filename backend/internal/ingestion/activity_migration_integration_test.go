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
