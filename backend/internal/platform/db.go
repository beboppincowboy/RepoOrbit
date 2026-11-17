package platform

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// OpenDB opens a connection pool and pings it; the caller closes the pool.
func OpenDB(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Migrate applies schema migrations from ./migrations. Both modes run it.
func Migrate(ctx context.Context, url string) error {
	conn, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// SeedDemo loads the labeled demo rows from ./testdata/demo.sql (relative to the working directory, like
// Migrate's ./migrations). Only Demo mode calls it, so real mode never sees them.
func SeedDemo(ctx context.Context, url string) error {
	conn, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer conn.Close()
	seed, err := os.ReadFile("testdata/demo.sql")
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, string(seed))
	return err
}

// WithRequestDeadline bounds one request's database work to 3 seconds. Callers must call the returned cancel.
func WithRequestDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 3*time.Second)
}
