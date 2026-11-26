package platform

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config is loaded once at startup by LoadConfig. Demo is true when no GitHub credentials are set.
// Each field names its environment variable.
type Config struct {
	DatabaseURL  string // DATABASE_URL, required
	ListenAddr   string // API_ADDR, default ":8080"
	Demo         bool
	ClientID     string // GITHUB_CLIENT_ID; with ClientSecret, both or neither
	ClientSecret string // GITHUB_CLIENT_SECRET
	// GitHubURL and GitHubAPIURL are configurable so tests and local development can use a fake GitHub.
	GitHubURL    string // GITHUB_URL, default https://github.com
	GitHubAPIURL string // GITHUB_API_URL, default https://api.github.com
	// AppURL is the browser-facing origin (the web server) that GitHub redirects back to.
	AppURL string // APP_URL, default http://localhost:3000; no trailing slash
	// AppSlug is the GitHub App's URL name; optional, it only enables the "install the app" link.
	AppSlug string // GITHUB_APP_SLUG
	// TokenKey is the 32-byte AES-GCM key for stored GitHub tokens (real mode only).
	TokenKey []byte // TOKEN_ENCRYPTION_KEY, base64
	// WebhookSecret is the GitHub App webhook secret used to verify X-Hub-Signature-256 (real mode only, required).
	WebhookSecret []byte          // WEBHOOK_SECRET
	AppPrivateKey *rsa.PrivateKey // GITHUB_APP_PRIVATE_KEY_FILE
}

// LoadConfig reads and validates the environment. It returns an error naming the bad variable; in Demo mode it
// skips the real-mode checks (TOKEN_ENCRYPTION_KEY, WEBHOOK_SECRET, APP_URL shape).
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
	if cfg.WebhookSecret = []byte(os.Getenv("WEBHOOK_SECRET")); len(cfg.WebhookSecret) == 0 {
		return Config{}, fmt.Errorf("WEBHOOK_SECRET is required in real mode (the GitHub App webhook secret)")
	}
	if u, err := url.Parse(cfg.AppURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		return Config{}, fmt.Errorf("APP_URL must be an origin such as https://example.com")
	}
	keyPEM, err := os.ReadFile(os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"))
	if err != nil {
		return Config{}, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE must point to a readable RSA private key")
	}
	block, rest := pem.Decode(keyPEM)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return Config{}, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE must contain an RSA private key")
	}
	var privateKey *rsa.PrivateKey
	if block.Type == "RSA PRIVATE KEY" {
		privateKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	} else if block.Type == "PRIVATE KEY" {
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			privateKey, _ = parsed.(*rsa.PrivateKey)
		}
	} else {
		err = fmt.Errorf("invalid key type")
	}
	if err != nil || privateKey == nil || privateKey.Validate() != nil {
		return Config{}, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE must contain a valid RSA private key")
	}
	cfg.AppPrivateKey = privateKey

	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
