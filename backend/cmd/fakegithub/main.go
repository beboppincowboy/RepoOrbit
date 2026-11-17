// Command fakegithub serves the fake GitHub for local end-to-end runs. Never deploy it.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
)

func main() {
	addr := os.Getenv("FAKE_GITHUB_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9090"
	}
	s := fake.New(os.Getenv("GITHUB_CLIENT_ID"), os.Getenv("GITHUB_CLIENT_SECRET"))
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
