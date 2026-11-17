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
