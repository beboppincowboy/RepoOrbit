package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

const maxAttempts = 5

var errTimeout = errors.New("jobs: execution time limit exceeded")

func (w *Worker) now() time.Time {
	if w.Clock == nil {
		return time.Now()
	}
	return w.Clock.Now()
}

func orDefault(v, d time.Duration) time.Duration {
	if v <= 0 {
		return d
	}
	return v
}

func (w *Worker) lease() time.Duration          { return orDefault(w.Lease, 60*time.Second) }
func (w *Worker) heartbeatEvery() time.Duration { return orDefault(w.Heartbeat, 20*time.Second) }
func (w *Worker) maxExecution() time.Duration   { return orDefault(w.MaxExecution, 5*time.Minute) }

// backoff is 30s doubling per spent attempt (capped at 1h) plus up to 20% jitter.
func (w *Worker) backoff(attempt int) time.Duration {
	d := min(30*time.Second<<(attempt-1), time.Hour)
	j := rand.Float64()
	if w.Jitter != nil {
		j = w.Jitter()
	}
	return d + time.Duration(float64(d)*0.2*j)
}

// Register binds a handler to a kind. Call before Run; the worker only claims registered kinds.
// Registering a kind twice replaces the earlier handler.
func (w *Worker) Register(kind Kind, h Handler) {
	if w.handlers == nil {
		w.handlers = map[Kind]Handler{}
	}
	w.handlers[kind] = h
}

// claim atomically takes the next due job (or a job whose lease expired) and commits the claim before any I/O.
func (w *Worker) claim(ctx context.Context) (Job, bool, error) {
	if len(w.handlers) == 0 {
		return Job{}, false, nil
	}
	kinds := make([]string, 0, len(w.handlers))
	for k := range w.handlers {
		kinds = append(kinds, string(k))
	}
	now := w.now()
	q := db.New(w.Pool)
	// A job that keeps killing its worker must not be reclaimed forever.
	if err := q.KillExhaustedJobs(ctx, db.KillExhaustedJobsParams{Now: ts(now), MaxAttempts: maxAttempts}); err != nil {
		return Job{}, false, err
	}
	r, err := q.ClaimJob(ctx, db.ClaimJobParams{LeaseUntil: ts(now.Add(w.lease())), Now: ts(now), Kinds: kinds})
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return Job{ID: r.ID, Kind: Kind(r.Kind), Key: r.Key, Generation: r.Generation, Attempt: int(r.Attempts) + 1}, true, nil
}

// heartbeat extends the lease; false means the claim was lost (expired or taken over).
func (w *Worker) heartbeat(ctx context.Context, j Job) bool {
	now := w.now()
	n, err := db.New(w.Pool).HeartbeatJob(ctx, db.HeartbeatJobParams{ID: j.ID, Generation: j.Generation, LeaseUntil: ts(now.Add(w.lease())), Now: ts(now)})
	return err == nil && n == 1
}

type fence struct {
	w   *Worker
	job Job
}

func (f fence) Commit(ctx context.Context, fn func(tx pgx.Tx) error) error {
	err := f.commit(ctx, fn)
	if err != nil && errors.Is(context.Cause(ctx), ErrLostClaim) {
		return ErrLostClaim // the heartbeat already saw the takeover and cancelled this attempt
	}
	return err
}

func (f fence) commit(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := f.w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// FOR SHARE keeps a reclaiming worker from taking the row until this transaction ends.
	_, err = db.New(tx).CheckClaim(ctx, db.CheckClaimParams{ID: f.job.ID, Generation: f.job.Generation, Now: ts(f.w.now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLostClaim
	}
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// runOnce claims and runs a single job to completion; false means nothing was due.
func (w *Worker) runOnce(ctx context.Context) (bool, error) {
	j, found, err := w.claim(ctx)
	if err != nil || !found {
		return false, err
	}
	w.execute(ctx, j)
	return true, nil
}

// execute runs the handler under a heartbeat and records the outcome. Every write is guarded by the claim
// generation and lease, so a worker that lost its claim changes nothing. hard is cancelled to force shutdown.
func (w *Worker) execute(hard context.Context, j Job) {
	started := w.now()
	jctx, cancel := context.WithCancelCause(hard)
	defer cancel(nil)
	stop := make(chan struct{})
	var hb sync.WaitGroup
	hb.Add(1)
	go func() {
		defer hb.Done()
		t := time.NewTicker(w.heartbeatEvery())
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if !w.heartbeat(jctx, j) {
					cancel(ErrLostClaim)
					return
				}
				if w.now().Sub(started) >= w.maxExecution() {
					cancel(errTimeout)
					return
				}
			}
		}
	}()
	err := w.call(jctx, j)
	close(stop)
	hb.Wait()

	fctx, fcancel := context.WithTimeout(context.WithoutCancel(hard), 10*time.Second)
	defer fcancel()
	if ferr := w.finish(fctx, hard, j, err, context.Cause(jctx)); ferr != nil {
		// The lease will lapse and the job is reclaimed; nothing else is safe to do.
		slog.Error("job outcome not recorded", "kind", j.Kind, "key", j.Key, "error", ferr)
	}
}

func (w *Worker) call(ctx context.Context, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	h, ok := w.handlers[j.Kind]
	if !ok {
		return Permanent(fmt.Errorf("no handler for kind %q", j.Kind))
	}
	return h(ctx, j, fence{w: w, job: j})
}

func reason(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (w *Worker) finish(ctx, hard context.Context, j Job, err error, cause error) error {
	q := db.New(w.Pool)
	now := w.now()
	var d *Defer
	var p *Pause
	var perm permanent
	var n int64
	var e error
	switch {
	case err == nil:
		if n, e = q.DeleteFinishedJob(ctx, db.DeleteFinishedJobParams{ID: j.ID, Generation: j.Generation, Now: ts(now)}); e == nil && n == 0 {
			// Intent arrived during execution (pending), or the claim is lost; the guarded follow-up tells them apart.
			_, e = q.FollowUpJob(ctx, db.FollowUpJobParams{ID: j.ID, Generation: j.Generation, Now: ts(now)})
		}
	case errors.Is(err, ErrLostClaim) || errors.Is(cause, ErrLostClaim):
		return nil // someone else owns the job now
	case hard.Err() != nil:
		// Forced shutdown: hand the job back without spending an attempt.
		_, e = q.ReleaseJob(ctx, db.ReleaseJobParams{ID: j.ID, Generation: j.Generation, Now: ts(now)})
	case errors.As(err, &d):
		_, e = q.DeferJob(ctx, db.DeferJobParams{ID: j.ID, Generation: j.Generation, RunAt: ts(d.RetryAt), Reason: reason(err), Now: ts(now)})
	case errors.As(err, &p):
		_, e = q.PauseJob(ctx, db.PauseJobParams{ID: j.ID, Generation: j.Generation, Reason: reason(err), Now: ts(now)})
	case errors.As(err, &perm):
		_, e = q.KillJob(ctx, db.KillJobParams{ID: j.ID, Generation: j.Generation, Reason: reason(err), Now: ts(now)})
	default:
		if errors.Is(cause, errTimeout) {
			err = errTimeout
		}
		_, e = q.RetryJob(ctx, db.RetryJobParams{ID: j.ID, Generation: j.Generation, MaxAttempts: maxAttempts, RunAt: ts(now.Add(w.backoff(j.Attempt))), Reason: reason(err), Now: ts(now)})
	}
	return e
}

// Run claims and runs jobs until ctx is cancelled, then waits up to ShutdownGrace (default 30s) for in-flight
// jobs to finish; stragglers are cancelled and released without spending an attempt.
func (w *Worker) Run(ctx context.Context) error {
	hard, hardCancel := context.WithCancel(context.Background())
	defer hardCancel()
	conc := w.Concurrency
	if conc <= 0 {
		conc = 2
	}
	slots := make(chan struct{}, conc)
	finished := make(chan struct{}, 1)
	var wg sync.WaitGroup
	poll := time.NewTicker(orDefault(w.Poll, time.Second))
	defer poll.Stop()
	for ctx.Err() == nil {
		for w.startOne(ctx, hard, slots, finished, &wg) {
		}
		select {
		case <-ctx.Done():
		case <-poll.C:
		case <-finished:
		}
	}
	grace := time.AfterFunc(orDefault(w.ShutdownGrace, 30*time.Second), hardCancel)
	defer grace.Stop()
	wg.Wait()
	return nil
}

// startOne claims one job into a free slot and runs it in the background; false when full or nothing is due.
func (w *Worker) startOne(ctx, hard context.Context, slots, finished chan struct{}, wg *sync.WaitGroup) bool {
	select {
	case slots <- struct{}{}:
	default:
		return false
	}
	j, found, err := w.claim(ctx)
	if err != nil || !found {
		<-slots
		if err != nil && ctx.Err() == nil {
			slog.Error("job claim failed", "error", err)
		}
		return false
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.execute(hard, j)
		<-slots
		select {
		case finished <- struct{}{}:
		default:
		}
	}()
	return true
}
