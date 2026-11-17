//go:build integration

package jobs

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const kind = KindRepoMetadataRefresh

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	clk  *fakeClock
	w    *Worker
}

func setup(t *testing.T) *env {
	t.Helper()
	_, pool := testutil.Database(t)
	clk := &fakeClock{t: time.Now().Truncate(time.Microsecond)}
	w := &Worker{Pool: pool, Clock: clk, Heartbeat: 5 * time.Millisecond, Poll: 5 * time.Millisecond, Jitter: func() float64 { return 0 }}
	return &env{t: t, pool: pool, clk: clk, w: w}
}

func (e *env) tx(fn func(tx pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	return nil
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) enqueue(key string) {
	e.t.Helper()
	e.must(e.tx(func(tx pgx.Tx) error { return Enqueue(context.Background(), tx, kind, key, e.clk.Now()) }))
}

func (e *env) resume(key string) {
	e.t.Helper()
	e.must(e.tx(func(tx pgx.Tx) error { return Resume(context.Background(), tx, kind, key, e.clk.Now()) }))
}

type row struct {
	State      string
	Pending    bool
	Attempts   int
	Generation int64
}

func (e *env) row(key string) (r row, exists bool) {
	e.t.Helper()
	err := e.pool.QueryRow(context.Background(), `SELECT state, pending, attempts, generation FROM jobs WHERE kind=$1 AND key=$2`, kind, key).
		Scan(&r.State, &r.Pending, &r.Attempts, &r.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false
	}
	e.must(err)
	return r, true
}

func (e *env) want(key, state string, attempts int) row {
	e.t.Helper()
	r, exists := e.row(key)
	if !exists || r.State != state || r.Attempts != attempts {
		e.t.Fatalf("job %s = %+v (exists %v), want state %s attempts %d", key, r, exists, state, attempts)
	}
	return r
}

func (e *env) gone(key string) {
	e.t.Helper()
	if r, exists := e.row(key); exists {
		e.t.Fatalf("job %s should be deleted, is %+v", key, r)
	}
}

func (e *env) runOnce() bool {
	e.t.Helper()
	ran, err := e.w.runOnce(context.Background())
	e.must(err)
	return ran
}

// runAsync runs one job on its own goroutine; receive from the result to wait for it.
func (e *env) runAsync() chan bool {
	done := make(chan bool, 1)
	go func() { ran, _ := e.w.runOnce(context.Background()); done <- ran }()
	return done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 600; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestConcurrentClaimsNeverShareAJob(t *testing.T) {
	e := setup(t)
	e.w.Register(kind, func(context.Context, Job, ClaimFence) error { return nil })
	const n = 30
	for i := 0; i < n; i++ {
		e.enqueue("k" + strconv.Itoa(i))
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, found, err := e.w.claim(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if !found {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("job %d claimed %d times", id, c)
		}
	}
}

func TestEnqueueSemantics(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	release := make(chan struct{})
	started := make(chan struct{})
	var first atomic.Bool
	e.w.Register(kind, func(ctx context.Context, j Job, f ClaimFence) error {
		if j.Key == "run" && first.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		return nil
	})
	// queued coalesces to the earlier runAt: one row
	e.must(e.tx(func(tx pgx.Tx) error { return Enqueue(ctx, tx, kind, "q", e.clk.Now().Add(time.Hour)) }))
	e.enqueue("q")
	var n int
	var runAt time.Time
	e.must(e.pool.QueryRow(ctx, `SELECT count(*), min(run_at) FROM jobs WHERE key='q'`).Scan(&n, &runAt))
	if n != 1 || !runAt.Equal(e.clk.Now()) {
		t.Fatalf("coalesce: n=%d runAt=%v want %v", n, runAt, e.clk.Now())
	}
	_, err := e.pool.Exec(ctx, `DELETE FROM jobs WHERE key='q'`)
	e.must(err)
	// running keeps intent as pending and a follow-up runs after completion
	e.enqueue("run")
	done := e.runAsync()
	<-started
	e.enqueue("run")
	if r, _ := e.row("run"); r.State != "running" || !r.Pending {
		t.Fatalf("enqueue while running = %+v", r)
	}
	close(release)
	<-done
	e.want("run", "queued", 0)
	// an uncontended success deletes the row
	if !e.runOnce() {
		t.Fatal("follow-up did not run")
	}
	e.gone("run")
	// paused records pending but only Resume queues it
	_, err = e.pool.Exec(ctx, `INSERT INTO jobs (kind,key,state,run_at) VALUES ($1,'p','paused',now())`, kind)
	e.must(err)
	e.enqueue("p")
	if r := e.want("p", "paused", 0); !r.Pending {
		t.Fatal("paused enqueue should record pending")
	}
	e.resume("p")
	e.want("p", "queued", 0)
}

func TestReclaimRejectsStaleWorkerEffects(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, err := e.pool.Exec(ctx, `CREATE TABLE effects (v text)`)
	e.must(err)
	e.enqueue("r")
	release := make(chan struct{})
	started := make(chan Job, 1)
	staleCommit := make(chan error, 1)
	effect := func(v string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `INSERT INTO effects VALUES ($1)`, v); return err }
	}
	e.w.Register(kind, func(ctx context.Context, j Job, f ClaimFence) error {
		if j.Generation == 1 {
			started <- j
			<-release
			staleCommit <- f.Commit(ctx, effect("stale"))
			return errors.New("stale failure") // must not schedule a retry over the new owner
		}
		return f.Commit(ctx, effect("fresh"))
	})
	done := e.runAsync()
	stale := <-started
	e.want("r", "running", 0)
	// the first worker "crashes": its lease lapses and a second worker reclaims
	e.clk.Advance(2 * time.Minute)
	if e.w.heartbeat(ctx, stale) {
		t.Fatal("heartbeat on an expired lease must be rejected")
	}
	if !e.runOnce() {
		t.Fatal("expired lease was not reclaimed")
	}
	e.gone("r")
	if e.w.heartbeat(ctx, stale) {
		t.Fatal("old-generation heartbeat accepted")
	}
	close(release)
	<-done
	if err := <-staleCommit; !errors.Is(err, ErrLostClaim) {
		t.Fatalf("stale Commit = %v, want ErrLostClaim", err)
	}
	var got []string
	rows, err := e.pool.Query(ctx, `SELECT v FROM effects`)
	e.must(err)
	for rows.Next() {
		var v string
		e.must(rows.Scan(&v))
		got = append(got, v)
	}
	if len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("effects = %v, want only the fresh owner's", got)
	}
	e.gone("r") // the stale failure neither retried nor resurrected the job
}

func TestStaleFailureCannotDisturbNewClaim(t *testing.T) {
	e := setup(t)
	e.enqueue("s")
	started := make(chan struct{})
	release := make(chan struct{})
	hold := make(chan struct{})
	e.w.Register(kind, func(ctx context.Context, j Job, f ClaimFence) error {
		if j.Generation == 1 {
			close(started)
			<-release
			return errors.New("late failure")
		}
		<-hold
		return nil
	})
	first := e.runAsync()
	<-started
	e.clk.Advance(2 * time.Minute)
	second := e.runAsync()
	waitFor(t, func() bool { r, _ := e.row("s"); return r.Generation == 2 })
	close(release)
	<-first
	e.want("s", "running", 1) // reclaim spent one attempt; the stale failure spent none
	close(hold)
	<-second
	e.gone("s")
}

func TestExecutionTimeLimitFailsTheAttempt(t *testing.T) {
	e := setup(t)
	e.enqueue("t")
	e.w.Register(kind, func(ctx context.Context, j Job, f ClaimFence) error {
		<-ctx.Done() // a stuck handler that honors cancellation
		return ctx.Err()
	})
	done := e.runAsync()
	finished := func() bool { return len(done) > 0 }
	// advance in lease-sized steps so heartbeats keep the claim valid, as in production
	for i := 0; i < 12 && !finished(); i++ {
		e.clk.Advance(30 * time.Second)
		waitFor(t, func() bool {
			if finished() {
				return true
			}
			var until time.Time
			_ = e.pool.QueryRow(context.Background(), `SELECT coalesce(lease_until, 'infinity') FROM jobs WHERE key='t'`).Scan(&until)
			return until.After(e.clk.Now().Add(40 * time.Second))
		})
	}
	<-done
	e.want("t", "queued", 1)
}

func TestTransientFailuresExhaustThenStayDeadUntilRequeue(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var fail atomic.Bool
	fail.Store(true)
	e.w.Register(kind, func(context.Context, Job, ClaimFence) error {
		if fail.Load() {
			return errors.New("boom")
		}
		return nil
	})
	e.enqueue("x")
	for i := 1; i <= 5; i++ {
		if !e.runOnce() {
			t.Fatalf("attempt %d did not run", i)
		}
		if i < 5 {
			e.want("x", "queued", i)
			if e.runOnce() {
				t.Fatalf("attempt %d ran before its backoff elapsed", i+1)
			}
			e.clk.Advance(10 * time.Minute)
		}
	}
	e.want("x", "dead", 5)
	// dead barrier: Enqueue/Resume record intent but never revive
	e.enqueue("x")
	e.resume("x")
	e.clk.Advance(24 * time.Hour)
	if e.runOnce() {
		t.Fatal("dead job ran")
	}
	if r := e.want("x", "dead", 5); !r.Pending {
		t.Fatal("dead enqueue should keep pending intent")
	}
	fail.Store(false)
	e.must(e.tx(func(tx pgx.Tx) error { return RequeueDead(ctx, tx, kind, "x", e.clk.Now()) }))
	e.want("x", "queued", 0) // fresh attempt budget
	if !e.runOnce() {
		t.Fatal("requeued job did not run")
	}
	e.gone("x")
	// Requeue of a job that is not dead reports it instead of silently doing nothing
	e.enqueue("y")
	err := e.tx(func(tx pgx.Tx) error { return RequeueDead(ctx, tx, kind, "y", e.clk.Now()) })
	if !errors.Is(err, ErrNotDead) {
		t.Fatalf("Requeue of a queued job = %v, want ErrNotDead", err)
	}
}

func TestCrashLoopExhaustsAttempts(t *testing.T) {
	e := setup(t)
	e.w.Register(kind, func(context.Context, Job, ClaimFence) error { return nil })
	e.enqueue("c")
	for i := 0; i < 5; i++ {
		if _, found, err := e.w.claim(context.Background()); err != nil || !found {
			t.Fatalf("claim %d: found=%v err=%v", i, found, err)
		}
		e.clk.Advance(2 * time.Minute) // each worker dies without finishing
	}
	if _, found, _ := e.w.claim(context.Background()); found {
		t.Fatal("a job that keeps killing its worker must go dead, not run forever")
	}
	e.want("c", "dead", 4)
}

func TestWaitsDoNotConsumeAttemptsAndPermanentIsDead(t *testing.T) {
	e := setup(t)
	mode := "defer"
	var until time.Time
	e.w.Register(kind, func(context.Context, Job, ClaimFence) error {
		switch mode {
		case "defer":
			return &Defer{RetryAt: until, Reason: "rate limited"}
		case "pause":
			return &Pause{Reason: "no credential"}
		}
		return Permanent(errors.New("gone"))
	})
	e.enqueue("d")
	for i := 0; i < 8; i++ { // more than the 5-attempt budget
		until = e.clk.Now().Add(10 * time.Minute)
		if !e.runOnce() {
			t.Fatalf("round %d did not run", i)
		}
		e.want("d", "queued", 0)
		if e.runOnce() {
			t.Fatal("deferred job ran before RetryAt")
		}
		e.clk.Advance(11 * time.Minute)
	}
	mode = "pause"
	e.runOnce()
	e.want("d", "paused", 0)
	e.clk.Advance(time.Hour)
	if e.runOnce() {
		t.Fatal("paused job ran without Resume")
	}
	mode = "perm"
	e.resume("d")
	e.runOnce()
	e.want("d", "dead", 0)
}

func TestGracefulShutdown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grace time.Duration
		stuck bool
	}{
		{"in-flight work finishes inside the grace period", 5 * time.Second, false},
		{"stuck work is cancelled after grace and released without spending an attempt", 50 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.w.ShutdownGrace = tc.grace
			e.enqueue("g")
			started := make(chan struct{})
			finish := make(chan struct{})
			var once sync.Once
			e.w.Register(kind, func(ctx context.Context, j Job, f ClaimFence) error {
				once.Do(func() { close(started) })
				if tc.stuck {
					<-ctx.Done()
					return ctx.Err()
				}
				<-finish
				return nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			ran := make(chan error, 1)
			go func() { ran <- e.w.Run(ctx) }()
			<-started
			cancel()
			if !tc.stuck {
				select {
				case <-ran:
					t.Fatal("Run returned before in-flight work finished")
				case <-time.After(100 * time.Millisecond):
				}
				close(finish)
			}
			if err := <-ran; err != nil {
				t.Fatalf("Run = %v", err)
			}
			if tc.stuck {
				e.want("g", "queued", 0)
			} else {
				e.gone("g")
			}
		})
	}
}

func TestRunProcessesQueuedWork(t *testing.T) {
	e := setup(t)
	var n atomic.Int32
	e.w.Register(kind, func(context.Context, Job, ClaimFence) error { n.Add(1); return nil })
	for _, k := range []string{"1", "2", "3"} {
		e.enqueue(k)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- e.w.Run(ctx) }()
	waitFor(t, func() bool { return n.Load() == 3 })
	cancel()
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
}

// A refresh requested while the job runs is not lost when the job then pauses: it is queued again (no attempt
// spent) so it re-evaluates with the new request; with nothing pending the job pauses as before.
func TestPauseKeepsRequestThatArrivedDuringTheRun(t *testing.T) {
	e := setup(t)
	arrive := false
	e.w.Register(kind, func(ctx context.Context, _ Job, _ ClaimFence) error {
		if arrive {
			e.enqueue("p") // a selection or webhook lands while this run is in flight
		}
		return &Pause{Reason: "no credential"}
	})
	e.enqueue("p")
	e.runOnce()
	e.want("p", "paused", 0)
	e.resume("p")
	arrive = true
	e.runOnce()
	r := e.want("p", "queued", 0)
	if r.Pending {
		t.Fatal("pending flag survived the re-queue")
	}
	arrive = false
	e.runOnce()
	e.want("p", "paused", 0) // nothing pending this time
}

// A Resume that lands while the job is still running (sign-in or an installation change arriving between the handler's
// no-credential decision and its Pause) is remembered, so the pause re-queues the job instead of parking it. A dead job
// stays dead.
func TestResumeDuringRunIsNotLostAndNeverRevivesDeadWork(t *testing.T) {
	e := setup(t)
	resumeDuringRun := true
	e.w.Register(kind, func(context.Context, Job, ClaimFence) error {
		if resumeDuringRun {
			e.resume("r")
		}
		return &Pause{Reason: "no credential"}
	})
	e.enqueue("r")
	e.runOnce()
	e.want("r", "queued", 0) // the resume that arrived mid-run survived the pause
	resumeDuringRun = false
	e.runOnce()
	e.want("r", "paused", 0)
	e.must(e.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE jobs SET state = 'dead' WHERE key = 'r'`)
		return err
	}))
	e.resume("r")
	e.want("r", "dead", 0)
}
