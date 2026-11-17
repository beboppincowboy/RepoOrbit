// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
)

// Database creates an empty, migrated database for one test and drops it afterwards. It also changes
// the working directory to the backend root, where platform.Migrate and SeedDemo find their files.
func Database(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	raw := os.Getenv("DATABASE_URL")
	if raw == "" {
		t.Fatal("DATABASE_URL required")
	}
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
	t.Chdir(dir)
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("repoorbit_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); admin.Close() })
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	if err = platform.Migrate(ctx, u.String()); err != nil {
		t.Fatal(err)
	}
	pool, err := platform.OpenDB(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return u.String(), pool
}
