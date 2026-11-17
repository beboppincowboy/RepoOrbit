// Package jobs is the durable PostgreSQL work queue. worker.go holds the claim/run loop.
package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/db"
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
	Attempt    int // 1-based: the number of failed attempts so far plus this one; Defer and Pause do not count
}

// Enqueue schedules (Kind, Key) inside the caller's transaction so selection changes and
// scheduling commit atomically. Semantics, all inside tx:
//   - no row: insert queued at runAt
//   - queued: coalesce (keep the earlier runAt)
//   - running: set pending=true so a follow-up runs after completion (intent is never lost)
//   - paused: record pending=true; stays paused until Resume
//   - dead: record pending=true only; NEVER revives (dead-work barrier). Only RequeueDead revives.
func Enqueue(ctx context.Context, tx pgx.Tx, kind Kind, key string, runAt time.Time) error {
	return db.New(tx).UpsertJob(ctx, db.UpsertJobParams{Kind: string(kind), Key: key, RunAt: ts(runAt)})
}

// Resume moves a PAUSED job to queued at runAt (eligibility changed). A RUNNING job is marked pending, so a Pause it
// is about to return re-queues it. No-op for queued and dead jobs;
// it never revives dead work (use RequeueDead, operator only). Runs inside the caller's transaction.
func Resume(ctx context.Context, tx pgx.Tx, kind Kind, key string, runAt time.Time) error {
	q := db.New(tx)
	if err := q.ResumeJob(ctx, db.ResumeJobParams{Kind: string(kind), Key: key, RunAt: ts(runAt)}); err != nil {
		return err
	}
	return q.MarkRunningPending(ctx, db.MarkRunningPendingParams{Kind: string(kind), Key: key})
}

// Handler runs one claimed job. Return values:
//   - nil: success; if pending was set during execution a follow-up is queued
//   - *Defer: wait until RetryAt; does NOT consume an attempt (rate limit)
//   - *Pause: park until Resume; makes no remote calls and consumes no attempt (no credential)
//   - Permanent(err): job becomes dead
//   - any other error: retried with backoff+jitter, at most 5 attempts, then dead
type Handler func(ctx context.Context, job Job, fence ClaimFence) error

// ClaimFence guards effects. Commit runs fn in a transaction that first verifies this job still
// holds its claim (generation + lease) and fails with ErrLostClaim otherwise. No network
// calls inside fn. Handlers must write projections only through Commit.
type ClaimFence interface {
	Commit(ctx context.Context, fn func(tx pgx.Tx) error) error
}

var ErrLostClaim = errors.New("jobs: claim lost")

type Defer struct {
	RetryAt time.Time
	Reason  string
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
	Pool  *pgxpool.Pool
	Clock Clock
	// Zero values use the documented defaults (2, 60s, 20s, 5min, 1s, 30s).
	Concurrency   int
	Lease         time.Duration
	Heartbeat     time.Duration
	MaxExecution  time.Duration
	Poll          time.Duration
	ShutdownGrace time.Duration
	// Jitter returns a value in [0,1); nil uses math/rand. Tests inject a constant.
	Jitter   func() float64
	handlers map[Kind]Handler // set by Register before Run
}

var ErrNotDead = errors.New("jobs: job is not dead")

// SystemClock is the production Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// RequeueDead is the operator command: revive a dead job with a fresh attempt budget.
// The job handler rechecks eligibility when it next runs.
// It returns ErrNotDead when no dead job exists for (kind, key).
func RequeueDead(ctx context.Context, tx pgx.Tx, kind Kind, key string, runAt time.Time) error {
	n, err := db.New(tx).RequeueJob(ctx, db.RequeueJobParams{Kind: string(kind), Key: key, RunAt: ts(runAt)})
	if err == nil && n == 0 {
		return ErrNotDead
	}
	return err
}
