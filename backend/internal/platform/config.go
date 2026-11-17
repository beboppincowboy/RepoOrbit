package platform

import (
	"fmt"
	"os"
)

type Config struct{ DatabaseURL string }

func LoadConfig() (Config, error) {
	if os.Getenv("GITHUB_CLIENT_ID") != "" || os.Getenv("GITHUB_CLIENT_SECRET") != "" {
		return Config{}, fmt.Errorf("GitHub authentication is not available in the U1 demo")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return Config{DatabaseURL: url}, nil
}
