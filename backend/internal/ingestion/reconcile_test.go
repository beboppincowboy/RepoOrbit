package ingestion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// A sleep that advances the injected clock and stops the loop after n sleeps.
func steppingSleep(c *fakeClock, cancel context.CancelFunc, n int, waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		c.now = c.now.Add(d)
		if len(*waits) >= n {
			cancel()
		}
		return nil
	}
}

func TestRunDutiesSchedulesPollEveryQuarterHourAndStarsHourly(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	var polls, stars []time.Time
	duties := []*duty{
		{name: "poll", every: MetadataPollInterval, run: func(_ context.Context, now time.Time) error { polls = append(polls, now); return nil }},
		{name: "star", every: StarReconcileInterval, run: func(_ context.Context, now time.Time) error { stars = append(stars, now); return errors.New("boom") }},
	}
	runDuties(ctx, clock, steppingSleep(clock, cancel, 6, &waits), func() float64 { return 0.5 }, duties) // rnd 0.5 means no jitter
	if len(polls) != 7 || len(stars) != 2 {                                                               // t=0, then 15,30,45,60,75,90 ; t=0 and 60 (a failing duty keeps its schedule)
		t.Fatalf("polls=%d stars=%d", len(polls), len(stars))
	}
	if !polls[0].Equal(clock.now.Add(-90*time.Minute)) || !stars[1].Equal(polls[0].Add(time.Hour)) {
		t.Fatalf("times: %v %v", polls, stars)
	}
	for _, w := range waits {
		if w != 15*time.Minute {
			t.Fatalf("waits %v", waits)
		}
	}
}

func TestJitterStaysWithinTenPercent(t *testing.T) {
	for _, r := range []float64{0, 0.25, 0.999999} {
		got := jittered(15*time.Minute, func() float64 { return r })
		if got < 13*time.Minute+29*time.Second || got > 16*time.Minute+31*time.Second {
			t.Fatalf("r=%v got %v", r, got)
		}
	}
	if got := jittered(time.Hour, func() float64 { return 0 }); got != 54*time.Minute {
		t.Fatalf("got %v", got)
	}
}

type fakeStars struct {
	res      StarFetch
	err      error
	calls    int
	stored   int // StoreStars calls
	storeErr error
}

func (f *fakeStars) FetchStars(context.Context, int64) (StarFetch, error) {
	f.calls++
	return f.res, f.err
}

func (f *fakeStars) StoreStars(context.Context, pgx.Tx, int64, []githubapp.Repo) error {
	f.stored++
	return f.storeErr
}

// A nil fence proves the handler never commits unless the refresh completed.
func TestStarHandlerFailurePathsCommitNothingAndSpendNoAttempt(t *testing.T) {
	retry := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		stars *fakeStars
		check func(error) bool
	}{
		{"no credential pauses", &fakeStars{err: ErrNoCredential}, func(err error) bool { var p *jobs.Pause; return errors.As(err, &p) }},
		{"rate limit defers", &fakeStars{res: StarFetch{RetryAt: &retry}}, func(err error) bool { var d *jobs.Defer; return errors.As(err, &d) && d.RetryAt.Equal(retry) }},
		{"failed refresh retries ordinarily", &fakeStars{}, func(err error) bool {
			var p *jobs.Pause
			var d *jobs.Defer
			return err != nil && !errors.As(err, &p) && !errors.As(err, &d)
		}},
		{"transport error retries ordinarily", &fakeStars{err: errors.New("net")}, func(err error) bool { return err != nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := StarReconcileHandler(c.stars, &fakeClock{now: retry}, nil)
			if err := h(context.Background(), jobs.Job{Key: "user:7"}, nil); !c.check(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
	h := StarReconcileHandler(&fakeStars{}, &fakeClock{}, nil)
	err := h(context.Background(), jobs.Job{Key: "repo:7"}, nil)
	if err == nil || err.Error() != "bad star reconcile key \"repo:7\"" {
		t.Fatalf("got %v", err)
	}
}

// onceFence is a jobs.ClaimFence that either runs the commit function or reports a lost claim without running it.
type onceFence struct{ lost bool }

func (f onceFence) Commit(_ context.Context, fn func(pgx.Tx) error) error {
	if f.lost {
		return jobs.ErrLostClaim
	}
	return fn(nil)
}

// The snapshot is stored only inside the claim-fenced commit: a worker that lost its claim after fetching stores nothing.
func TestStarHandlerStoresOnlyInsideTheClaimFence(t *testing.T) {
	stars := &fakeStars{res: StarFetch{Complete: true}}
	h := StarReconcileHandler(stars, &fakeClock{}, nil)
	if err := h(context.Background(), jobs.Job{Key: "user:7"}, onceFence{lost: true}); !errors.Is(err, jobs.ErrLostClaim) {
		t.Fatalf("lost claim: %v", err)
	}
	if stars.calls != 1 || stars.stored != 0 {
		t.Fatalf("fetched %d, stored %d; a lost claim must store nothing", stars.calls, stars.stored)
	}
	if err := h(context.Background(), jobs.Job{Key: "user:7"}, onceFence{}); err != nil || stars.stored != 1 {
		t.Fatalf("held claim: err %v, stored %d", err, stars.stored)
	}
	stars.storeErr = errors.New("db")
	if err := h(context.Background(), jobs.Job{Key: "user:7"}, onceFence{}); err == nil {
		t.Fatal("a failing store must fail the job (backoff retry)")
	}
}

// Fetching never stores: the handler gets nothing to write when the fetch is incomplete.
func TestStarHandlerIncompleteFetchStoresNothing(t *testing.T) {
	stars := &fakeStars{}
	_ = StarReconcileHandler(stars, &fakeClock{}, nil)(context.Background(), jobs.Job{Key: "user:7"}, onceFence{})
	if stars.stored != 0 {
		t.Fatal("incomplete fetch reached the store")
	}
}
