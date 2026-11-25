//go:build integration

package ingestion_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

func statusIssue(number int64, state string, labels ...string) fake.IssueFixture {
	now := time.Now().UTC().Add(-time.Minute)
	issue := fake.IssueFixture{ID: 7000 + number, Number: number, Title: fmt.Sprintf("issue %d", number), State: state, CreatedAt: now, UpdatedAt: now,
		User: fake.IssueActor{ID: 9, Login: "bot", Type: "Bot"}}
	if state == "closed" {
		issue.ClosedAt = &now
	}
	for i, l := range labels {
		issue.Labels = append(issue.Labels, fake.IssueLabel{ID: number*10 + int64(i) + 1, Name: l})
	}
	return issue
}

// statusRepo is a public repository alice selected, with a GitHub App installation (id 9) holding Issues write unless
// changed, status labels set as given, and these issues on GitHub, synced into the projection.
func statusRepo(t *testing.T, e *env, labelsOn bool, issues ...fake.IssueFixture) int64 {
	t.Helper()
	ctx := context.Background()
	uid := e.signIn("alice")
	repoID := e.gh.AddRepo("alice", "board", false)
	e.gh.Install("alice", 9, repoID)
	e.store(uid, repoID, false)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e.gh.AppPublicKey = &key.PublicKey
	e.deps.AppKey = key
	e.deps.InstallationCredentials = githubapp.NewInstallationCredentials(e.ghc, key)
	if _, err := e.pool.Exec(ctx, `UPDATE repositories SET name='alice/board', status_labels=$2 WHERE id=$1`, repoID, labelsOn); err != nil {
		t.Fatal(err)
	}
	e.gh.SetIssues(repoID, issues...)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ingestion.RequestActivity(ctx, tx, repoID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		err := e.deps.SyncIssues(ctx, jobs.Job{Key: fmt.Sprintf("repo:%d", repoID)}, txFence{pool: e.pool})
		var deferred *jobs.Defer
		if err != nil && !errors.As(err, &deferred) {
			t.Fatal(err)
		}
	}
	return repoID
}

func (e *env) cleanupQueued(repoID int64) bool {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE kind='status_label_cleanup' AND key=$1`, fmt.Sprintf("repo:%d", repoID)).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

func (e *env) runCleanup(repoID int64) error {
	return e.deps.CleanupStatusLabels(context.Background(), jobs.Job{Kind: jobs.KindStatusLabelCleanup, Key: fmt.Sprintf("repo:%d", repoID)}, txFence{pool: e.pool})
}

func (e *env) labelDeletes() int {
	n := 0
	for _, tok := range e.gh.WriteTokens() {
		if strings.HasPrefix(tok, "installation_") {
			n++
		}
	}
	return n
}

func remoteLabels(e *env, repoID, number int64) []string {
	for _, issue := range e.gh.Issues(repoID) {
		if issue.Number == number {
			var out []string
			for _, l := range issue.Labels {
				out = append(out, l.Name)
			}
			return out
		}
	}
	return nil
}

func listBoard(e *env, repoID int64) ([]db.ListRepositoryBoardRow, error) {
	return db.New(e.pool).ListRepositoryBoard(context.Background(), db.ListRepositoryBoardParams{RepositoryID: repoID})
}

// Each issue's Board state comes from its synced labels and open/closed state (R15).
func TestBoardStateFromSyncedLabels(t *testing.T) {
	e := setup(t)
	repoID := statusRepo(t, e, true,
		statusIssue(1, "open"),
		statusIssue(2, "open", "status:todo", "bug"),
		statusIssue(3, "open", "Status:In-Progress"),
		statusIssue(4, "open", "status:todo", "status:review"),
		statusIssue(5, "closed", "status:review"),
		statusIssue(6, "closed"),
		statusIssue(7, "open", "status:blocked"))
	rows, err := listBoard(e, repoID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, r := range rows {
		got[r.Number] = r.Status.String
		if r.MultipleStatus != (r.Number == 4) {
			t.Errorf("#%d multiple_status=%v", r.Number, r.MultipleStatus)
		}
	}
	want := map[int64]string{1: "backlog", 2: "todo", 3: "in_progress", 4: "in_review", 5: "done", 6: "done", 7: "backlog"}
	for n, s := range want {
		if got[n] != s {
			t.Errorf("#%d status=%q want %q", n, got[n], s)
		}
	}
	// A label changed on GitHub shows on the next sync; turning labels off places nothing in a column.
	e.gh.SetIssues(repoID, statusIssue(1, "open", "status:review"))
	if _, err := e.pool.Exec(context.Background(), `UPDATE activity_resources SET phase='catchup', previous_scan=NULL WHERE repository_id=$1`, repoID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_ = e.deps.SyncIssues(context.Background(), jobs.Job{Key: fmt.Sprintf("repo:%d", repoID)}, txFence{pool: e.pool})
	}
	rows, _ = listBoard(e, repoID)
	if i := slices.IndexFunc(rows, func(r db.ListRepositoryBoardRow) bool { return r.Number == 1 }); i < 0 || rows[i].Status.String != "in_review" {
		t.Fatalf("label change not synced: %+v", rows)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE repositories SET status_labels=false WHERE id=$1`, repoID); err != nil {
		t.Fatal(err)
	}
	rows, _ = listBoard(e, repoID)
	if len(rows) == 0 || slices.ContainsFunc(rows, func(r db.ListRepositoryBoardRow) bool { return r.Status.Valid || r.MultipleStatus }) {
		t.Fatalf("repository without status labels has columns: %+v", rows)
	}
}

// Covers AE9 and R20: an issue closed on GitHub with status:review loses that label through the app, run by the real
// worker; a non-status label stays, the removal is recorded with the app as actor, and the issue shows in Done.
func TestClosedIssueLosesStatusLabelThroughApp(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review", "status:blocked"), statusIssue(2, "open", "status:todo"))
	if !e.cleanupQueued(repoID) {
		t.Fatal("sync did not queue the cleanup")
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM jobs WHERE kind <> 'status_label_cleanup'`); err != nil {
		t.Fatal(err)
	}
	worker := &jobs.Worker{Pool: e.pool, Poll: 10 * time.Millisecond}
	e.deps.Register(worker)
	runCtx, cancel := context.WithCancel(ctx)
	exited := make(chan error, 1)
	go func() { exited <- worker.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		if err := e.pool.QueryRow(ctx, `SELECT state FROM jobs WHERE kind='status_label_cleanup'`).Scan(&state); err != nil {
			break // a finished job's row is deleted
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-exited

	if got := remoteLabels(e, repoID, 1); !slices.Equal(got, []string{"status:blocked"}) {
		t.Fatalf("remote labels of #1: %v (job state %q)", got, state)
	}
	if got := remoteLabels(e, repoID, 2); !slices.Equal(got, []string{"status:todo"}) {
		t.Fatalf("an open issue lost its label: %v", got)
	}
	ops, err := db.New(e.pool).ListAppOperations(ctx, repoID)
	if err != nil || len(ops) != 1 || ops[0].IssueNumber != 1 || ops[0].Label != "status:review" || ops[0].State != "succeeded" {
		t.Fatalf("app operations: %+v err=%v", ops, err)
	}
	var local []string
	if err := e.pool.QueryRow(ctx, `SELECT array_agg(l.name ORDER BY l.name) FROM activity_issue_labels l JOIN activity_issues i ON i.source_id=l.issue_id WHERE i.repository_id=$1 AND i.number=1`, repoID).Scan(&local); err != nil || !slices.Equal(local, []string{"status:blocked"}) {
		t.Fatalf("local labels of #1: %v err=%v", local, err)
	}
	rows, _ := listBoard(e, repoID)
	if rows[len(rows)-1].Number != 1 || rows[len(rows)-1].Status.String != "done" {
		t.Fatalf("board: %+v", rows)
	}
	if e.labelDeletes() != 1 {
		t.Fatalf("installation writes: %v", e.gh.WriteTokens())
	}
}

// An issue write holding the per-issue lock makes the cleanup defer at once rather than wait or project under it; the
// next run settles the removal it already sent.
func TestCleanupDefersWhileAnIssueWriteHoldsTheLock(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review"))
	write, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Rollback(ctx)
	if err := ingestion.LockIssue(ctx, write, repoID, 1); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var deferred *jobs.Defer
	if err := e.runCleanup(repoID); !errors.As(err, &deferred) || deferred.Reason != ingestion.CodeAccessMoved {
		t.Fatalf("got %v, want a deferral while the issue is locked", err)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("cleanup waited %v for the issue lock", waited)
	}
	if err := write.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.runCleanup(repoID); err != nil {
		t.Fatal(err)
	}
	ops, err := db.New(e.pool).ListAppOperations(ctx, repoID)
	if err != nil || len(ops) != 1 || ops[0].State != "succeeded" {
		t.Fatalf("app operations: %+v err=%v", ops, err)
	}
}

// The app writes nothing where status labels are off, where no installation covers the repository, where the
// installation lacks Issues write, or where the issue was reopened on GitHub before the cleanup ran.
func TestStatusCleanupWritesNothingWithoutAuthority(t *testing.T) {
	closed := statusIssue(1, "closed", "status:review")
	t.Run("status labels off", func(t *testing.T) {
		e := setup(t)
		repoID := statusRepo(t, e, false, closed)
		if e.cleanupQueued(repoID) {
			t.Fatal("cleanup queued for a repository without status labels")
		}
		if err := e.runCleanup(repoID); err != nil || e.labelDeletes() != 0 {
			t.Fatalf("err=%v writes=%v", err, e.gh.WriteTokens())
		}
	})
	t.Run("no installation", func(t *testing.T) {
		e := setup(t)
		repoID := statusRepo(t, e, true, closed)
		e.gh.Uninstall(9)
		if err := e.runCleanup(repoID); err != nil || e.labelDeletes() != 0 || !slices.Equal(remoteLabels(e, repoID, 1), []string{"status:review"}) {
			t.Fatalf("err=%v writes=%v", err, e.gh.WriteTokens())
		}
		if rows, _ := listBoard(e, repoID); len(rows) != 1 || rows[0].Status.String != "done" {
			t.Fatalf("board: %+v", rows)
		}
	})
	t.Run("installation without Issues write", func(t *testing.T) {
		e := setup(t)
		repoID := statusRepo(t, e, true, closed)
		e.gh.SetInstallationIssues(9, "read")
		if err := e.runCleanup(repoID); err != nil || e.labelDeletes() != 0 {
			t.Fatalf("err=%v writes=%v", err, e.gh.WriteTokens())
		}
	})
	t.Run("reopened before the cleanup", func(t *testing.T) {
		e := setup(t)
		repoID := statusRepo(t, e, true, closed)
		e.gh.SetIssues(repoID, statusIssue(1, "open", "status:review"))
		if err := e.runCleanup(repoID); err != nil || e.labelDeletes() != 0 {
			t.Fatalf("err=%v writes=%v", err, e.gh.WriteTokens())
		}
		if rows, _ := listBoard(e, repoID); len(rows) != 1 || rows[0].Status.String != "in_review" {
			t.Fatalf("reopened issue not refreshed: %+v", rows)
		}
	})
}

// roundTrip lets a test rewrite fake GitHub answers.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func answer(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"message":"x"}`))}
}

// A refused removal is recorded as failed once and does not make the cleanup schedule itself again.
func TestRefusedRemovalDoesNotRequeueCleanup(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review"))
	if _, err := e.pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	e.ghc.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			return answer(http.StatusForbidden), nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	if err := e.runCleanup(repoID); err != nil {
		t.Fatal(err)
	}
	ops, _ := db.New(e.pool).ListAppOperations(ctx, repoID)
	if len(ops) != 1 || ops[0].State != "failed" {
		t.Fatalf("app operations: %+v", ops)
	}
	if e.cleanupQueued(repoID) {
		t.Fatal("a refused removal queued the cleanup again")
	}
}

// A removal GitHub applied but whose answer and read-back were lost stays unknown, and the next run settles it from
// what GitHub shows, without another removal.
func TestUnknownRemovalIsSettledByNextRun(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review"))
	lose := true
	e.ghc.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if lose && r.Method == http.MethodDelete {
			return answer(http.StatusBadGateway), err
		}
		if lose && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/1") && e.labelDeletes() > 0 {
			return answer(http.StatusBadGateway), err
		}
		return resp, err
	})
	if err := e.runCleanup(repoID); !upstreamDeferred(err) {
		t.Fatalf("a lost answer must defer the cleanup without spending an attempt, got %v", err)
	}
	ops, _ := db.New(e.pool).ListAppOperations(ctx, repoID)
	if len(ops) != 1 || ops[0].State != "unknown" {
		t.Fatalf("app operations after the lost answer: %+v", ops)
	}
	lose = false
	if err := e.runCleanup(repoID); err != nil {
		t.Fatal(err)
	}
	ops, _ = db.New(e.pool).ListAppOperations(ctx, repoID)
	if len(ops) != 1 || ops[0].State != "succeeded" || e.labelDeletes() != 1 {
		t.Fatalf("app operations=%+v deletes=%d", ops, e.labelDeletes())
	}
}

// upstreamDeferred reports whether err is the cleanup's fixed-interval deferral for temporary GitHub trouble.
func upstreamDeferred(err error) bool {
	var d *jobs.Defer
	return errors.As(err, &d) && d.Reason == ingestion.CodeUpstream && d.RetryAt.After(time.Now().Add(10*time.Minute))
}

// A removal GitHub never answers (and never applied) defers the cleanup on every run, reusing the one unknown
// operation instead of adding rows.
func TestUnansweredRemovalDefersCleanup(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review"))
	e.ghc.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			return answer(http.StatusBadGateway), nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	for run := 1; run <= 2; run++ {
		if err := e.runCleanup(repoID); !upstreamDeferred(err) {
			t.Fatalf("run %d: want an upstream deferral, got %v", run, err)
		}
		ops, _ := db.New(e.pool).ListAppOperations(ctx, repoID)
		if len(ops) != 1 || ops[0].State != "unknown" || e.labelDeletes() != 0 {
			t.Fatalf("run %d: app operations=%+v deletes=%d", run, ops, e.labelDeletes())
		}
	}
}

// A network failure defers the cleanup, including on the app-JWT calls (installation lookup, token mint).
func TestNetworkFailureDefersCleanup(t *testing.T) {
	for name, fails := range map[string]func(*http.Request) bool{
		"every request": func(*http.Request) bool { return true },
		"token mint":    func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/access_tokens") },
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review"))
			e.deps.InstallationCredentials = githubapp.NewInstallationCredentials(e.ghc, e.deps.AppKey) // no token cached by the sync
			e.ghc.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				if fails(r) {
					return nil, errors.New("dial: connection refused")
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			if err := e.runCleanup(repoID); !upstreamDeferred(err) {
				t.Fatalf("want an upstream deferral, got %v", err)
			}
		})
	}
}

// A permanent refusal (a plain 403) is not temporary: it still spends an attempt so an operator sees it.
func TestRefusedIssueReadSpendsAttempt(t *testing.T) {
	e := setup(t)
	repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review"))
	e.ghc.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/1") {
			return answer(http.StatusForbidden), nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	err := e.runCleanup(repoID)
	var d *jobs.Defer
	if err == nil || errors.As(err, &d) {
		t.Fatalf("want a plain error, got %v", err)
	}
}

// An issue GitHub reports as gone is skipped like a missing one, so it cannot block the rest of the batch.
func TestGoneIssueDoesNotBlockCleanup(t *testing.T) {
	e := setup(t)
	repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review"), statusIssue(2, "closed", "status:todo"))
	e.ghc.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/1") {
			return answer(http.StatusGone), nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	if err := e.runCleanup(repoID); err != nil {
		t.Fatal(err)
	}
	if got := remoteLabels(e, repoID, 2); len(got) != 0 {
		t.Fatalf("issue 2 labels after cleanup: %v", got)
	}
}

// A stale stored name pointing at another repository never receives a removal.
func TestCleanupUsesTheRepositoryByID(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	repoID := statusRepo(t, e, true, statusIssue(1, "closed", "status:review"))
	other := e.gh.AddRepo("alice", "other", false)
	e.gh.Install("alice", 10, other)
	e.gh.SetIssues(other, statusIssue(1, "closed", "status:review"))
	e.gh.Uninstall(9)
	if _, err := e.pool.Exec(ctx, `UPDATE repositories SET name='alice/other' WHERE id=$1`, repoID); err != nil {
		t.Fatal(err)
	}
	if err := e.runCleanup(repoID); err != nil || e.labelDeletes() != 0 || !slices.Equal(remoteLabels(e, other, 1), []string{"status:review"}) {
		t.Fatalf("err=%v writes=%v", err, e.gh.WriteTokens())
	}
}
