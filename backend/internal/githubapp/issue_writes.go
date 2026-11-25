package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// NewIssue is the content of an issue to create.
type NewIssue struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

// ErrNotSent wraps an issue-call error raised before the request left (an invalid repository name, a body that does not
// encode, a request that cannot be built): GitHub received nothing, so the call is definitely not applied.
var ErrNotSent = errors.New("github: request not sent")

// repoPath is /repos/{owner}/{name} for a full name, with both parts escaped.
func repoPath(fullName string) (string, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: invalid repository", ErrNotSent)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// send makes one API request with a JSON body (nil for none) and decodes a 2xx response into out (nil to discard).
// Errors before the request is handed to the transport wrap ErrNotSent.
func (c *Client) send(ctx context.Context, token, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrNotSent, err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIURL+path, reader)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotSent, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			return fmt.Errorf("github %s %s: bad response", method, path)
		}
	}
	return nil
}

func (c *Client) issueCall(ctx context.Context, token, method, fullName, rest string, body, out any) error {
	base, err := repoPath(fullName)
	if err != nil {
		return err
	}
	return c.send(ctx, token, method, base+rest, body, out)
}

// CreateIssue opens an issue (POST, not idempotent: never resend after an ambiguous error). GitHub may silently
// drop labels and assignees, so callers must inspect the returned issue. Errors, here and in every issue write below:
// an error wrapping ErrNotSent was raised before anything was sent (nothing applied); a *APIError with a 4xx Status is
// a definite rejection (nothing applied); a *APIError with any other status, a transport error or an unreadable or
// invalid response is ambiguous (the write may have been applied).
func (c *Client) CreateIssue(ctx context.Context, token, fullName string, in NewIssue) (Issue, error) {
	var out Issue
	if err := c.issueCall(ctx, token, http.MethodPost, fullName, "/issues", in, &out); err != nil {
		return Issue{}, err
	}
	if out.ID <= 0 || out.Number <= 0 {
		return Issue{}, fmt.Errorf("github: invalid created issue")
	}
	return out, nil
}

// FetchIssue reads one issue (or pull request: check PullRequest) by number. A missing issue, or a repository the
// token cannot read, is a *APIError whose NotFound() is true.
func (c *Client) FetchIssue(ctx context.Context, token, fullName string, number int64) (Issue, error) {
	var out Issue
	if err := c.issueCall(ctx, token, http.MethodGet, fullName, fmt.Sprintf("/issues/%d", number), nil, &out); err != nil {
		return Issue{}, err
	}
	if out.ID <= 0 || out.Number != number {
		return Issue{}, fmt.Errorf("github: invalid issue")
	}
	return out, nil
}

// AddLabels adds labels without touching the others on the issue; read the issue back to see which landed.
func (c *Client) AddLabels(ctx context.Context, token, fullName string, number int64, labels []string) error {
	return c.issueCall(ctx, token, http.MethodPost, fullName, fmt.Sprintf("/issues/%d/labels", number), map[string][]string{"labels": labels}, nil)
}

// RemoveLabel removes one label. Any 404 counts as success (nil): a label that is not on the issue, but also a missing
// issue or an unreadable repository, so callers must read the issue back to confirm.
func (c *Client) RemoveLabel(ctx context.Context, token, fullName string, number int64, label string) error {
	err := c.issueCall(ctx, token, http.MethodDelete, fullName, fmt.Sprintf("/issues/%d/labels/%s", number, url.PathEscape(label)), nil, nil)
	var api *APIError
	if errors.As(err, &api) && api.NotFound() {
		return nil
	}
	return err
}

// AddAssignees adds assignees without touching the others. GitHub silently ignores logins it cannot assign.
func (c *Client) AddAssignees(ctx context.Context, token, fullName string, number int64, logins []string) error {
	return c.issueCall(ctx, token, http.MethodPost, fullName, fmt.Sprintf("/issues/%d/assignees", number), map[string][]string{"assignees": logins}, nil)
}

// RemoveAssignees removes assignees without touching the others; a login that is not assigned is ignored.
func (c *Client) RemoveAssignees(ctx context.Context, token, fullName string, number int64, logins []string) error {
	return c.issueCall(ctx, token, http.MethodDelete, fullName, fmt.Sprintf("/issues/%d/assignees", number), map[string][]string{"assignees": logins}, nil)
}

// SetIssueState closes or reopens an issue: state is "open" or "closed". Only the state field is sent (no
// state_reason).
func (c *Client) SetIssueState(ctx context.Context, token, fullName string, number int64, state string) error {
	return c.issueCall(ctx, token, http.MethodPatch, fullName, fmt.Sprintf("/issues/%d", number), map[string]string{"state": state}, nil)
}

// LabelExists reports whether the repository has a label of this name; GitHub matches label names case-insensitively.
func (c *Client) LabelExists(ctx context.Context, token, fullName, name string) (bool, error) {
	err := c.issueCall(ctx, token, http.MethodGet, fullName, "/labels/"+url.PathEscape(name), nil, nil)
	var api *APIError
	if errors.As(err, &api) && api.NotFound() {
		return false, nil
	}
	return err == nil, err
}

// CreateLabel creates a repository label with a hex color (no "#"). A 422 usually means a label of that name already
// exists, so callers read the labels back (LabelExists) rather than trust either answer.
func (c *Client) CreateLabel(ctx context.Context, token, fullName, name, color string) error {
	return c.issueCall(ctx, token, http.MethodPost, fullName, "/labels", map[string]string{"name": name, "color": color}, nil)
}

// ListAssignees returns the logins that can be assigned issues in the repository, in GitHub's order.
// ponytail: first page only (100 users); page through Link headers if suggestions must cover larger teams.
func (c *Client) ListAssignees(ctx context.Context, token, fullName string) ([]string, error) {
	var users []IssueUser
	if err := c.issueCall(ctx, token, http.MethodGet, fullName, "/assignees?per_page=100", nil, &users); err != nil {
		return nil, err
	}
	logins := make([]string, 0, len(users))
	for _, u := range users {
		if u.Login == "" {
			return nil, fmt.Errorf("github: invalid assignee listing")
		}
		logins = append(logins, u.Login)
	}
	return logins, nil
}

// IsAssignable reports whether login can be assigned issues in the repository (GitHub answers 204 or 404).
func (c *Client) IsAssignable(ctx context.Context, token, fullName, login string) (bool, error) {
	err := c.issueCall(ctx, token, http.MethodGet, fullName, "/assignees/"+url.PathEscape(login), nil, nil)
	var api *APIError
	if errors.As(err, &api) && api.NotFound() {
		return false, nil
	}
	return err == nil, err
}
