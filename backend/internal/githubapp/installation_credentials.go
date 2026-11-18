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

// NewInstallationCredentials creates an empty, in-memory token and cooldown cache for one GitHub App.
func NewInstallationCredentials(client *Client, key *rsa.PrivateKey) *InstallationCredentials {
	return &InstallationCredentials{client: client, key: key, tokens: make(map[int64]installationToken), limits: make(map[int64]time.Time)}
}

// AccessToken returns the installation credential, minting over HTTP under a shared lock when renewal is due.
// It reuses tokens until one minute before expiry and returns an APIError during a known cooldown.
func (c *InstallationCredentials) AccessToken(ctx context.Context, installationID int64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if until := c.limits[installationID]; until.After(now) {
		return "", &APIError{Status: 429, RetryAt: until}
	}
	if token := c.tokens[installationID]; token.ExpiresAt.After(now.Add(time.Minute)) {
		return token.Value, nil
	}
	jwt, err := c.client.appJWT(c.key)
	if err != nil {
		return "", err
	}
	token, err := c.client.requestInstallationToken(ctx, jwt, installationID)
	if err != nil {
		return "", err
	}
	if !token.ExpiresAt.After(now) {
		return "", fmt.Errorf("github app: expired installation credential")
	}
	c.tokens[installationID] = token
	return token.Value, nil
}

// RecordAPIError extends the installation cooldown for rate limits. A 401 removes the token only if it is
// still the cached value. Pass only errors and tokens from requests made for this installation.
func (c *InstallationCredentials) RecordAPIError(installationID int64, token string, api *APIError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if api.RetryAt.After(c.limits[installationID]) {
		c.limits[installationID] = api.RetryAt
	}
	if api.Unauthorized() && c.tokens[installationID].Value == token {
		delete(c.tokens, installationID)
	}
}
