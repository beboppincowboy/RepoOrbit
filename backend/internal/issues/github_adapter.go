package issues

import (
	"context"
	"errors"
	"strings"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/issues/application"
	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// githubGateway implements application.IssueGateway on the GitHub client. Every failure comes back as an
// *application.RemoteError wrapping the client error; an Issue's Payload is the githubapp.Issue.
type githubGateway struct{ c *githubapp.Client }

// remoteError maps a GitHub client error onto a domain error kind, keeping the original reachable through errors.As.
func remoteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, githubapp.ErrNotSent) {
		return &application.RemoteError{Kind: domain.NotSent, Err: err}
	}
	var api *githubapp.APIError
	switch {
	case !errors.As(err, &api):
		return &application.RemoteError{Kind: domain.OtherError, Err: err}
	case api.Unauthorized():
		return &application.RemoteError{Kind: domain.Unauthorized, Status: api.Status, Err: err}
	case api.NotFound():
		return &application.RemoteError{Kind: domain.NotFound, Status: api.Status, Err: err}
	}
	return &application.RemoteError{Kind: domain.HTTPError, Status: api.Status, Err: err}
}

// classify maps a GitHub client error onto domain.Classify's state and reason, exactly as the gateway's errors classify.
func classify(err error) (string, string) {
	var re *application.RemoteError
	if errors.As(remoteError(err), &re) {
		return domain.Classify(re.Kind, re.Status)
	}
	return domain.Classify(domain.OtherError, 0)
}

func viewIssue(issue githubapp.Issue) *issueView {
	labels := make([]string, len(issue.Labels))
	for i, l := range issue.Labels {
		labels[i] = l.Name
	}
	assignees := make([]string, len(issue.Assignees))
	for i, a := range issue.Assignees {
		assignees[i] = a.Login
	}
	return domain.NewIssueView(issue.Number, issue.HTMLURL, issue.Title, issue.State, labels, assignees)
}

func (g githubGateway) issue(fullName string, issue githubapp.Issue) application.Issue {
	var author int64
	if issue.User != nil {
		author = issue.User.ID
	}
	return application.Issue{View: viewIssue(issue), AuthorID: author, Body: issue.Body, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
		PullRequest: issue.PullRequest != nil, InRepository: strings.EqualFold(issue.RepoURL, g.c.APIURL+"/repos/"+fullName), Payload: issue}
}

func (g githubGateway) FetchIssue(ctx context.Context, token, fullName string, number int64) (application.Issue, error) {
	issue, err := g.c.FetchIssue(ctx, token, fullName, number)
	if err != nil {
		return application.Issue{}, remoteError(err)
	}
	return g.issue(fullName, issue), nil
}

func (g githubGateway) CreateIssue(ctx context.Context, token, fullName string, d domain.CreateDraft) (application.Issue, error) {
	issue, err := g.c.CreateIssue(ctx, token, fullName, githubapp.NewIssue{Title: d.Title, Body: d.Body, Labels: d.Labels, Assignees: d.Assignees})
	if err != nil {
		return application.Issue{}, remoteError(err)
	}
	return g.issue(fullName, issue), nil
}

func (g githubGateway) SetIssueState(ctx context.Context, token, fullName string, number int64, state string) error {
	return remoteError(g.c.SetIssueState(ctx, token, fullName, number, state))
}

func (g githubGateway) AddLabels(ctx context.Context, token, fullName string, number int64, names []string) error {
	return remoteError(g.c.AddLabels(ctx, token, fullName, number, names))
}

func (g githubGateway) RemoveLabel(ctx context.Context, token, fullName string, number int64, name string) error {
	return remoteError(g.c.RemoveLabel(ctx, token, fullName, number, name))
}

func (g githubGateway) AddAssignees(ctx context.Context, token, fullName string, number int64, logins []string) error {
	return remoteError(g.c.AddAssignees(ctx, token, fullName, number, logins))
}

func (g githubGateway) RemoveAssignees(ctx context.Context, token, fullName string, number int64, logins []string) error {
	return remoteError(g.c.RemoveAssignees(ctx, token, fullName, number, logins))
}

func (g githubGateway) IsAssignable(ctx context.Context, token, fullName, login string) (bool, error) {
	ok, err := g.c.IsAssignable(ctx, token, fullName, login)
	return ok, remoteError(err)
}

func (g githubGateway) CreateLabel(ctx context.Context, token, fullName, name, color string) error {
	return remoteError(g.c.CreateLabel(ctx, token, fullName, name, color))
}

func (g githubGateway) LabelExists(ctx context.Context, token, fullName, name string) (bool, error) {
	ok, err := g.c.LabelExists(ctx, token, fullName, name)
	return ok, remoteError(err)
}
