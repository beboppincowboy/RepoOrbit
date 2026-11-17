package platform

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config is loaded once at startup. Demo is true when no GitHub credentials are set.
type Config struct {
	DatabaseURL  string
	ListenAddr   string
	Demo         bool
	ClientID     string
	ClientSecret string
	// GitHubURL and GitHubAPIURL are configurable so tests and local development can use a fake GitHub.
	GitHubURL    string
	GitHubAPIURL string
	// AppURL is the browser-facing origin (the Next.js server) that GitHub redirects back to.
	AppURL string
	// AppSlug is the GitHub App's URL name; optional, it only enables the "install the app" link.
	AppSlug string
	// TokenKey is the 32-byte AES-GCM key for stored GitHub tokens (real mode only).
	TokenKey []byte
}

func LoadConfig() (Config, error) {
	cfg := Config{
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		ListenAddr:   envOr("API_ADDR", ":8080"),
		ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		GitHubURL:    envOr("GITHUB_URL", "https://github.com"),
		GitHubAPIURL: envOr("GITHUB_API_URL", "https://api.github.com"),
		AppURL:       strings.TrimRight(envOr("APP_URL", "http://localhost:3000"), "/"),
		AppSlug:      os.Getenv("GITHUB_APP_SLUG"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if (cfg.ClientID == "") != (cfg.ClientSecret == "") {
		return Config{}, fmt.Errorf("set both GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET, or neither for demo mode")
	}
	cfg.Demo = cfg.ClientID == ""
	if cfg.Demo {
		return cfg, nil
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 {
		return Config{}, fmt.Errorf("TOKEN_ENCRYPTION_KEY must be 32 random bytes, base64-encoded")
	}
	cfg.TokenKey = key
	if u, err := url.Parse(cfg.AppURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		return Config{}, fmt.Errorf("APP_URL must be an origin such as https://example.com")
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
