package fake

import (
	"context"
	"fmt"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"net/http/httptest"
	"testing"
	"time"
)

func TestActivityPaginationAndAuthorization(t *testing.T) {
	s := New("client", "secret")
	s.access["token"] = "alice"
	s.access["other"] = "bob"
	id := s.AddRepo("alice", "project", true)
	s.Install("alice", 1, id)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	actor := IssueActor{ID: 1, Login: "alice", Type: "User"}
	prs := make([]PullRequestFixture, 101)
	for i := range prs {
		prs[i] = PullRequestFixture{ID: int64(i + 1), Number: int64(i + 1), Title: fmt.Sprint(i), State: "open", HTMLURL: "url", CreatedAt: now, UpdatedAt: now, User: actor}
	}
	s.SetPullRequests(id, prs...)
	s.SetReviews(id, 1, ReviewFixture{ID: 1, User: actor, State: "PENDING", HTMLURL: "url"})
	s.SetComments(id, 1, CommentFixture{ID: 1, User: actor, Body: "body", HTMLURL: "url", CreatedAt: now, UpdatedAt: now})
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	c := githubapp.New("", "", "", server.URL, "")
	ctx := context.Background()
	first, cursor, err := c.FetchPullRequestsCursor(ctx, "token", "alice/project", "")
	if err != nil || len(first) != 100 || cursor == "" {
		t.Fatalf("first=%d cursor=%q err=%v", len(first), cursor, err)
	}
	second, next, err := c.FetchPullRequestsCursor(ctx, "token", "alice/project", cursor)
	if err != nil || len(second) != 1 || next != "" {
		t.Fatalf("second=%d next=%q err=%v", len(second), next, err)
	}
	if _, _, err := c.FetchReviewsCursor(ctx, "other", "alice/project", 1, ""); err == nil {
		t.Fatal("private reviews accessible")
	}
	reviews, _, err := c.FetchReviewsCursor(ctx, "token", "alice/project", 1, "")
	if err != nil || len(reviews) != 1 || reviews[0].SubmittedAt != nil {
		t.Fatalf("reviews=%v err=%v", reviews, err)
	}
	comments, _, err := c.FetchIssueCommentsCursor(ctx, "token", "alice/project", 1, "")
	if err != nil || len(comments) != 1 || comments[0].Body != "body" {
		t.Fatalf("comments=%v err=%v", comments, err)
	}
}

func TestReleaseAndEventPaginationAndPrivateAccess(t *testing.T) {
	s := New("client", "secret")
	s.access["token"] = "alice"
	s.access["other"] = "bob"
	id := s.AddRepo("alice", "project", true)
	s.Install("alice", 1, id)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	releases := make([]ReleaseFixture, 101)
	events := make([]IssueEventFixture, 101)
	for i := range releases {
		releases[i] = ReleaseFixture{ID: int64(i + 1), TagName: "v1", HTMLURL: "url", CreatedAt: now, PublishedAt: &now}
		events[i] = IssueEventFixture{ID: int64(i + 1), Event: "closed", URL: "url", CreatedAt: now, Issue: &EventIssueFixture{ID: 2, Number: 3}}
	}
	s.SetReleases(id, releases...)
	s.SetIssueEvents(id, events...)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	c := githubapp.New("", "", "", server.URL, "")
	ctx := context.Background()
	for _, fetch := range []func(context.Context, string, string, string) (int, string, error){
		func(ctx context.Context, token, name, cursor string) (int, string, error) {
			x, n, e := c.FetchReleasesCursor(ctx, token, name, cursor)
			return len(x), n, e
		},
		func(ctx context.Context, token, name, cursor string) (int, string, error) {
			x, n, e := c.FetchIssueEventsCursor(ctx, token, name, cursor)
			return len(x), n, e
		},
	} {
		n, cursor, err := fetch(ctx, "token", "alice/project", "")
		if err != nil || n != 100 || cursor == "" {
			t.Fatalf("first=%d cursor=%q err=%v", n, cursor, err)
		}
		n, next, err := fetch(ctx, "token", "alice/project", cursor)
		if err != nil || n != 1 || next != "" {
			t.Fatalf("second=%d next=%q err=%v", n, next, err)
		}
		if _, _, err := fetch(ctx, "other", "alice/project", ""); err == nil {
			t.Fatal("private data accessible")
		}
		if _, _, err := fetch(ctx, "invalid", "alice/project", ""); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
}

func TestRepoCommentsSinceOrderAndPaging(t *testing.T) {
	s := New("client", "secret")
	s.access["token"] = "alice"
	s.access["other"] = "bob"
	id := s.AddRepo("alice", "project", true)
	s.Install("alice", 1, id)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	actor := IssueActor{ID: 1, Login: "alice", Type: "User"}
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	s.SetComments(id, 4, CommentFixture{ID: 1, User: actor, Body: "a", HTMLURL: "url", CreatedAt: at(1), UpdatedAt: at(3)}, CommentFixture{ID: 2, User: actor, Body: "b", HTMLURL: "url", CreatedAt: at(1), UpdatedAt: at(1)})
	s.SetComments(id, 7, CommentFixture{ID: 3, User: actor, Body: "pr", HTMLURL: "url", CreatedAt: at(2), UpdatedAt: at(2)})
	old := make([]CommentFixture, 101)
	for i := range old {
		old[i] = CommentFixture{ID: int64(100 + i), User: actor, Body: "old", HTMLURL: "url", CreatedAt: base, UpdatedAt: base.Add(-time.Hour)}
	}
	s.SetComments(id, 9, old...)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	c := githubapp.New("", "", "", server.URL, "")
	ctx := context.Background()
	got, next, err := c.FetchRepoCommentsCursor(ctx, "token", "alice/project", "", base)
	if err != nil || next != "" || len(got) != 3 {
		t.Fatalf("got=%v next=%q err=%v", got, next, err)
	}
	for i, want := range []struct{ id, number int64 }{{2, 4}, {3, 7}, {1, 4}} {
		if got[i].ID != want.id || got[i].IssueNumber != want.number {
			t.Fatalf("item %d = id %d issue %d, want %v", i, got[i].ID, got[i].IssueNumber, want)
		}
	}
	all, cursor, err := c.FetchRepoCommentsCursor(ctx, "token", "alice/project", "", base.Add(-48*time.Hour))
	if err != nil || len(all) != 100 || cursor == "" {
		t.Fatalf("first=%d cursor=%q err=%v", len(all), cursor, err)
	}
	rest, next, err := c.FetchRepoCommentsCursor(ctx, "token", "alice/project", cursor, time.Time{})
	if err != nil || len(rest) != 4 || next != "" {
		t.Fatalf("second=%d next=%q err=%v", len(rest), next, err)
	}
	if _, _, err := c.FetchRepoCommentsCursor(ctx, "other", "alice/project", "", base); err == nil {
		t.Fatal("private comments accessible")
	}
}
