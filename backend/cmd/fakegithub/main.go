// Command fakegithub serves the fake GitHub for local end-to-end runs. Never deploy it.
package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
)

func main() {
	addr := os.Getenv("FAKE_GITHUB_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9090"
	}
	s := fake.New(os.Getenv("GITHUB_CLIENT_ID"), os.Getenv("GITHUB_CLIENT_SECRET"))
	if app := strings.TrimRight(os.Getenv("APP_URL"), "/"); app != "" {
		s.SetupURL = app + "/auth/github/installed"
	}
	// Seed for scripts/e2e-auth.sh: ada owns a public repository and a private one the app is not yet installed on, and stars grace's public repository.
	s.AddRepo("ada", "notes", false)
	s.AddRepo("ada", "secret", true)
	// ...and has starred a public repository she does not own.
	s.Star("ada", s.AddRepo("grace", "compiler", false))
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
