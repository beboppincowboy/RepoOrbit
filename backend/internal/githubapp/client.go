// Package githubapp talks to GitHub (or a fake of it) on behalf of the GitHub App.
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

	"golang.org/x/oauth2"
)

// Client calls the GitHub API. Every method that takes a token makes a network call (10 s timeout) and returns
// *APIError for non-2xx responses.
type Client struct {
	OAuth  oauth2.Config
	APIURL string
	HTTP   *http.Client
}

func New(clientID, clientSecret, githubURL, apiURL, redirectURL string) *Client {
	return &Client{
		OAuth: oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
			Endpoint: oauth2.Endpoint{AuthURL: githubURL + "/login/oauth/authorize", TokenURL: githubURL + "/login/oauth/access_token", AuthStyle: oauth2.AuthStyleInParams},
		},
		APIURL: apiURL,
		HTTP:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Tokens is a user-to-server token pair as returned by GitHub. Zero times mean "does not expire".
type Tokens struct {
	AccessToken, RefreshToken         string
	AccessExpiresAt, RefreshExpiresAt time.Time
}

// withHTTPClient makes the oauth2 library use c.HTTP.
func (c *Client) withHTTPClient(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, c.HTTP)
}

// AuthURL returns the GitHub authorization URL using PKCE. No scopes are requested; the app's permissions apply.
func (c *Client) AuthURL(state, verifier string) string {
	return c.OAuth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// Exchange trades the OAuth callback code for tokens. Network call.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (Tokens, error) {
	t, err := c.OAuth.Exchange(c.withHTTPClient(ctx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Tokens{}, err
	}
	return toTokens(t, time.Now()), nil
}

// Refresh exchanges a refresh token. GitHub rotates refresh tokens, so the result must be stored.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	t, err := c.OAuth.TokenSource(c.withHTTPClient(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return Tokens{}, err
	}
	return toTokens(t, time.Now()), nil
}

func toTokens(t *oauth2.Token, now time.Time) Tokens {
	out := Tokens{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, AccessExpiresAt: t.Expiry}
	if s := fmt.Sprint(t.Extra("refresh_token_expires_in")); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			out.RefreshExpiresAt = now.Add(time.Duration(secs) * time.Second)
		}
	}
	return out
}

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func (c *Client) FetchUser(ctx context.Context, accessToken string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+"/user", nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return User{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return User{}, fmt.Errorf("github user lookup: status %d", resp.StatusCode)
	}
	var u User
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil || u.ID == 0 || u.Login == "" {
		return User{}, fmt.Errorf("github user lookup: bad response")
	}
	return u, nil
}

// APIError is a non-2xx GitHub response. RetryAt is non-zero only for rate limits: GitHub's Retry-After or
// reset time, or one minute from now when the response carried neither.
type APIError struct {
	Status  int
	RetryAt time.Time
}

func (e *APIError) Error() string { return fmt.Sprintf("github: status %d", e.Status) }

func (e *APIError) Unauthorized() bool { return e.Status == http.StatusUnauthorized }
func (e *APIError) NotFound() bool     { return e.Status == http.StatusNotFound }
func (e *APIError) RateLimited() bool  { return !e.RetryAt.IsZero() }

func apiError(resp *http.Response) *APIError {
	e := &APIError{Status: resp.StatusCode}
	h := resp.Header
	if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusForbidden && (h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != "")) {
		e.RetryAt = time.Now().Add(time.Minute) // GitHub gave no usable time
		if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil && secs > 0 {
			e.RetryAt = time.Now().Add(time.Duration(secs) * time.Second)
		} else if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
			e.RetryAt = time.Unix(reset, 0)
		}
	}
	return e
}

type Repo struct {
	ID              int64  `json:"id"`
	FullName        string `json:"full_name"`
	Description     string `json:"description"` // GitHub sends null for none
	Private         bool   `json:"private"`
	Archived        bool   `json:"archived"`
	HasPullRequests *bool  `json:"has_pull_requests"`
	HasIssues       bool   `json:"has_issues"`
	// Permissions are the requesting user's when a user token fetched the repository. All false otherwise (an
	// installation token, or a response without them), which reads the same as no access: use only with user tokens.
	Permissions struct {
		Admin    bool `json:"admin"`
		Maintain bool `json:"maintain"`
		Push     bool `json:"push"`
		Triage   bool `json:"triage"`
		Pull     bool `json:"pull"`
	} `json:"permissions"`
	Owner struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
}

// get decodes one JSON response into out and reports whether GitHub advertises another page.
func (c *Client) get(ctx context.Context, token, path string, out any) (hasNext bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, apiError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return false, fmt.Errorf("github %s: bad response", path)
	}
	return strings.Contains(resp.Header.Get("Link"), `rel="next"`), nil
}

// FetchOwnedRepos lists one page of repositories the user owns. With a user-to-server token GitHub returns
// only those the app can also reach (the intersection of user access and installations).
func (c *Client) FetchOwnedRepos(ctx context.Context, token string, page, perPage int) (repos []Repo, hasNext bool, err error) {
	var out []Repo
	next, err := c.get(ctx, token, fmt.Sprintf("/user/repos?affiliation=owner&sort=full_name&per_page=%d&page=%d", perPage, page), &out)
	if err == nil && out == nil {
		return nil, false, fmt.Errorf("github: invalid repository listing")
	}
	return out, next, err
}

// FetchInstallations lists one page of app installations the user can access.
func (c *Client) FetchInstallations(ctx context.Context, token string, page int) (installs []Installation, hasNext bool, err error) {
	var out struct {
		Installations []Installation `json:"installations"`
	}
	next, err := c.get(ctx, token, fmt.Sprintf("/user/installations?per_page=100&page=%d", page), &out)
	if err == nil && out.Installations == nil {
		return nil, false, fmt.Errorf("github: invalid installation listing")
	}
	return out.Installations, next, err
}

// FetchInstallationRepos lists one page of repositories the user can reach through an installation.
func (c *Client) FetchInstallationRepos(ctx context.Context, token string, installationID int64, page int) (repos []Repo, hasNext bool, err error) {
	var out struct {
		Repositories []Repo `json:"repositories"`
	}
	next, err := c.get(ctx, token, fmt.Sprintf("/user/installations/%d/repositories?per_page=100&page=%d", installationID, page), &out)
	if err == nil && out.Repositories == nil {
		return nil, false, fmt.Errorf("github: invalid installation repository listing")
	}
	return out.Repositories, next, err
}

// FetchRepo fetches one repository by ID; a 404 means the token cannot read it.
func (c *Client) FetchRepo(ctx context.Context, token string, id int64) (Repo, error) {
	var out Repo
	_, err := c.get(ctx, token, fmt.Sprintf("/repositories/%d", id), &out)
	return out, err
}

// FetchStarredRepos lists one page of repositories the user has starred.
func (c *Client) FetchStarredRepos(ctx context.Context, token string, page, perPage int) (repos []Repo, hasNext bool, err error) {
	var out []Repo
	next, err := c.get(ctx, token, fmt.Sprintf("/user/starred?per_page=%d&page=%d", perPage, page), &out)
	return out, next, err
}

// IssueUser identifies an issue author or assignee, including GitHub's bot classification.
type IssueUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Issue is an issue as GitHub's issues endpoints return it. PullRequest is non-nil
// when the entry is a pull request. User is nil when GitHub sends no author.
type Issue struct {
	ID        int64      `json:"id"`
	Number    int64      `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	HTMLURL   string     `json:"html_url"`
	Body      string     `json:"body"`           // GitHub sends null for none
	RepoURL   string     `json:"repository_url"` // API URL of the repository the issue belongs to
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	User      *IssueUser `json:"user"`
	Labels    []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"labels"`
	Assignees   []IssueUser      `json:"assignees"`
	PullRequest *json.RawMessage `json:"pull_request"`
}

// FetchIssuesCursor follows GitHub's advertised next link after validating its origin and endpoint.
// The cursor contains only its query, so a verified repository rename retains the stable identity.
func (c *Client) FetchIssuesCursor(ctx context.Context, token, fullName, cursor string) ([]Issue, string, error) {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, "", fmt.Errorf("github: invalid repository")
	}
	endpoint, err := url.Parse(c.APIURL + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/issues")
	if err != nil {
		return nil, "", fmt.Errorf("github: invalid issue endpoint")
	}
	if cursor == "" {
		cursor = "state=all&per_page=100"
	}
	params, err := url.ParseQuery(cursor)
	if err != nil || strings.ContainsAny(cursor, "#\r\n") || params.Get("state") != "all" {
		return nil, "", fmt.Errorf("github: invalid issue cursor")
	}
	requestURL := *endpoint
	requestURL.RawQuery = cursor
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("github: invalid issue request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("github: issue request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", apiError(resp)
	}
	var items []Issue
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&items); err != nil || items == nil {
		return nil, "", fmt.Errorf("github: invalid issue listing")
	}
	next := ""
	for _, link := range strings.Split(resp.Header.Get("Link"), ",") {
		fields := strings.Split(link, ";")
		if strings.TrimSpace(link) == "" {
			continue
		}
		if len(fields) < 2 {
			return nil, "", fmt.Errorf("github: invalid issue pagination")
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
			return nil, "", fmt.Errorf("github: invalid issue pagination")
		}
		if !isNext {
			continue
		}
		raw := strings.TrimSpace(fields[0])
		if len(raw) < 2 || raw[0] != '<' || raw[len(raw)-1] != '>' {
			return nil, "", fmt.Errorf("github: invalid issue pagination")
		}
		target, err := url.Parse(raw[1 : len(raw)-1])
		if err != nil {
			return nil, "", fmt.Errorf("github: invalid issue pagination")
		}
		target = requestURL.ResolveReference(target)
		if target.Scheme != endpoint.Scheme || target.Host != endpoint.Host || !repositoryPagePath(target.Path, endpoint.Path, fullName, "/issues") || target.User != nil || target.Fragment != "" || target.RawQuery == "" || next != "" {
			return nil, "", fmt.Errorf("github: unsafe issue pagination")
		}
		params, err := url.ParseQuery(target.RawQuery)
		if err != nil || params.Get("state") != "all" {
			return nil, "", fmt.Errorf("github: invalid issue pagination")
		}
		next = target.RawQuery
	}
	return items, next, nil
}

// repositoryPagePath accepts GitHub's numeric alias for the same resource type.
// Callers retain only the query and always send it to their original endpoint.
func repositoryPagePath(target, endpoint, fullName, suffix string) bool {
	if target == endpoint {
		return true
	}
	base := strings.TrimSuffix(endpoint, "/repos/"+fullName+suffix)
	numeric, ok := strings.CutPrefix(target, base+"/repositories/")
	if !ok {
		return false
	}
	id, resource, ok := strings.Cut(numeric, "/")
	repoID, err := strconv.ParseInt(id, 10, 64)
	return ok && err == nil && repoID > 0 && strconv.FormatInt(repoID, 10) == id && "/"+resource == suffix
}
