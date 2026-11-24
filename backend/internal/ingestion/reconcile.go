package ingestion

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

// Fixed operating limits (no tuning controls until measurements justify them).
const (
	MetadataPollInterval  = 15 * time.Minute
	StarReconcileInterval = time.Hour
	jitterFraction        = 0.1 // each period varies by up to +-10%
	enqueueSpreadDivisor  = 15  // due times spread over interval/15 (one minute of a poll)
)

// ErrNoCredential means the user has no usable GitHub credential (repositories.ErrSignedOut, mapped by the worker's
// adapter). The star handler turns it into a pause: no remote call is retried and no attempt is spent.
var ErrNoCredential = errors.New("ingestion: no usable credential")

// StarFetch is the outcome of reading a user's stars from GitHub, without storing them.
type StarFetch struct {
	Repos []githubapp.Repo
	// Complete is true only when every page was read (the same word as repositories.StarFetch). A failed fetch stores
	// nothing and keeps the previous snapshot.
	Complete bool
	RetryAt  *time.Time // set when GitHub rate limited the fetch
}

// StarRefresher splits re-reading stars from storing them so the job can commit the snapshot, follow eligibility and
// resumed work in one claim-fenced transaction. The worker wiring adapts repositories.Service to it.
type StarRefresher interface {
	// FetchStars makes GitHub calls and writes nothing. It returns ErrNoCredential when the user is signed out or revoked.
	FetchStars(ctx context.Context, userID int64) (StarFetch, error)
	// StoreStars replaces the snapshot with a completed fetch, updates follow eligibility and rechecks paused work, all
	// through tx. The caller commits.
	StoreStars(ctx context.Context, tx pgx.Tx, userID int64, repos []githubapp.Repo) error
}

// SchedulerDeps is everything StartScheduler needs. Sleep and Rand default to real time and math/rand.
type SchedulerDeps struct {
	Pool   *pgxpool.Pool
	Worker *jobs.Worker // the star reconcile handler is registered here; the caller runs the worker
	Stars  StarRefresher
	Clock  jobs.Clock
	Sleep  func(ctx context.Context, d time.Duration) error
	Rand   func() float64 // [0,1)
}

func userKey(id int64) string { return "user:" + strconv.FormatInt(id, 10) }

// RecheckEligibility resumes paused work that a user's eligibility change may unblock: refreshes of the repositories
// they actively follow and their own star reconcile. A job that is running right now is marked pending instead, so a
// Pause it is about to return re-queues it. Call it inside the transaction of the sign-in, reselection, completed star
// store, or installation change. Never revives dead work (jobs.Resume only unparks paused jobs and flags running ones)
// and makes no remote call. Writes job rows via tx.
func RecheckEligibility(ctx context.Context, tx pgx.Tx, userID int64, now time.Time) error {
	ids, err := db.New(tx).ListEligibleRepositoryIDsForUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := jobs.Resume(ctx, tx, jobs.KindRepoMetadataRefresh, repoKey(id), now); err != nil {
			return err
		}
		if err := jobs.Resume(ctx, tx, jobs.KindActivityIssues, repoKey(id), now); err != nil {
			return err
		}
		if err := jobs.Resume(ctx, tx, jobs.KindActivityPullRequests, repoKey(id), now); err != nil {
			return err
		}
		for _, kind := range eventStreamKinds {
			if err := jobs.Resume(ctx, tx, kind, repoKey(id), now); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT parent_id FROM activity_children WHERE repository_id=$1 AND resource='reviews' AND NOT complete`, id)
		if err != nil {
			return err
		}
		parents, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, parent := range parents {
			if err := jobs.Resume(ctx, tx, jobs.KindActivityReviews, childKey(id, parent), now); err != nil {
				return err
			}
		}
		if err := RequestActivity(ctx, tx, id, now); err != nil {
			return err
		}
	}
	return jobs.Resume(ctx, tx, jobs.KindStarReconcile, userKey(userID), now)
}

// StarReconcileHandler refreshes one user's stars. It fetches first (no write), then stores the completed snapshot,
// suspends the user's starred selections that are no longer starred (the Selection stays), restores ones that are
// again, and rechecks paused work, all inside one claim-fenced commit: a worker that lost its claim writes nothing.
// A failed fetch changes nothing. No credential pauses without spending an attempt; a rate limit defers without one too
// and is shared with the metadata refresh through the credential throttle (q nil means no throttle store).
func StarReconcileHandler(stars StarRefresher, clock jobs.Clock, q *db.Queries) jobs.Handler {
	return func(ctx context.Context, job jobs.Job, fence jobs.ClaimFence) error {
		userID, err := parseKey("user:", job.Key)
		if err != nil {
			return jobs.Permanent(fmt.Errorf("bad star reconcile key %q", job.Key))
		}
		if until := credentialThrottledUntil(ctx, q, userID); until.After(clock.Now()) {
			return &jobs.Defer{RetryAt: until, Reason: "rate limited"} // pace the credential: no GitHub call at all
		}
		res, err := stars.FetchStars(ctx, userID)
		if err == nil && !res.Complete && res.RetryAt != nil {
			throttleCredential(ctx, q, userID, *res.RetryAt)
		}
		switch {
		case errors.Is(err, ErrNoCredential):
			return &jobs.Pause{Reason: "no usable credential"}
		case err != nil:
			return err
		case !res.Complete && res.RetryAt != nil:
			return &jobs.Defer{RetryAt: *res.RetryAt, Reason: "rate limited"}
		case !res.Complete:
			return errors.New("star refresh incomplete") // backoff retry; the previous snapshot and eligibility stay
		}
		return fence.Commit(ctx, func(tx pgx.Tx) error { return stars.StoreStars(ctx, tx, userID, res.Repos) })
	}
}

// jittered returns base varied by up to +-jitterFraction.
func jittered(base time.Duration, rnd func() float64) time.Duration {
	return time.Duration(float64(base) * (1 + (rnd()*2-1)*jitterFraction))
}

// duty is one periodic job of the scheduler.
type duty struct {
	name  string
	every time.Duration
	run   func(ctx context.Context, now time.Time) error
	next  time.Time
}

// runDuties runs each duty at startup and then every period (jittered), until ctx ends. A failing run is logged and
// retried next period; it never stops the loop.
func runDuties(ctx context.Context, clock jobs.Clock, sleep func(context.Context, time.Duration) error, rnd func() float64, duties []*duty) {
	start := clock.Now()
	for _, d := range duties {
		d.next = start
	}
	for ctx.Err() == nil {
		now := clock.Now()
		earliest := duties[0].next
		for _, d := range duties {
			if d.next.Before(earliest) {
				earliest = d.next
			}
		}
		if wait := earliest.Sub(now); wait > 0 {
			if sleep(ctx, wait) != nil {
				return
			}
			now = clock.Now()
		}
		for _, d := range duties {
			if d.next.After(now) {
				continue
			}
			if err := d.run(ctx, now); err != nil && ctx.Err() == nil {
				log.Printf("ingestion: %s reconciliation failed: %v", d.name, err)
			}
			d.next = now.Add(jittered(d.every, rnd))
		}
	}
}

// StartScheduler registers the star reconcile handler on the worker and starts the periodic reconciliation loop:
// at startup and then every 15 minutes it schedules refreshes of every followed repository and resumes paused ones;
// hourly it schedules star reconciliation for users who follow starred repositories and deletes issue-write operations
// past writeOperationRetention. Due times are spread by jitter.
// It only writes job rows (Enqueue never revives dead work); the jobs worker executes them, two at a time. The
// returned channel closes when the loop has stopped (ctx done).
func StartScheduler(ctx context.Context, d SchedulerDeps) <-chan struct{} {
	if d.Sleep == nil {
		d.Sleep = func(ctx context.Context, w time.Duration) error {
			t := time.NewTimer(w)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if d.Rand == nil {
		d.Rand = rand.Float64
	}
	d.Worker.Register(jobs.KindStarReconcile, StarReconcileHandler(d.Stars, d.Clock, db.New(d.Pool)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDuties(ctx, d.Clock, d.Sleep, d.Rand, []*duty{
			{name: "metadata poll", every: MetadataPollInterval, run: d.pollMetadata},
			{name: "activity poll", every: MetadataPollInterval, run: d.pollActivity},
			{name: "star", every: StarReconcileInterval, run: d.scheduleStars},
			{name: "write operation retention", every: StarReconcileInterval, run: d.expireWriteOperations},
		})
	}()
	return done
}

// pollMetadata enqueues a refresh for every followed repository (jittered due time) and resumes paused ones.
// Dead repositories stay dead: Enqueue records intent only, Resume ignores them.
func (d SchedulerDeps) pollMetadata(ctx context.Context, now time.Time) error {
	return db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeGlobal, 0); err != nil {
			return err
		}
		ids, err := db.New(tx).ListFollowedRepositoryIDs(ctx)
		if err != nil {
			return err
		}
		return d.enqueueAndResume(ctx, tx, jobs.KindRepoMetadataRefresh, repoKey, ids, now, MetadataPollInterval)
	})
}

func (d SchedulerDeps) scheduleStars(ctx context.Context, now time.Time) error {
	return db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		ids, err := db.New(tx).ListStarFollowerIDs(ctx)
		if err != nil {
			return err
		}
		return d.enqueueAndResume(ctx, tx, jobs.KindStarReconcile, userKey, ids, now, StarReconcileInterval)
	})
}

// enqueueAndResume records intent for each id at a jittered due time, then unparks it if paused. Enqueue never
// revives dead work and Resume ignores it.
func (d SchedulerDeps) enqueueAndResume(ctx context.Context, tx pgx.Tx, kind jobs.Kind, keyOf func(int64) string, ids []int64, now time.Time, interval time.Duration) error {
	for _, id := range ids {
		at := now.Add(spread(interval, d.Rand))
		if err := jobs.Enqueue(ctx, tx, kind, keyOf(id), at); err != nil {
			return err
		}
		if err := jobs.Resume(ctx, tx, kind, keyOf(id), at); err != nil {
			return err
		}
	}
	return nil
}

func spread(interval time.Duration, rnd func() float64) time.Duration {
	return time.Duration(rnd() * float64(interval) / enqueueSpreadDivisor)
}

// pollActivity repairs missed webhooks: every followed repository's caught-up streams are reopened for a catch-up
// scan at a jittered time, and paused streams resume. Public and private repositories take the same path; access
// is re-verified by the jobs themselves. Dead streams stay dead.
func (d SchedulerDeps) pollActivity(ctx context.Context, now time.Time) error {
	return db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		ids, err := db.New(tx).ListFollowedRepositoryIDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			at := now.Add(spread(MetadataPollInterval, d.Rand))
			if _, err := SignalActivity(ctx, tx, id, at); err != nil {
				return err
			}
			for _, kind := range activityParentKinds {
				if err := jobs.Resume(ctx, tx, kind, repoKey(id), at); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// writeOperationRetention is the age, measured from write_operations.created_at, after which a succeeded or failed
// issue-write operation is deleted and an unresolved one is wiped and redacted (and then kept indefinitely).
const (
	writeOperationRetention = 30 * 24 * time.Hour
	StalePendingTimeout     = time.Minute
)

func (d SchedulerDeps) expireWriteOperations(ctx context.Context, now time.Time) error {
	staleCutoff := now.Add(-StalePendingTimeout)
	cutoff := now.Add(-writeOperationRetention)
	return db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE write_operations SET state = 'failed',
			outcome = CASE WHEN draft IS NULL THEN redacted_write_outcome('{"error":"abandoned before sending; nothing was sent to GitHub"}'::jsonb)
				ELSE '{"error":"abandoned before sending; nothing was sent to GitHub"}'::jsonb END,
			updated_at = now()
			WHERE state = 'pending' AND created_at < $1`, staleCutoff); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM write_operations WHERE created_at < $1 AND state IN ('succeeded', 'failed')`, cutoff); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM app_operations WHERE created_at < $1`, cutoff); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE write_operations SET draft = NULL, outcome = redacted_write_outcome(outcome)
			WHERE created_at < $1 AND (draft IS NOT NULL OR NOT outcome ? 'redacted')`, cutoff)
		return err
	})
}
