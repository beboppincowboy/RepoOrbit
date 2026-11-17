// Command fakegithub serves the fake GitHub for local end-to-end runs. Never deploy it.
package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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
	if path := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatal("cannot read fake app key")
		}
		block, _ := pem.Decode(data)
		if block == nil {
			log.Fatal("invalid fake app key")
		}
		var key *rsa.PrivateKey
		if block.Type == "RSA PRIVATE KEY" {
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		} else {
			var parsed any
			parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
			key, _ = parsed.(*rsa.PrivateKey)
		}
		if err != nil || key == nil {
			log.Fatal("invalid fake app key")
		}
		s.AppPublicKey = &key.PublicKey
	}
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
