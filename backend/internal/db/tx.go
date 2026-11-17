package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InTx runs fn in one transaction: it commits when fn returns nil and rolls back on any error (or a panic).
// The rollback ignores ctx cancellation, so an attempt cancelled mid-flight still releases its locks.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // a no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
