package githubapp

import (
	"context"
	"crypto/rsa"
	"fmt"
	"sync"
	"time"
)

// InstallationCredentials keeps short-lived worker credentials and cooldowns per installation.
type InstallationCredentials struct {
	client *Client
	key    *rsa.PrivateKey
	mu     sync.Mutex
	tokens map[int64]installationToken
	limits map[int64]time.Time
}

func NewInstallationCredentials(client *Client, key *rsa.PrivateKey) *InstallationCredentials {
	return &InstallationCredentials{client: client, key: key, tokens: make(map[int64]installationToken), limits: make(map[int64]time.Time)}
}

func (c *InstallationCredentials) AccessToken(ctx context.Context, id int64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if until := c.limits[id]; until.After(now) {
		return "", &APIError{Status: 429, RetryAt: until}
	}
	if token := c.tokens[id]; token.ExpiresAt.After(now.Add(time.Minute)) {
		return token.Value, nil
	}
	jwt, err := c.client.appJWT(c.key)
	if err != nil {
		return "", err
	}
	token, err := c.client.requestInstallationToken(ctx, jwt, id)
	if err != nil {
		return "", err
	}
	if !token.ExpiresAt.After(now) {
		return "", fmt.Errorf("github app: expired installation credential")
	}
	c.tokens[id] = token
	return token.Value, nil
}

// Observe records only errors from requests authenticated as this installation.
func (c *InstallationCredentials) Observe(id int64, token string, api *APIError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if api.RetryAt.After(c.limits[id]) {
		c.limits[id] = api.RetryAt
	}
	if api.Unauthorized() && c.tokens[id].Value == token {
		delete(c.tokens, id)
	}
}
