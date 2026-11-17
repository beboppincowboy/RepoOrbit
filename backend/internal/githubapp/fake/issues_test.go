package fake

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

func TestIssuesPaginationAndPrivateAuthorization(t *testing.T) {
	s := New("client", "secret")
	s.access["token"] = "alice"
	id := s.AddRepo("alice", "project", true)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	issues := make([]IssueFixture, 101)
	for i := range issues {
		issues[i] = IssueFixture{ID: int64(i + 1), Number: int64(i + 1), Title: fmt.Sprintf("Issue %d", i+1), State: "open",
			HTMLURL: fmt.Sprintf("https://github.com/alice/project/issues/%d", i+1), CreatedAt: base, UpdatedAt: base,
			User: IssueActor{ID: 1, Login: "alice", Type: "User"}, Labels: []IssueLabel{{ID: 2, Name: "bug"}}, Assignees: []IssueActor{}, PullRequest: i == 0}
	}
	s.SetIssues(id, issues...)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	c := githubapp.New("", "", "", server.URL, "")
	ctx := context.Background()

	if _, _, err := c.FetchIssuesCursor(ctx, "token", "alice/project", ""); err == nil {
		t.Fatal("private issues were readable without an app installation")
	}
	s.Install("alice", 42, id)
	first, cursor, err := c.FetchIssuesCursor(ctx, "token", "alice/project", "")
	if err != nil || len(first) != 100 || cursor == "" {
		t.Fatalf("first page: len=%d cursor=%q err=%v", len(first), cursor, err)
	}
	if first[0].ID != 1 || first[0].User.Type != "User" || len(first[0].Labels) != 1 || first[0].PullRequest == nil {
		t.Fatalf("issue evidence lost: %+v", first[0])
	}
	second, next, err := c.FetchIssuesCursor(ctx, "token", "alice/project", cursor)
	if err != nil || len(second) != 1 || next != "" || second[0].ID != 101 {
		t.Fatalf("second page: issues=%+v cursor=%q err=%v", second, next, err)
	}

	s.access["other-token"] = "bob"
	if _, _, err := c.FetchIssuesCursor(ctx, "other-token", "alice/project", ""); err == nil {
		t.Fatal("private issues were readable without user access")
	}
	s.SetIssues(id, IssueFixture{ID: 999, Number: 999, Title: "Updated fixture", State: "closed"})
	updated, _, err := c.FetchIssuesCursor(ctx, "token", "alice/project", "")
	if err != nil || len(updated) != 1 || updated[0].ID != 999 {
		t.Fatalf("updated fixture not returned: issues=%+v err=%v", updated, err)
	}
}
