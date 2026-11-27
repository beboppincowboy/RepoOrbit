package ingestion

import (
	"reflect"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

func TestOpenIssueProjectionArgs(t *testing.T) {
	const repoID int64 = 42
	createdAt := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	issue := githubapp.Issue{
		ID:        101,
		Number:    7,
		Title:     "Track a follow-up",
		State:     "open",
		HTMLURL:   "https://github.com/example/project/issues/7",
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		User:      &githubapp.IssueUser{ID: 21, Login: "author", Type: "User"},
		Labels: []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}{{ID: 31, Name: "bug"}},
		Assignees: []githubapp.IssueUser{{ID: 22, Login: "maintainer", Type: "User"}},
	}

	args, err := issueProjectionArgs(repoID, issue, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		int64(101), repoID, int64(7), "Track a follow-up", "open", "https://github.com/example/project/issues/7",
		createdAt, updatedAt, (*time.Time)(nil), int64(21), "author", "User", false,
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("issue args = %#v, want %#v", args, want)
	}

	label := issue.Labels[0]
	labelArgs, err := issueLabelProjectionArgs(issue.ID, label.ID, label.Name)
	if err != nil {
		t.Fatal(err)
	}
	wantLabel := []any{int64(101), int64(31), "bug"}
	if !reflect.DeepEqual(labelArgs, wantLabel) {
		t.Errorf("label args = %#v, want %#v", labelArgs, wantLabel)
	}

	assigneeArgs, err := issueAssigneeProjectionArgs(issue.ID, issue.Assignees[0])
	if err != nil {
		t.Fatal(err)
	}
	wantAssignee := []any{int64(101), int64(22), "maintainer", "User"}
	if !reflect.DeepEqual(assigneeArgs, wantAssignee) {
		t.Errorf("assignee args = %#v, want %#v", assigneeArgs, wantAssignee)
	}
}

func TestClosedIssueProjectionArgs(t *testing.T) {
	closedAt := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	issue := githubapp.Issue{
		ID:        102,
		Number:    8,
		Title:     "Resolve the follow-up",
		State:     "closed",
		HTMLURL:   "https://github.com/example/project/issues/8",
		CreatedAt: closedAt.Add(-time.Hour),
		UpdatedAt: closedAt,
		ClosedAt:  &closedAt,
		User:      &githubapp.IssueUser{ID: 23, Login: "contributor", Type: "User"},
	}
	args, err := issueProjectionArgs(42, issue, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		int64(102), int64(42), int64(8), "Resolve the follow-up", "closed", "https://github.com/example/project/issues/8",
		closedAt.Add(-time.Hour), closedAt, &closedAt, int64(23), "contributor", "User", true,
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("issue args = %#v, want %#v", args, want)
	}
}

func TestIssueProjectionArgsWithDeletedAuthor(t *testing.T) {
	args, err := issueProjectionArgs(42, githubapp.Issue{ID: 103, State: "open"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 13 {
		t.Fatalf("issue args = %#v, want 13 arguments", args)
	}
	want := []any{nil, nil, nil}
	if !reflect.DeepEqual(args[9:12], want) {
		t.Errorf("author args = %#v, want %#v", args[9:12], want)
	}
}

func TestIssueProjectionArgsWithInvalidAuthor(t *testing.T) {
	issue := githubapp.Issue{User: &githubapp.IssueUser{Login: "author", Type: "User"}}
	if _, err := issueProjectionArgs(42, issue, false); err == nil || err.Error() != "invalid activity actor" {
		t.Errorf("error = %v, want invalid activity actor", err)
	}
}

func TestIssueChildProjectionArgsWithInvalidID(t *testing.T) {
	for _, id := range []int64{0, -1} {
		if _, err := issueLabelProjectionArgs(101, id, "bug"); err == nil || err.Error() != "invalid issue label" {
			t.Errorf("label ID %d error = %v, want invalid issue label", id, err)
		}
		assignee := githubapp.IssueUser{ID: id, Login: "maintainer", Type: "User"}
		if _, err := issueAssigneeProjectionArgs(101, assignee); err == nil || err.Error() != "invalid issue assignee" {
			t.Errorf("assignee ID %d error = %v, want invalid issue assignee", id, err)
		}
	}
}
