package ingestion

import (
	"errors"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

// issueProjectionArgs returns the activity_issues upsert arguments $1..$13 used by projectIssue.
func issueProjectionArgs(repoID int64, issue githubapp.Issue, sameSecond bool) ([]any, error) {
	actorID, actorLogin, actorType, err := evidenceActor(issue.User)
	if err != nil {
		return nil, err
	}
	return []any{
		issue.ID, repoID, issue.Number, issue.Title, issue.State, issue.HTMLURL,
		issue.CreatedAt, issue.UpdatedAt, issue.ClosedAt, actorID, actorLogin, actorType, sameSecond,
	}, nil
}

// issueLabelProjectionArgs and issueAssigneeProjectionArgs validate one child row. Call them only after the issue
// upsert wrote a row: a stale or tombstoned copy is ignored without validating its labels or assignees.
func issueLabelProjectionArgs(issueID, labelID int64, name string) ([]any, error) {
	if labelID <= 0 {
		return nil, errors.New("invalid issue label")
	}
	return []any{issueID, labelID, name}, nil
}

func issueAssigneeProjectionArgs(issueID int64, assignee githubapp.IssueUser) ([]any, error) {
	if assignee.ID <= 0 {
		return nil, errors.New("invalid issue assignee")
	}
	return []any{issueID, assignee.ID, assignee.Login, assignee.Type}, nil
}
