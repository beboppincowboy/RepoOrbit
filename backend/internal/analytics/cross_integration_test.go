//go:build integration

package analytics

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func TestCrossRepositoryOverview(t *testing.T) {
	_, pool := testutil.Database(t)
	ctx := context.Background()
	fixture, err := os.ReadFile("testdata/analytics.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(fixture)); err != nil {
		t.Fatal(err)
	}
	// Repository 30 is fully synchronized; repository 40 has never converged a scan; 20 is never passed in.
	for _, sql := range []string{
		`INSERT INTO repositories (id, name, description, is_private) VALUES (30, 'test/second', '', false), (40, 'test/syncing', '', false)`,
		`INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark)
		 SELECT 30, resource, '2026-08-18 00:00Z', 'catchup', '2026-11-17 10:30Z'
		 FROM unnest(ARRAY['issues', 'pull_requests', 'releases', 'issue_events', 'comments']) AS resource`,
		`INSERT INTO activity_issues (source_id, repository_id, number, title, state, html_url, created_at, updated_at, closed_at, author_id, author_type) VALUES
		 (501, 30, 1, 'Quiet issue', 'open', 'https://github.com/test/second/issues/1', '2026-09-01 00:00Z', '2026-09-02 00:00Z', null, 20, 'User'),
		 (502, 30, 2, 'Assigned to viewer', 'open', 'https://github.com/test/second/issues/2', '2026-11-12 00:00Z', '2026-11-16 00:00Z', null, 20, 'User'),
		 (503, 30, 3, 'Closed assigned', 'closed', '', '2026-11-12 00:00Z', '2026-11-13 00:00Z', '2026-11-13 00:00Z', 20, 'User'),
		 (504, 30, 4, 'Someone else', 'open', '', '2026-11-14 00:00Z', '2026-11-14 00:00Z', null, 20, 'User'),
		 (701, 40, 1, 'Syncing assigned', 'open', '', '2026-11-14 00:00Z', '2026-11-14 00:00Z', null, 20, 'User')`,
		`INSERT INTO activity_issue_assignees (issue_id, source_id, login, type) VALUES
		 (1, 77, 'Viewer', 'User'), (502, 77, 'viewer', 'User'), (503, 77, 'viewer', 'User'), (504, 78, 'other', 'User'),
		 (701, 77, 'viewer', 'User'), (4, 77, 'viewer', 'User')`,
		`INSERT INTO activity_comments (source_id, repository_id, issue_number, body, html_url, created_at, updated_at, author_id, author_type) VALUES
		 (551, 30, 1, '', '', '2026-09-01 01:00Z', '2026-09-01 01:00Z', 21, 'User'),
		 (552, 30, 4, '', '', '2026-11-14 01:00Z', '2026-11-14 01:00Z', 21, 'User')`,
		`INSERT INTO activity_pull_requests (source_id, repository_id, number, title, state, html_url, created_at, updated_at, merged_at, author_id, author_login, author_type) VALUES
		 (600, 30, 10, 'Needs review', 'open', 'https://github.com/test/second/pull/10', '2026-11-13 00:00Z', '2026-11-13 00:00Z', null, 20, 'a', 'User'),
		 (601, 30, 11, 'Merged', 'closed', '', '2026-11-14 00:00Z', '2026-11-15 00:00Z', '2026-11-15 00:00Z', 21, 'b', 'User'),
		 (800, 40, 2, 'Syncing PR', 'open', '', '2026-11-14 00:00Z', '2026-11-14 00:00Z', null, 21, 'b', 'User')`,
		`INSERT INTO activity_reviews (source_id, repository_id, pull_request_id, state, body, html_url, submitted_at, author_id, author_login, author_type) VALUES
		 (651, 30, 601, 'APPROVED', '', '', '2026-11-14 12:00Z', 20, 'a', 'User')`,
		`INSERT INTO activity_children (repository_id, resource, parent_id, parent_number, watermark)
		 SELECT 30, 'reviews', id, id - 590, '2026-11-17 10:00Z' FROM unnest(ARRAY[600, 601]) AS id`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cross, err := LoadCrossRepositoryOverview(ctx, pool, []int64{10, 30, 40}, 77, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if cross.RepositoriesCounted != 2 || cross.RepositoriesSyncing != 1 {
		t.Fatalf("counted %d syncing %d", cross.RepositoriesCounted, cross.RepositoriesSyncing)
	}

	// Parity: totals and the trend are sums of the per-repository overview for the same window.
	var openIssues, backlog int64
	daily := make([][2]int64, 7)
	for _, id := range []int64{10, 30} {
		one, err := LoadRepositoryOverview(ctx, pool, id, 7, now)
		if err != nil {
			t.Fatal(err)
		}
		openIssues += *one.OpenIssues.Value
		backlog += *one.UnreviewedOpenPullRequests.Value
		for i, d := range one.Daily {
			daily[i][0] += *d.IssuesOpened
			daily[i][1] += *d.PullRequestsMerged
		}
	}
	if cross.OpenIssues.Value == nil || *cross.OpenIssues.Value != openIssues || openIssues != 5 ||
		cross.ReviewBacklog.Value == nil || *cross.ReviewBacklog.Value != backlog || backlog != 2 {
		t.Fatalf("totals %+v %+v, want %d %d", cross.OpenIssues, cross.ReviewBacklog, openIssues, backlog)
	}
	if want := time.Date(2026, 11, 17, 10, 30, 0, 0, time.UTC); !cross.OpenIssues.Coverage.Through.Equal(want) {
		t.Fatalf("through is the minimum over counted repositories: %v", cross.OpenIssues.Coverage.Through)
	}
	if len(cross.Daily) != 7 {
		t.Fatalf("daily: %+v", cross.Daily)
	}
	for i, d := range cross.Daily {
		if *d.IssuesOpened != daily[i][0] || *d.PullRequestsMerged != daily[i][1] {
			t.Fatalf("day %d: got %d/%d want %v", i, *d.IssuesOpened, *d.PullRequestsMerged, daily[i])
		}
	}

	type row struct {
		kind   string
		repo   int64
		number int64
	}
	wantAttention := []row{{"stale_issue", 30, 1}, {"issue_no_response", 10, 3}, {"issue_no_response", 30, 2}, {"pr_awaiting_review", 30, 10}, {"pr_awaiting_review", 10, 6}}
	if len(cross.NeedsAttention) != len(wantAttention) {
		t.Fatalf("needs attention: %+v", cross.NeedsAttention)
	}
	for i, want := range wantAttention {
		got := cross.NeedsAttention[i]
		if (row{got.Kind, got.RepositoryID, got.Number}) != want {
			t.Errorf("attention %d: got %+v want %+v", i, got, want)
		}
	}
	if first := cross.NeedsAttention[0]; first.FullName != "test/second" || first.URL != "https://github.com/test/second/issues/1" ||
		!first.Since.Equal(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("stale row: %+v", first)
	}

	if len(cross.AssignedToMe) != 2 || cross.AssignedToMe[0].RepositoryID != 30 || cross.AssignedToMe[0].Number != 2 ||
		cross.AssignedToMe[0].FullName != "test/second" || cross.AssignedToMe[1].RepositoryID != 10 || cross.AssignedToMe[1].Number != 1 {
		t.Fatalf("assigned to me: %+v", cross.AssignedToMe)
	}

	// A repository whose comment stream is still importing stays counted: totals stay exact, and only that
	// repository's issue rows leave Needs attention.
	if _, err := pool.Exec(ctx, `INSERT INTO activity_issues (source_id, repository_id, number, title, state, html_url, created_at, updated_at, author_id, author_type)
		VALUES (505, 30, 5, 'New issue', 'open', '', '2026-11-17 09:00Z', '2026-11-17 09:00Z', 20, 'User');
		UPDATE activity_resources SET watermark=NULL, phase='backfill' WHERE repository_id=30 AND resource='comments'`); err != nil {
		t.Fatal(err)
	}
	pending, err := LoadCrossRepositoryOverview(ctx, pool, []int64{10, 30, 40}, 77, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	thirty, err := LoadRepositoryOverview(ctx, pool, 30, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	ten, err := LoadRepositoryOverview(ctx, pool, 10, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if pending.RepositoriesCounted != 2 || pending.RepositoriesSyncing != 1 || pending.OpenIssues.Value == nil ||
		*pending.OpenIssues.Value != *ten.OpenIssues.Value+*thirty.OpenIssues.Value || *pending.ReviewBacklog.Value != 2 {
		t.Fatalf("pending comment history changed what is counted: %+v", pending)
	}
	var kinds []string
	for _, item := range pending.NeedsAttention {
		if item.RepositoryID == 30 {
			kinds = append(kinds, item.Kind)
		}
	}
	if len(kinds) != 1 || kinds[0] != "pr_awaiting_review" {
		t.Fatalf("repository 30 rows with incomplete comments: %v", kinds)
	}
	// A pending review history makes the backlog unknown, as the workspace shows it, and hides that repository's PR rows.
	if _, err := pool.Exec(ctx, `INSERT INTO activity_pull_requests (source_id, repository_id, number, title, state, html_url, created_at, updated_at, author_id, author_login, author_type)
		VALUES (602, 30, 12, 'New PR', 'open', '', '2026-11-17 09:00Z', '2026-11-17 09:00Z', 20, 'a', 'User');
		INSERT INTO activity_children (repository_id, resource, parent_id, parent_number) VALUES (30, 'reviews', 602, 12)`); err != nil {
		t.Fatal(err)
	}
	pending, err = LoadCrossRepositoryOverview(ctx, pool, []int64{10, 30, 40}, 77, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if pending.RepositoriesCounted != 2 || pending.ReviewBacklog.Value != nil || *pending.ReviewBacklog.Coverage.Reason != ReasonChildrenIncomplete {
		t.Fatalf("backlog with pending review history: %+v", pending.ReviewBacklog)
	}
	for _, item := range pending.NeedsAttention {
		if item.RepositoryID == 30 {
			t.Fatalf("repository 30 rows with incomplete reviews: %+v", item)
		}
	}

	// A repository that is still syncing is left out, never zeroed.
	syncing, err := LoadCrossRepositoryOverview(ctx, pool, []int64{40}, 77, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if syncing.RepositoriesCounted != 0 || syncing.RepositoriesSyncing != 1 || syncing.OpenIssues.Value != nil ||
		*syncing.OpenIssues.Coverage.Reason != ReasonNotSynchronized || syncing.Daily[0].IssuesOpened != nil ||
		len(syncing.NeedsAttention) != 0 || len(syncing.AssignedToMe) != 0 {
		t.Fatalf("syncing repository was counted: %+v", syncing)
	}
}
