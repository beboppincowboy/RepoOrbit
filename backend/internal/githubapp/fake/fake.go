// Package fake is an in-memory stand-in for the parts of GitHub that RepoOrbit uses.
// It serves both github.com (OAuth) and api.github.com on one origin.
package fake

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Server struct {
	ClientID, ClientSecret string
	// RefreshDelay widens the window for concurrent-refresh tests.
	RefreshDelay time.Duration

	mu        sync.Mutex
	n         int
	codes     map[string]grant // authorization code -> grant
	access    map[string]string
	refresh   map[string]string // refresh token -> login (single use, rotated)
	refreshes atomic.Int64
}

type grant struct{ challenge, login string }

func New(clientID, clientSecret string) *Server {
	return &Server{ClientID: clientID, ClientSecret: clientSecret, codes: map[string]grant{}, access: map[string]string{}, refresh: map[string]string{}}
}

// Refreshes reports how many refresh-token exchanges were accepted.
func (s *Server) Refreshes() int { return int(s.refreshes.Load()) }

// ExpireAccessToken makes a previously issued access token invalid, as after its lifetime ends.
func (s *Server) ExpireAccessToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.access, token)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/authorize", s.authorize)
	mux.HandleFunc("/login/oauth/access_token", s.token)
	mux.HandleFunc("/user", s.user)
	return mux
}

func (s *Server) next(prefix string) string {
	s.n++
	return fmt.Sprintf("%s_%d", prefix, s.n)
}

// authorize auto-approves. The optional "as" parameter picks the signed-in fake account.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != s.ClientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	login := q.Get("as")
	if login == "" {
		login = "octocat"
	}
	s.mu.Lock()
	code := s.next("code")
	s.codes[code] = grant{challenge: q.Get("code_challenge"), login: login}
	s.mu.Unlock()
	target, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || target.Host == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := target.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	target.RawQuery = v.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.ClientID || secret != s.ClientSecret {
		writeToken(w, url.Values{"error": {"incorrect_client_credentials"}})
		return
	}
	var login string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.mu.Lock()
		g, found := s.codes[r.PostForm.Get("code")]
		delete(s.codes, r.PostForm.Get("code")) // single use
		s.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !found || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			writeToken(w, url.Values{"error": {"bad_verification_code"}})
			return
		}
		login = g.login
	case "refresh_token":
		time.Sleep(s.RefreshDelay)
		s.mu.Lock()
		l, found := s.refresh[r.PostForm.Get("refresh_token")]
		delete(s.refresh, r.PostForm.Get("refresh_token")) // rotated: a refresh token works once
		s.mu.Unlock()
		if !found {
			writeToken(w, url.Values{"error": {"bad_refresh_token"}})
			return
		}
		s.refreshes.Add(1)
		login = l
	default:
		writeToken(w, url.Values{"error": {"unsupported_grant_type"}})
		return
	}
	s.mu.Lock()
	access, refresh := s.next("ghu"), s.next("ghr")
	s.access[access] = login
	s.refresh[refresh] = login
	s.mu.Unlock()
	writeToken(w, url.Values{"access_token": {access}, "token_type": {"bearer"}, "expires_in": {"28800"}, "refresh_token": {refresh}, "refresh_token_expires_in": {"15811200"}})
}

func writeToken(w http.ResponseWriter, v url.Values) {
	w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
	_, _ = w.Write([]byte(v.Encode()))
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	s.mu.Lock()
	login, ok := "", false
	if len(h) > len(prefix) {
		login, ok = s.access[h[len(prefix):]]
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":%s,"login":%q}`, strconv.Itoa(idFor(login)), login)
}

// idFor gives each fake login a stable numeric GitHub ID.
func idFor(login string) int {
	sum := sha256.Sum256([]byte(login))
	return int(sum[0])<<16 | int(sum[1])<<8 | int(sum[2]) + 1000
}
