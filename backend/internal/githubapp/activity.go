package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type PullRequest struct {
	ID        int64      `json:"id"`
	Number    int64      `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedAt  *time.Time `json:"merged_at"`
	User      *IssueUser `json:"user"`
}

type Review struct {
	ID          int64      `json:"id"`
	User        *IssueUser `json:"user"`
	State       string     `json:"state"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	SubmittedAt *time.Time `json:"submitted_at"`
}

type IssueComment struct {
	ID          int64      `json:"id"`
	User        *IssueUser `json:"user"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	IssueURL    string     `json:"issue_url"`
	IssueNumber int64      `json:"-"` // parsed from IssueURL; pull request conversation comments carry the PR number
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// FetchRepoCommentsCursor pages through a repository's issue and pull request conversation comments in ascending
// update order. An empty cursor starts a scan of comments updated at or after since; later pages follow GitHub's
// Link header, which carries the since, sort and direction parameters forward.
func (c *Client) FetchRepoCommentsCursor(ctx context.Context, token, fullName, cursor string, since time.Time) ([]IssueComment, string, error) {
	if cursor == "" {
		cursor = "sort=updated&direction=asc&per_page=100&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	}
	var items []IssueComment
	next, err := c.activityPage(ctx, token, fullName, "/issues/comments", cursor, false, &items)
	if err != nil {
		return nil, "", err
	}
	for i, v := range items {
		_, tail, _ := strings.Cut(v.IssueURL, "/issues/")
		number, err := strconv.ParseInt(tail, 10, 64)
		if v.ID <= 0 || !validActivityUser(v.User) || v.HTMLURL == "" || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() || err != nil || number <= 0 {
			return nil, "", fmt.Errorf("github: invalid comment listing")
		}
		items[i].IssueNumber = number
	}
	return items, next, nil
}

func (c *Client) FetchPullRequestsCursor(ctx context.Context, token, fullName, cursor string) ([]PullRequest, string, error) {
	var items []PullRequest
	next, err := c.activityPage(ctx, token, fullName, "/pulls", cursor, true, &items)
	if err != nil {
		return nil, "", err
	}
	for _, v := range items {
		if v.ID <= 0 || v.Number <= 0 || v.Title == "" || (v.State != "open" && v.State != "closed") || v.HTMLURL == "" || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() || !validActivityUser(v.User) {
			return nil, "", fmt.Errorf("github: invalid pull request listing")
		}
	}
	return items, next, nil
}

func (c *Client) FetchReviewsCursor(ctx context.Context, token, fullName string, number int64, cursor string) ([]Review, string, error) {
	if number <= 0 {
		return nil, "", fmt.Errorf("github: invalid pull request number")
	}
	var items []Review
	next, err := c.activityPage(ctx, token, fullName, "/pulls/"+strconv.FormatInt(number, 10)+"/reviews", cursor, false, &items)
	if err != nil {
		return nil, "", err
	}
	for _, v := range items {
		if v.ID <= 0 || !validActivityUser(v.User) || v.State == "" || v.HTMLURL == "" || (v.SubmittedAt == nil && !strings.EqualFold(v.State, "PENDING")) || (v.SubmittedAt != nil && v.SubmittedAt.IsZero()) {
			return nil, "", fmt.Errorf("github: invalid review listing")
		}
	}
	return items, next, nil
}

func validActivityUser(u *IssueUser) bool {
	return u == nil || (u.ID > 0 && u.Login != "" && u.Type != "")
}

// activityPage stores only the query as a cursor, retaining repository identity across renames.
func (c *Client) activityPage(ctx context.Context, token, fullName, suffix, cursor string, all bool, out any) (string, error) {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("github: invalid repository")
	}
	endpoint, err := url.Parse(c.APIURL + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + suffix)
	if err != nil {
		return "", fmt.Errorf("github: invalid activity endpoint")
	}
	if cursor == "" {
		cursor = "per_page=100"
		if all {
			cursor = "state=all&per_page=100"
		}
	}
	validQuery := func(q string) bool {
		p, e := url.ParseQuery(q)
		return e == nil && q != "" && !strings.ContainsAny(q, "#\r\n") && (!all || p.Get("state") == "all")
	}
	if !validQuery(cursor) {
		return "", fmt.Errorf("github: invalid activity cursor")
	}
	requestURL := *endpoint
	requestURL.RawQuery = cursor
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("github: invalid activity request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: activity request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	// Decode through RawMessage so JSON null and trailing or truncated content cannot claim a complete page.
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 || len(strings.TrimSpace(string(data))) == 0 || strings.TrimSpace(string(data)) == "null" || json.Unmarshal(data, out) != nil {
		return "", fmt.Errorf("github: invalid activity listing")
	}
	next := ""
	for _, link := range strings.Split(resp.Header.Get("Link"), ",") {
		fields := strings.Split(link, ";")
		if strings.TrimSpace(link) == "" {
			continue
		}
		if len(fields) < 2 {
			return "", fmt.Errorf("github: invalid activity pagination")
		}
		isNext, hasRel := false, false
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "rel") {
				hasRel = true
				for _, relation := range strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)) {
					if relation == "next" {
						isNext = true
					}
				}
			}
		}
		if !hasRel {
			return "", fmt.Errorf("github: invalid activity pagination")
		}
		if !isNext {
			continue
		}
		raw := strings.TrimSpace(fields[0])
		if len(raw) < 2 || raw[0] != '<' || raw[len(raw)-1] != '>' {
			return "", fmt.Errorf("github: invalid activity pagination")
		}
		target, err := url.Parse(raw[1 : len(raw)-1])
		if err != nil {
			return "", fmt.Errorf("github: invalid activity pagination")
		}
		target = requestURL.ResolveReference(target)
		if target.Scheme != endpoint.Scheme || target.Host != endpoint.Host || !repositoryPagePath(target.Path, endpoint.Path, fullName, suffix) || target.User != nil || target.Fragment != "" || !validQuery(target.RawQuery) || next != "" {
			return "", fmt.Errorf("github: unsafe activity pagination")
		}
		next = target.RawQuery
	}
	return next, nil
}

// Release is source evidence from the repository releases endpoint.
type Release struct {
	ID          int64      `json:"id"`
	Author      *IssueUser `json:"author"`
	Name        string     `json:"name"`
	TagName     string     `json:"tag_name"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
}

type EventIssue struct {
	ID     int64 `json:"id"`
	Number int64 `json:"number"`
}
type EventLabel struct {
	Name string `json:"name"`
}

// IssueEvent is source evidence from the repository-wide issue events endpoint.
type IssueEvent struct {
	ID        int64       `json:"id"`
	Actor     *IssueUser  `json:"actor"`
	Event     string      `json:"event"`
	CreatedAt time.Time   `json:"created_at"`
	URL       string      `json:"url"`
	Issue     *EventIssue `json:"issue"`
	Assignee  *IssueUser  `json:"assignee"`
	Label     *EventLabel `json:"label"`
}

func (c *Client) FetchReleasesCursor(ctx context.Context, token, fullName, cursor string) ([]Release, string, error) {
	var raw []json.RawMessage
	next, err := c.activityPage(ctx, token, fullName, "/releases", cursor, false, &raw)
	if err != nil {
		return nil, "", err
	}
	items := make([]Release, 0, len(raw))
	for _, data := range raw {
		var v Release
		var required struct {
			Draft      *bool `json:"draft"`
			Prerelease *bool `json:"prerelease"`
		}
		if json.Unmarshal(data, &v) != nil || json.Unmarshal(data, &required) != nil || required.Draft == nil || required.Prerelease == nil || v.ID <= 0 || v.TagName == "" || v.HTMLURL == "" || v.CreatedAt.IsZero() || (v.PublishedAt != nil && v.PublishedAt.IsZero()) || (!v.Draft && v.PublishedAt == nil) || !validActivityUser(v.Author) {
			return nil, "", fmt.Errorf("github: invalid release listing")
		}
		items = append(items, v)
	}
	return items, next, nil
}

func (c *Client) FetchIssueEventsCursor(ctx context.Context, token, fullName, cursor string) ([]IssueEvent, string, error) {
	var items []IssueEvent
	next, err := c.activityPage(ctx, token, fullName, "/issues/events", cursor, false, &items)
	if err != nil {
		return nil, "", err
	}
	for _, v := range items {
		if v.ID <= 0 || v.Event == "" || v.URL == "" || v.CreatedAt.IsZero() || v.Issue == nil || v.Issue.ID <= 0 || v.Issue.Number <= 0 || !validActivityUser(v.Actor) || !validActivityUser(v.Assignee) || (v.Label != nil && v.Label.Name == "") {
			return nil, "", fmt.Errorf("github: invalid issue event listing")
		}
	}
	return items, next, nil
}
