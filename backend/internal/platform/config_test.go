package platform

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadConfigModes(t *testing.T) {
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
		{"real bad app url", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y", "TOKEN_ENCRYPTION_KEY": key, "APP_URL": "localhost:3000/app"}, "APP_URL", false},
		{"real", map[string]string{"GITHUB_CLIENT_ID": "x", "GITHUB_CLIENT_SECRET": "y", "TOKEN_ENCRYPTION_KEY": key}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET", "TOKEN_ENCRYPTION_KEY"} {
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
