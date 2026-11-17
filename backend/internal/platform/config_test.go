package platform

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigModes(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}), 0600); err != nil {
		t.Fatal(err)
	}
	badKeyFile := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badKeyFile, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
		demo    bool
	}{
		{"demo", nil, "", true},
		{"only id", map[string]string{"GITHUB_CLIENT_ID": "x"}, "both", false},
		{"only secret", map[string]string{"GITHUB_CLIENT_SECRET": "x"}, "both", false},
		{"real without key", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y"}, "TOKEN_ENCRYPTION_KEY", false},
		{"real short key", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y", "TOKEN_ENCRYPTION_KEY": "c2hvcnQ="}, "TOKEN_ENCRYPTION_KEY", false},
		{"real without webhook secret", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y", "TOKEN_ENCRYPTION_KEY": key}, "WEBHOOK_SECRET", false},
		{"real bad app url", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y", "TOKEN_ENCRYPTION_KEY": key, "WEBHOOK_SECRET": "s", "APP_URL": "localhost:3000/app"}, "APP_URL", false},
		{"real without app key", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y", "TOKEN_ENCRYPTION_KEY": key, "WEBHOOK_SECRET": "s"}, "GITHUB_APP_PRIVATE_KEY_FILE", false},
		{"real invalid app key", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y", "TOKEN_ENCRYPTION_KEY": key, "WEBHOOK_SECRET": "s", "GITHUB_APP_PRIVATE_KEY_FILE": badKeyFile}, "GITHUB_APP_PRIVATE_KEY_FILE", false},
		{"real", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y", "TOKEN_ENCRYPTION_KEY": key, "WEBHOOK_SECRET": "s", "GITHUB_APP_PRIVATE_KEY_FILE": keyFile}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET", "TOKEN_ENCRYPTION_KEY", "WEBHOOK_SECRET", "GITHUB_APP_PRIVATE_KEY_FILE"} {
				t.Setenv(k, tc.env[k])
			}
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv("APP_URL", tc.env["APP_URL"])
			cfg, err := LoadConfig()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil || cfg.Demo != tc.demo {
				t.Fatalf("demo=%v err=%v", cfg.Demo, err)
			}
		})
	}
}
