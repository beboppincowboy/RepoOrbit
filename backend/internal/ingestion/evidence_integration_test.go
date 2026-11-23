//go:build integration

package ingestion_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

type evidenceFixture struct {
	issueFixture
	pulls        []githubapp.PullRequest
	reviews      []githubapp.Review
	comments     []githubapp.IssueComment
	failReviews  bool
	pullCalls    int
	commentSince []time.Time // since passed on each first-page comment request
}

func (f *evidenceFixture) FetchPullRequestsCursor(context.Context, string, string, string) ([]githubapp.PullRequest, string, error) {
	f.pullCalls++
	return f.pulls, "", nil
}
func (f *evidenceFixture) FetchReviewsCursor(context.Context, string, string, int64, string) ([]githubapp.Review, string, error) {
	if f.failReviews {
		return nil, "", errors.New("failure")
	}
	return f.reviews, "", nil
}
func (f *evidenceFixture) FetchRepoCommentsCursor(_ context.Context, _, _, cursor string, since time.Time) ([]githubapp.IssueComment, string, error) {
	if cursor == "" {
		f.commentSince = append(f.commentSince, since)
	}
	return f.comments, "", nil
}

// signalActivity schedules a repository's activity (request) or reopens its caught-up streams for a catch-up.
func signalActivity(t *testing.T, e *env, repoID int64, request bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if request {
		err = ingestion.RequestActivity(ctx, tx, repoID, time.Now())
	} else {
		_, err = ingestion.SignalActivity(ctx, tx, repoID, time.Now())
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// drain runs a stream job until it reports nothing left to do, tolerating continue/retry deferrals.
func drain(t *testing.T, call func() error) {
	t.Helper()
	for i := 0; i < 10; i++ {
		err := call()
		if err == nil {
			return
		}
		if _, ok := err.(*jobs.Defer); !ok {
			t.Fatal(err)
		}
	}
	t.Fatal("stream did not settle")
}

func TestEvidenceBackfillAndChildIsolation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	e.store(uid, 70, false)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ingestion.RequestActivity(ctx, tx, 70, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-365 * 24 * time.Hour)
	merged := now.Add(-time.Hour)
	actor := githubapp.IssueUser{ID: 9, Login: "bot", Type: "Bot"}
	f := &evidenceFixture{pulls: []githubapp.PullRequest{{ID: 101, Number: 4, Title: "old merged", State: "closed", CreatedAt: old, UpdatedAt: now, MergedAt: &merged, User: &actor}}, reviews: []githubapp.Review{{ID: 201, User: &actor, State: "APPROVED", SubmittedAt: &old}, {ID: 202, User: &actor, State: "PENDING", Body: "private"}}, comments: []githubapp.IssueComment{{ID: 301, IssueNumber: 4, User: &actor, Body: "old reply", CreatedAt: old, UpdatedAt: old}}}
	e.deps.GitHub = f
	e.deps.Access = f
	parent := jobs.Job{Key: "repo:70"}
	review := jobs.Job{Key: "repo:70:parent:101"}
	comment := jobs.Job{Key: "repo:70"}
	fence := txFence{pool: e.pool}
	if err = e.deps.SyncPullRequests(ctx, parent, lostFence{}); !errors.Is(err, jobs.ErrLostClaim) {
		t.Fatalf("lost claim: %v", err)
	}
	var children int
	if err = e.pool.QueryRow(ctx, `SELECT count(*) FROM activity_children`).Scan(&children); err != nil || children != 0 {
		t.Fatalf("lost claim children=%d err=%v", children, err)
	}
	for i := 0; i < 3; i++ {
		err = e.deps.SyncPullRequests(ctx, parent, fence)
		if err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
	}
	// Only the review history is a child: comments sync as one repository-wide stream.
	if err = e.pool.QueryRow(ctx, `SELECT count(*) FROM activity_children WHERE resource='reviews'`).Scan(&children); err != nil || children != 1 {
		t.Fatalf("children=%d err=%v", children, err)
	}
	if err = e.deps.SyncComments(ctx, comment, lostFence{}); !errors.Is(err, jobs.ErrLostClaim) {
		t.Fatalf("child lost claim: %v", err)
	}
	var before int
	if err = e.pool.QueryRow(ctx, `SELECT count(*) FROM activity_comments`).Scan(&before); err != nil || before != 0 {
		t.Fatalf("lost child claim wrote %d comments: %v", before, err)
	}
	f.failReviews = true
	if err = e.deps.SyncReviews(ctx, review, fence); err == nil {
		t.Fatal("review failure expected")
	}
	f.failReviews = false
	// A review history that fits in one page completes without a confirming second scan.
	if err = e.deps.SyncReviews(ctx, review, fence); err != nil {
		t.Fatalf("single-page child did not complete in one run: %v", err)
	}
	drain(t, func() error { return e.deps.SyncComments(ctx, comment, fence) })
	var prs, reviews, comments, complete int
	err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_pull_requests),(SELECT count(*) FROM activity_reviews),(SELECT count(*) FROM activity_comments),(SELECT count(*) FROM activity_children WHERE complete)`).Scan(&prs, &reviews, &comments, &complete)
	if err != nil || prs != 1 || reviews != 1 || comments != 1 || complete != 1 {
		t.Fatalf("prs=%d reviews=%d comments=%d complete=%d err=%v", prs, reviews, comments, complete, err)
	}
	// Re-discovery must requeue history without allowing an older page to replace newer content.
	if _, err = e.pool.Exec(ctx, `UPDATE activity_resources SET phase='backfill' WHERE repository_id=70 AND resource IN ('pull_requests','comments')`); err != nil {
		t.Fatal(err)
	}
	f.comments[0].Body = "older replay"
	f.comments[0].UpdatedAt = old.Add(-time.Minute)
	f.pulls[0].Title = "older replay"
	f.pulls[0].UpdatedAt = old
	f.reviews[0].Body = "equal timestamp replay"
	if err = e.deps.SyncPullRequests(ctx, parent, fence); err != nil {
		if _, ok := err.(*jobs.Defer); !ok {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err = e.deps.SyncComments(ctx, comment, fence); err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
		if err = e.deps.SyncReviews(ctx, review, fence); err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
	}
	var body string
	if err = e.pool.QueryRow(ctx, `SELECT body FROM activity_comments WHERE source_id=301`).Scan(&body); err != nil || body != "old reply" {
		t.Fatalf("replay body=%q err=%v", body, err)
	}
	var title string
	if err = e.pool.QueryRow(ctx, `SELECT title FROM activity_pull_requests WHERE source_id=101`).Scan(&title); err != nil || title != "old merged" {
		t.Fatalf("replay title=%q err=%v", title, err)
	}
	if err = e.pool.QueryRow(ctx, `SELECT body FROM activity_reviews WHERE source_id=201`).Scan(&body); err != nil || body != "" {
		t.Fatalf("equal timestamp review body=%q err=%v", body, err)
	}
	// A visibility change observed after a child fetch must withhold the page.
	if _, err = e.pool.Exec(ctx, `UPDATE activity_resources SET phase='backfill' WHERE repository_id=70 AND resource IN ('pull_requests','comments')`); err != nil {
		t.Fatal(err)
	}
	if err = e.deps.SyncPullRequests(ctx, parent, fence); err != nil {
		if _, ok := err.(*jobs.Defer); !ok {
			t.Fatal(err)
		}
	}
	f.comments[0].Body = "must not commit"
	f.comments[0].UpdatedAt = now
	f.flip = true
	f.accesses = 0
	if err = e.deps.SyncComments(ctx, comment, fence); err == nil {
		t.Fatal("visibility change should defer")
	}
	if err = e.pool.QueryRow(ctx, `SELECT body FROM activity_comments WHERE source_id=301`).Scan(&body); err != nil || body != "old reply" {
		t.Fatalf("visibility fence body=%q err=%v", body, err)
	}
}

type shiftingComments struct {
	evidenceFixture
	shifted bool
	now     time.Time
}

func (f *shiftingComments) FetchRepoCommentsCursor(_ context.Context, _, _, cursor string, _ time.Time) ([]githubapp.IssueComment, string, error) {
	actor := githubapp.IssueUser{ID: 9, Login: "u", Type: "User"}
	makeComment := func(id int64) githubapp.IssueComment {
		return githubapp.IssueComment{ID: id, IssueNumber: 4, User: &actor, CreatedAt: f.now, UpdatedAt: f.now}
	}
	if cursor == "" {
		if f.shifted {
			return []githubapp.IssueComment{makeComment(1), makeComment(2)}, "page=2", nil
		}
		return []githubapp.IssueComment{makeComment(1)}, "page=2", nil
	}
	return []githubapp.IssueComment{makeComment(3)}, "", nil
}
func TestCommentCatchupReconcilesShiftedPages(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	e.store(uid, 70, false)
	if _, err := e.pool.Exec(ctx, `INSERT INTO activity_resources(repository_id,resource,boundary,phase) VALUES(70,'comments',now(),'catchup')`); err != nil {
		t.Fatal(err)
	}
	f := &shiftingComments{now: time.Now().UTC()}
	e.deps.GitHub = f
	e.deps.Access = f
	job := jobs.Job{Key: "repo:70"}
	fence := txFence{pool: e.pool}
	for i := 0; i < 2; i++ {
		if err := e.deps.SyncComments(ctx, job, fence); err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
	}
	f.shifted = true
	for i := 0; i < 2; i++ {
		if err := e.deps.SyncComments(ctx, job, fence); err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
	}
	var complete bool
	var count int
	if err := e.pool.QueryRow(ctx, `SELECT phase='current',(SELECT count(*) FROM activity_comments) FROM activity_resources WHERE repository_id=70 AND resource='comments'`).Scan(&complete, &count); err != nil || complete || count != 3 {
		t.Fatalf("shift complete=%v count=%d err=%v", complete, count, err)
	}
	for i := 0; i < 2; i++ {
		if err := e.deps.SyncComments(ctx, job, fence); err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
	}
	if err := e.pool.QueryRow(ctx, `SELECT phase='current' FROM activity_resources WHERE repository_id=70 AND resource='comments'`).Scan(&complete); err != nil || !complete {
		t.Fatalf("stable scan complete=%v err=%v", complete, err)
	}
}

func TestNullActorsPersistWithoutIdentity(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	e.store(uid, 70, false)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ingestion.RequestActivity(ctx, tx, 70, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f := &evidenceFixture{issueFixture: issueFixture{pages: map[int][]githubapp.Issue{1: {{ID: 100, Number: 3, State: "open", CreatedAt: now, UpdatedAt: now}}}}, pulls: []githubapp.PullRequest{{ID: 101, Number: 4, Title: "PR", State: "open", CreatedAt: now, UpdatedAt: now}}, reviews: []githubapp.Review{{ID: 201, State: "APPROVED", SubmittedAt: &now}}, comments: []githubapp.IssueComment{{ID: 301, IssueNumber: 4, CreatedAt: now, UpdatedAt: now}}}
	e.deps.GitHub = f
	e.deps.Access = f
	fence := txFence{pool: e.pool}
	for _, call := range []func() error{func() error { return e.deps.SyncIssues(ctx, jobs.Job{Key: "repo:70"}, fence) }, func() error { return e.deps.SyncPullRequests(ctx, jobs.Job{Key: "repo:70"}, fence) }, func() error { return e.deps.SyncReviews(ctx, jobs.Job{Key: "repo:70:parent:101"}, fence) }, func() error { return e.deps.SyncComments(ctx, jobs.Job{Key: "repo:70"}, fence) }} {
		if err := call(); err != nil {
			if _, ok := err.(*jobs.Defer); !ok {
				t.Fatal(err)
			}
		}
	}
	var count int
	err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issues WHERE source_id=100 AND author_id IS NULL AND author_login IS NULL AND author_type IS NULL)+(SELECT count(*) FROM activity_pull_requests WHERE source_id=101 AND author_id IS NULL AND author_login IS NULL AND author_type IS NULL)+(SELECT count(*) FROM activity_reviews WHERE source_id=201 AND author_id IS NULL AND author_login IS NULL AND author_type IS NULL)+(SELECT count(*) FROM activity_comments WHERE source_id=301 AND author_id IS NULL AND author_login IS NULL AND author_type IS NULL)`).Scan(&count)
	if err != nil || count != 4 {
		t.Fatalf("null actors=%d err=%v", count, err)
	}
}

func TestDisabledPullRequestsSkipFetch(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.store(e.signIn("alice"), 70, false)
	if _, err := e.pool.Exec(ctx, `INSERT INTO activity_resources(repository_id, resource, boundary) VALUES(70, 'pull_requests', now())`); err != nil {
		t.Fatal(err)
	}
	enabled := false
	f := &evidenceFixture{issueFixture: issueFixture{hasPullRequests: &enabled}}
	e.deps.GitHub, e.deps.Access = f, f
	job := jobs.Job{Kind: "activity_pull_requests", Key: "repo:70"}
	if err := e.deps.SyncPullRequests(ctx, job, txFence{pool: e.pool}); err != nil {
		t.Fatal(err)
	}
	var phase, code string
	var hasWatermark bool
	if err := e.pool.QueryRow(ctx, `SELECT phase,last_error_code,watermark IS NOT NULL FROM activity_resources WHERE repository_id=70 AND resource='pull_requests'`).Scan(&phase, &code, &hasWatermark); err != nil {
		t.Fatal(err)
	}
	if f.pullCalls != 0 || phase != "current" || code != ingestion.CodePullRequestsDisabled || hasWatermark {
		t.Fatalf("calls=%d, phase=%s, code=%s, hasWatermark=%v", f.pullCalls, phase, code, hasWatermark)
	}
}

func TestCommentCatchupListsOnlyChangesSinceLastScan(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.store(e.signIn("alice"), 70, false)
	signal := func(request bool) { signalActivity(t, e, 70, request) }
	signal(true)
	now := time.Now().UTC()
	f := &evidenceFixture{comments: []githubapp.IssueComment{{ID: 301, IssueNumber: 4, Body: "first", CreatedAt: now, UpdatedAt: now}, {ID: 302, IssueNumber: 7, Body: "on a PR", CreatedAt: now, UpdatedAt: now}}}
	e.deps.GitHub, e.deps.Access = f, f
	job, fence := jobs.Job{Key: "repo:70"}, txFence{pool: e.pool}
	drain(t, func() error { return e.deps.SyncComments(ctx, job, fence) })
	var boundary, watermark time.Time
	var phase string
	if err := e.pool.QueryRow(ctx, `SELECT boundary,watermark,phase FROM activity_resources WHERE repository_id=70 AND resource='comments'`).Scan(&boundary, &watermark, &phase); err != nil || phase != "current" {
		t.Fatalf("phase=%s err=%v", phase, err)
	}
	if len(f.commentSince) == 0 || !f.commentSince[0].Equal(boundary) {
		t.Fatalf("first scan since=%v, want boundary %v", f.commentSince, boundary)
	}
	// An unchanged repository: one catch-up run, one listing from the last converged scan, then nothing to do.
	signal(false)
	f.commentSince = nil
	if err := e.deps.SyncComments(ctx, job, fence); err != nil {
		if _, ok := err.(*jobs.Defer); !ok {
			t.Fatal(err)
		}
	}
	if err := e.deps.SyncComments(ctx, job, fence); err != nil {
		t.Fatalf("catch-up did not converge in one run: %v", err)
	}
	if len(f.commentSince) != 1 || !f.commentSince[0].Equal(watermark.Add(-5*time.Minute)) {
		t.Fatalf("catch-up since=%v, want one listing from watermark-5m %v", f.commentSince, watermark)
	}
	// An edited comment is picked up by the next catch-up; numbers come from the listing, including PR comments.
	f.comments = []githubapp.IssueComment{{ID: 301, IssueNumber: 4, Body: "edited", CreatedAt: now, UpdatedAt: now.Add(time.Minute)}}
	signal(false)
	drain(t, func() error { return e.deps.SyncComments(ctx, job, fence) })
	var body string
	var prNumber int64
	if err := e.pool.QueryRow(ctx, `SELECT (SELECT body FROM activity_comments WHERE source_id=301),(SELECT issue_number FROM activity_comments WHERE source_id=302)`).Scan(&body, &prNumber); err != nil || body != "edited" || prNumber != 7 {
		t.Fatalf("body=%q pr=%d err=%v", body, prNumber, err)
	}
	var children int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM activity_children WHERE resource='comments'`).Scan(&children); err != nil || children != 0 {
		t.Fatalf("comment children=%d err=%v", children, err)
	}
}

// A catch-up reopens a finished review history only when its pull request changed after that history's last
// converged scan, or when the history is a day old (a safety net for edits that do not move updated_at).
func TestReviewChildReopensOnlyWhenPullRequestChanged(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.store(e.signIn("alice"), 70, false)
	signal := func(request bool) { signalActivity(t, e, 70, request) }
	signal(true)
	created := time.Now().UTC().Add(-time.Hour)
	f := &evidenceFixture{pulls: []githubapp.PullRequest{{ID: 101, Number: 4, Title: "PR", State: "open", CreatedAt: created, UpdatedAt: created}}}
	e.deps.GitHub, e.deps.Access = f, f
	parent, review, fence := jobs.Job{Key: "repo:70"}, jobs.Job{Key: "repo:70:parent:101"}, txFence{pool: e.pool}
	drain(t, func() error { return e.deps.SyncPullRequests(ctx, parent, fence) })
	drain(t, func() error { return e.deps.SyncReviews(ctx, review, fence) })
	reopened := func() bool {
		t.Helper()
		signal(false)
		drain(t, func() error { return e.deps.SyncPullRequests(ctx, parent, fence) })
		var complete bool
		if err := e.pool.QueryRow(ctx, `SELECT complete FROM activity_children WHERE repository_id=70 AND resource='reviews' AND parent_id=101`).Scan(&complete); err != nil {
			t.Fatal(err)
		}
		drain(t, func() error { return e.deps.SyncReviews(ctx, review, fence) })
		return !complete
	}
	if reopened() {
		t.Fatal("unchanged pull request reopened its review history")
	}
	f.pulls[0].UpdatedAt = time.Now().UTC().Add(time.Minute)
	if !reopened() {
		t.Fatal("changed pull request did not reopen its review history")
	}
	if _, err := e.pool.Exec(ctx, `UPDATE activity_children SET watermark=watermark - interval '25 hours' WHERE resource='reviews'`); err != nil {
		t.Fatal(err)
	}
	f.pulls[0].UpdatedAt = created
	if !reopened() {
		t.Fatal("day-old review history was not rescanned")
	}
}
