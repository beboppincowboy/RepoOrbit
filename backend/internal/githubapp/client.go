// Package githubapp talks to GitHub (or a fake of it) on behalf of the GitHub App.
package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/oauth2"
)

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
	Access, Refresh               string
	AccessExpires, RefreshExpires time.Time
}

func (c *Client) ctx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, c.HTTP)
}

// AuthURL returns the GitHub authorization URL using PKCE. No scopes are requested; the app's permissions apply.
func (c *Client) AuthURL(state, verifier string) string {
	return c.OAuth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

func (c *Client) Exchange(ctx context.Context, code, verifier string) (Tokens, error) {
	t, err := c.OAuth.Exchange(c.ctx(ctx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Tokens{}, err
	}
	return toTokens(t, time.Now()), nil
}

// Refresh exchanges a refresh token. GitHub rotates refresh tokens, so the result must be stored.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	t, err := c.OAuth.TokenSource(c.ctx(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return Tokens{}, err
	}
	return toTokens(t, time.Now()), nil
}

func toTokens(t *oauth2.Token, now time.Time) Tokens {
	out := Tokens{Access: t.AccessToken, Refresh: t.RefreshToken, AccessExpires: t.Expiry}
	if s := fmt.Sprint(t.Extra("refresh_token_expires_in")); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			out.RefreshExpires = now.Add(time.Duration(secs) * time.Second)
		}
	}
	return out
}

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func (c *Client) User(ctx context.Context, accessToken string) (User, error) {
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
