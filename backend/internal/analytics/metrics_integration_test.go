//go:build integration

package analytics

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func TestRepositoryMetrics(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	fixture, err := os.ReadFile("testdata/analytics.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(fixture)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 4, 0, 0, 0, time.FixedZone("PST", -8*3600))
	load := func() *Overview {
		t.Helper()
		out, err := LoadRepositoryOverview(ctx, pool, 10, 7, now)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := load()
	for _, check := range []struct {
		name string
		got  *int64
		want int64
	}{
		{"opened", out.IssuesOpened.Value, 2},
		{"closed", out.IssuesClosed.Value, 1},
		{"stale", out.StaleOpenIssues.Value, 0},
		{"releases", out.ReleasesPublished.Value, 0},
		{"first response floor median", out.FirstResponseTime.Seconds, 20},
		{"first response sample", out.FirstResponseTime.SampleSize, 2},
		{"first review", out.FirstReviewTime.Seconds, 3},
		{"never reviewed", out.PullRequestsNeverReviewed.Value, 1},
		{"merge", out.MergeTime.Seconds, 3600},
		{"humans", out.ActiveContributors.Value, 2},
		{"bots", out.ActiveBotContributors.Value, 1},
		{"first day", out.Daily[0].IssuesOpened, 1},
		{"last day", out.Daily[6].PullRequestsOpened, 1},
	} {
		if check.got == nil || *check.got != check.want {
			t.Errorf("%s: got %v, want %d", check.name, check.got, check.want)
		}
	}
	if out.IssuesReopened.Value != nil || *out.IssuesReopened.Coverage.Reason != ReasonHistoricalGap {
		t.Fatalf("expected unknown reopen count: %+v", out.IssuesReopened)
	}
	if len(out.Daily) != 7 || len(out.Timeline) != 4 || out.Timeline[0].Kind != "issue_opened" {
		t.Fatalf("unexpected daily or timeline: %+v", out)
	}
	// Humans first, then bots; comments count only for PR authors and reviewers, in the window, in this repository.
	wantContributors := []Contributor{
		{Login: "reviewer", ReviewsSubmitted: 1, CommentsCreated: 2},
		{Login: "author", PullRequestsOpened: 1, CommentsCreated: 1},
		{Login: "bot", IsBot: true, PullRequestsOpened: 1, ReviewsSubmitted: 1, CommentsCreated: 1},
	}
	if len(out.Contributors) != len(wantContributors) {
		t.Fatalf("contributors: %+v", out.Contributors)
	}
	for i, want := range wantContributors {
		if out.Contributors[i] != want {
			t.Errorf("contributor %d: got %+v, want %+v", i, out.Contributors[i], want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_resources SET watermark=NULL WHERE repository_id=10 AND resource='comments'`); err != nil {
		t.Fatal(err)
	}
	out = load()
	if len(out.Contributors) != 0 || *out.ContributorsCoverage.Reason != ReasonNotSynchronized || out.ActiveContributors.Value == nil ||
		out.FirstResponseTime.SampleSize != nil || out.UnansweredOpenIssues.Value != nil || out.IssuesOpened.Value == nil {
		t.Fatalf("an unsynchronized comment stream must hide only comment-derived metrics: %+v", out)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_resources SET watermark='2026-11-17 09:00Z' WHERE repository_id=10 AND resource='comments'`); err != nil {
		t.Fatal(err)
	}
	// An open issue created before the comment boundary may have replies the stream never fetched.
	if _, err := pool.Exec(ctx, `UPDATE activity_resources SET boundary='2026-11-11 00:00:01Z' WHERE repository_id=10 AND resource='comments'`); err != nil {
		t.Fatal(err)
	}
	out = load()
	if out.UnansweredOpenIssues.Value != nil || *out.UnansweredOpenIssues.Coverage.Reason != ReasonWindowBeforeBoundary || out.OpenIssues.Value == nil {
		t.Fatalf("old open issue must make unanswered unknown: %+v", out.UnansweredOpenIssues)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_resources SET boundary='2026-08-18 00:00Z' WHERE repository_id=10 AND resource='comments'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_resources SET watermark=NULL WHERE repository_id=10 AND resource='comments'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM activity_children WHERE resource='reviews' AND parent_id=101`); err != nil {
		t.Fatal(err)
	}
	out = load()
	if out.FirstReviewTime.SampleSize != nil || *out.FirstReviewTime.Coverage.Reason != ReasonChildrenIncomplete ||
		out.ActiveContributors.Value != nil || len(out.Contributors) != 0 || out.PullRequestsMerged.Value == nil {
		t.Fatalf("missing child coverage affected the wrong metrics: %+v", out)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_resources SET boundary='2026-11-16 00:00Z' WHERE repository_id=10 AND resource='pull_requests'`); err != nil {
		t.Fatal(err)
	}
	out = load()
	if c := out.PullRequestsMerged; c.Value != nil || c.Coverage.Reason == nil || *c.Coverage.Reason != ReasonWindowBeforeBoundary {
		t.Fatalf("PR window predates imported history: %+v", c)
	}

}
