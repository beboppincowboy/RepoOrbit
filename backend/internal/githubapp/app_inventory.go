package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AppInventory returns one complete app-wide installation-to-repository membership inventory.
// Any uncertainty returns an error and no inventory; callers must not treat an error as absence.
func (c *Client) AppInventory(ctx context.Context, key *rsa.PrivateKey) (map[int64][]int64, error) {
	jwt, err := c.appJWT(key)
	if err != nil {
		return nil, err
	}
	type installation struct {
		ID        int64           `json:"id"`
		Suspended json.RawMessage `json:"suspended_at"`
	}
	result := map[int64][]int64{}
	for p := 1; ; p++ {
		var installs []installation
		next, err := c.inventoryRequest(ctx, http.MethodGet, jwt, fmt.Sprintf("/app/installations?per_page=100&page=%d", p), &installs)
		if err != nil {
			return nil, err
		}
		if installs == nil || (next && len(installs) == 0) {
			return nil, fmt.Errorf("github app: invalid installations")
		}
		for _, in := range installs {
			if in.ID <= 0 || string(in.Suspended) != "null" {
				return nil, fmt.Errorf("github app: invalid or suspended installation")
			}
			if _, ok := result[in.ID]; ok {
				return nil, fmt.Errorf("github app: duplicate installation")
			}
			token, err := c.requestInstallationToken(ctx, jwt, in.ID)
			if err != nil {
				return nil, err
			}
			ids := []int64{}
			seen := map[int64]bool{}
			total := -1
			for rp := 1; ; rp++ {
				var out struct {
					Total        *int `json:"total_count"`
					Repositories []struct {
						ID int64 `json:"id"`
					} `json:"repositories"`
				}
				more, e := c.inventoryRequest(ctx, http.MethodGet, token.Value, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", rp), &out)
				if e != nil {
					return nil, e
				}
				if out.Total == nil || *out.Total < 0 || out.Repositories == nil || (more && len(out.Repositories) == 0) || (total >= 0 && total != *out.Total) {
					return nil, fmt.Errorf("github app: invalid repository listing")
				}
				total = *out.Total
				for _, repo := range out.Repositories {
					if repo.ID <= 0 || seen[repo.ID] {
						return nil, fmt.Errorf("github app: invalid repository membership")
					}
					seen[repo.ID] = true
					ids = append(ids, repo.ID)
				}
				if !more {
					break
				}
				if rp >= 10000 {
					return nil, fmt.Errorf("github app: too many pages")
				}
			}
			if len(ids) != total {
				return nil, fmt.Errorf("github app: incomplete repository listing")
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			result[in.ID] = ids
		}
		if !next {
			break
		}
		if p >= 10000 {
			return nil, fmt.Errorf("github app: too many pages")
		}
	}
	return result, nil
}

func (c *Client) appJWT(key *rsa.PrivateKey) (string, error) {
	if key == nil || c.OAuth.ClientID == "" {
		return "", fmt.Errorf("github app: missing credentials")
	}
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": c.OAuth.ClientID})
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github app: sign JWT")
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

type installationToken struct {
	Value     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *Client) requestInstallationToken(ctx context.Context, jwt string, id int64) (installationToken, error) {
	var token installationToken
	_, err := c.inventoryRequest(ctx, http.MethodPost, jwt, fmt.Sprintf("/app/installations/%d/access_tokens", id), &token)
	if err == nil && token.Value == "" {
		err = fmt.Errorf("github app: invalid installation token")
	}
	return token, err
}

func (c *Client) inventoryRequest(ctx context.Context, method, token, path string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.APIURL+path, nil)
	if err != nil {
		return false, fmt.Errorf("github app: invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("github app: request failed: %w", err) // keeps *url.Error matchable for callers that defer on transport failures
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, apiError(resp)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if decoder.Decode(out) != nil {
		return false, fmt.Errorf("github app: invalid response")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return false, fmt.Errorf("github app: invalid response")
	}
	link := resp.Header.Get("Link")
	if link == "" {
		return false, nil
	}
	current, _ := url.Parse(path)
	page, _ := strconv.Atoi(current.Query().Get("page"))
	next := false
	for _, part := range strings.Split(link, ",") {
		pieces := strings.Split(strings.TrimSpace(part), ";")
		if len(pieces) < 2 {
			return false, fmt.Errorf("github app: invalid pagination")
		}
		relNext := false
		for _, v := range pieces[1:] {
			if strings.TrimSpace(v) == `rel="next"` {
				relNext = true
			}
		}
		if !relNext {
			continue
		}
		if next || page == 0 {
			return false, fmt.Errorf("github app: invalid pagination")
		}
		raw := strings.TrimSpace(pieces[0])
		if len(raw) < 3 || raw[0] != '<' || raw[len(raw)-1] != '>' {
			return false, fmt.Errorf("github app: invalid pagination")
		}
		u, e := url.Parse(raw[1 : len(raw)-1])
		if e != nil || u.Path != current.Path || u.Query().Get("page") != strconv.Itoa(page+1) {
			return false, fmt.Errorf("github app: invalid pagination")
		}
		next = true
	}
	return next, nil
}

// RepositoryInstallation resolves the app grant for one repository: its installation ID, or 0 for none.
func (c *Client) RepositoryInstallation(ctx context.Context, key *rsa.PrivateKey, fullName string) (int64, error) {
	id, _, err := c.RepositoryInstallationGrant(ctx, key, fullName)
	return id, err
}

// RepositoryInstallationGrant is RepositoryInstallation plus whether the installation holds Issues write permission
// (false with no installation), read from the same response.
func (c *Client) RepositoryInstallationGrant(ctx context.Context, key *rsa.PrivateKey, fullName string) (id int64, issuesWrite bool, err error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || owner == "." || owner == ".." || name == "." || name == ".." {
		return 0, false, fmt.Errorf("github app: invalid repository name")
	}
	jwt, err := c.appJWT(key)
	if err != nil {
		return 0, false, err
	}
	var installation struct {
		ID          int64             `json:"id"`
		Suspended   json.RawMessage   `json:"suspended_at"`
		Permissions map[string]string `json:"permissions"`
	}
	_, err = c.inventoryRequest(ctx, http.MethodGet, jwt, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/installation", &installation)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && api.NotFound() {
			return 0, false, nil
		}
		return 0, false, err
	}
	if installation.ID <= 0 || string(installation.Suspended) != "null" {
		return 0, false, fmt.Errorf("github app: invalid or suspended repository installation")
	}
	return installation.ID, installation.Permissions["issues"] == "write", nil
}
