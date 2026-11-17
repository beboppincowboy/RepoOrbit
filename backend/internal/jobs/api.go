// Package jobs is the durable PostgreSQL work queue (U3a).
//
// CONTRACT STUB: signatures are frozen by docs/plans/2026-11-16-u3-contracts.md.
// U3a replaces the bodies (and may add files); other units must not edit this package.
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Kind names a job type. One job row exists per (Kind, Key).
type Kind string

const (
	KindRepoMetadataRefresh Kind = "repo_metadata_refresh" // Key "repo:<repositories.id>"
	KindStarReconcile       Kind = "star_reconcile"        // Key "user:<users.id>"
)

// Clock is injected everywhere timing matters.
type Clock interface{ Now() time.Time }

// Job is a claimed unit of work. Generation is the claim generation used for fencing.
type Job struct {
	ID         int64
	Kind       Kind
	Key        string
	Generation int64
	Attempt    int
}

var ErrNotImplemented = errors.New("jobs: not implemented (U3a)")

// Enqueue schedules (Kind, Key) inside the caller's transaction so selection changes and
// scheduling commit atomically. Semantics, all inside tx:
//   - no row: insert queued at runAt
//   - queued: coalesce (keep the earlier runAt)
//   - running: set pending=true so a follow-up runs after completion (intent is never lost)
//   - paused: record pending=true; stays paused until Resume
//   - dead: record pending=true only; NEVER revives (dead-work barrier). Only Requeue revives.
func Enqueue(ctx context.Context, tx pgx.Tx, kind Kind, key string, runAt time.Time) error {
	return ErrNotImplemented
}

// Resume moves a paused job to queued at runAt (eligibility changed). No-op for other states.
func Resume(ctx context.Context, tx pgx.Tx, kind Kind, key string, runAt time.Time) error {
	return ErrNotImplemented
}

// Handler runs one claimed job. Return values:
//   - nil: success; if pending was set during execution a follow-up is queued
//   - *Defer: wait until Until; does NOT consume an attempt (rate limit)
//   - *Pause: park until Resume; makes no remote calls and consumes no attempt (no credential)
//   - Permanent(err): job becomes dead
//   - any other error: retried with backoff+jitter, at most 5 attempts, then dead
type Handler func(ctx context.Context, job Job, fence Fence) error

// Fence guards effects. Commit runs fn in a transaction that first verifies this job still
// holds its claim (generation + lease) and fails with ErrLostClaim otherwise. No network
// calls inside fn. Handlers must write projections only through Commit.
type Fence interface {
	Commit(ctx context.Context, fn func(tx pgx.Tx) error) error
}

var ErrLostClaim = errors.New("jobs: claim lost")

type Defer struct {
	Until  time.Time
	Reason string
}

func (d *Defer) Error() string { return "jobs: deferred: " + d.Reason }

type Pause struct{ Reason string }

func (p *Pause) Error() string { return "jobs: paused: " + p.Reason }

type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent marks err as non-retryable.
func Permanent(err error) error { return permanent{err} }

// Worker claims and runs jobs: 2 concurrent, 60s lease, 20s heartbeat, 5min max per attempt.
type Worker struct {
	Clock Clock
	// internal fields are U3a's
}

func (w *Worker) Register(kind Kind, h Handler) {}
func (w *Worker) Run(ctx context.Context) error { return ErrNotImplemented }

// Requeue is the operator command: revive a dead job with a fresh attempt budget.
// Callers recheck eligibility afterwards (U3b handler does this on execution).
func Requeue(ctx context.Context, tx pgx.Tx, kind Kind, key string, runAt time.Time) error {
	return ErrNotImplemented
}
