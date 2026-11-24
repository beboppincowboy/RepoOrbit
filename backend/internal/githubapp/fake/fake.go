// Package fake is an in-memory stand-in for the parts of GitHub that RepoOrbit uses.
// It serves both github.com (OAuth) and api.github.com on one origin.
package fake

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Server struct {
	ClientID, ClientSecret string
	AppPublicKey           *rsa.PublicKey
	// RefreshDelay widens the window for concurrent-refresh tests.
	RefreshDelay time.Duration
	// SetupURL is where the fake app-install page sends the browser, e.g. http://app.test/auth/github/installed.
	SetupURL string

	mu        sync.Mutex
	n         int
	codes     map[string]grant // authorization code -> grant
	access    map[string]string
	refresh   map[string]string // refresh token -> login (single use, rotated)
	refreshes atomic.Int64

	repos        map[int64]*repo
	installs     map[int64]*install
	stars        map[string]map[int64]bool // login -> starred repository IDs
	issues       map[int64][]IssueFixture
	pulls        map[int64][]PullRequestFixture
	reviews      map[int64]map[int64][]ReviewFixture
	comments     map[int64]map[int64][]CommentFixture
	releases     map[int64][]ReleaseFixture
	issueEvents  map[int64][]IssueEventFixture
	perms        map[int64]map[string]string // repository -> login -> granted level (read, triage, write, maintain, admin)
	writeFault   IssueWriteFault
	writeTokens  []string
	limitedUntil time.Time // API calls fail with a rate limit until this time
	failing      bool      // API calls fail with 502
	hits         map[string]int
}

type repo struct {
	id                int64
	owner, name, desc string
	private           bool
}

// IssueFixture is the source data returned by GitHub's issues endpoint.
type IssueFixture struct {
	ID          int64        `json:"id"`
	Number      int64        `json:"number"`
	Title       string       `json:"title"`
	State       string       `json:"state"`
	HTMLURL     string       `json:"html_url"`
	Body        string       `json:"body"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	ClosedAt    *time.Time   `json:"closed_at"`
	User        IssueActor   `json:"user"`
	Labels      []IssueLabel `json:"labels"`
	Assignees   []IssueActor `json:"assignees"`
	PullRequest bool         `json:"-"`
}

type IssueActor struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type IssueLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// install is a GitHub App installation visible to one user, covering a fixed set of repositories.
type install struct {
	id    int64
	owner string
	repos map[int64]bool
}

type grant struct{ challenge, login string }

func New(clientID, clientSecret string) *Server {
	return &Server{ClientID: clientID, ClientSecret: clientSecret, codes: map[string]grant{}, access: map[string]string{}, refresh: map[string]string{}, repos: map[int64]*repo{}, installs: map[int64]*install{}, stars: map[string]map[int64]bool{}, issues: map[int64][]IssueFixture{}, pulls: map[int64][]PullRequestFixture{}, reviews: map[int64]map[int64][]ReviewFixture{}, comments: map[int64]map[int64][]CommentFixture{}, releases: map[int64][]ReleaseFixture{}, issueEvents: map[int64][]IssueEventFixture{}, perms: map[int64]map[string]string{}, hits: map[string]int{}}
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
	mux.HandleFunc("/app/installations", s.appAPI(s.appInstallations))
	mux.HandleFunc("/app/installations/{id}/access_tokens", s.appAPI(s.appInstallationToken))
	mux.HandleFunc("/repos/{owner}/{repo}/installation", s.appAPI(s.repositoryInstallation))
	mux.HandleFunc("/installation/repositories", s.appAPI(s.appRepositories))
	mux.HandleFunc("/user", s.api(s.user))
	mux.HandleFunc("/user/repos", s.api(s.userRepos))
	mux.HandleFunc("/user/starred", s.api(s.userStarred))
	mux.HandleFunc("/user/installations", s.api(s.userInstallations))
	mux.HandleFunc("/user/installations/{id}/repositories", s.api(s.installationRepos))
	mux.HandleFunc("/repositories/{id}", s.api(s.repoByID))
	mux.HandleFunc("/repos/{owner}/{repo}/issues", s.api(s.repoIssues))
	mux.HandleFunc("/repos/{owner}/{repo}/issues/{number}", s.api(s.issueByNumber))
	mux.HandleFunc("/repos/{owner}/{repo}/issues/{number}/labels", s.api(s.issueLabels))
	mux.HandleFunc("/repos/{owner}/{repo}/issues/{number}/labels/{name}", s.api(s.issueLabels))
	mux.HandleFunc("/repos/{owner}/{repo}/issues/{number}/assignees", s.api(s.issueAssignees))
	mux.HandleFunc("/repos/{owner}/{repo}/assignees/{login}", s.api(s.assignable))
	mux.HandleFunc("/repos/{owner}/{repo}/releases", s.api(s.repoActivity))
	mux.HandleFunc("/repos/{owner}/{repo}/issues/events", s.api(s.repoActivity))
	mux.HandleFunc("/repos/{owner}/{repo}/pulls", s.api(s.repoActivity))
	mux.HandleFunc("/repos/{owner}/{repo}/pulls/{number}/reviews", s.api(s.repoActivity))
	mux.HandleFunc("/repos/{owner}/{repo}/issues/{number}/comments", s.api(s.repoActivity))
	mux.HandleFunc("/apps/{slug}/installations/new", s.installNew)
	return mux
}

func (s *Server) next(prefix string) string {
	s.n++
	return fmt.Sprintf("%s_%d", prefix, s.n)
}

// authorize auto-approves. The "as" parameter, or else the "fake_as" cookie (so a browser test can pick an account), picks the signed-in fake account.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != s.ClientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	login := asLogin(r)
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

func (s *Server) user(w http.ResponseWriter, r *http.Request, login string) {
	writeJSON(w, map[string]any{"id": idFor(login), "login": login})
}

// api wraps an authenticated API endpoint: it counts the call, applies the injected failure modes, and
// resolves the bearer token to a login (401 when the token is unknown or expired).
func (s *Server) api(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		s.mu.Lock()
		s.hits[r.URL.Path]++
		login, ok := "", false
		if len(auth) > len(prefix) {
			login, ok = s.access[auth[len(prefix):]]
			if raw, installation := strings.CutPrefix(auth, "Bearer installation_"); installation {
				id, _ := strconv.ParseInt(raw, 10, 64)
				if in := s.installs[id]; in != nil {
					for repoID := range in.repos {
						repo := s.repos[repoID]
						if repo != nil && repo.owner == r.PathValue("owner") && repo.name == r.PathValue("repo") {
							login, ok = in.owner, true
							break
						}
					}
				}
			}
		}
		limited, failing := s.limitedUntil, s.failing
		s.mu.Unlock()
		switch {
		case !ok:
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		case time.Now().Before(limited):
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(limited.Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		case failing:
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
		default:
			h(w, r, login)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- test controls -------------------------------------------------------------------------

// AddRepo creates a repository and returns its ID. IDs start high so they never collide with the demo seed.
func (s *Server) AddRepo(owner, name string, private bool) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := int64(100000 + len(s.repos) + 1)
	s.repos[id] = &repo{id: id, owner: owner, name: name, desc: name + " description", private: private}
	return id
}

// Star makes login a stargazer of the repository; Unstar undoes it.
func (s *Server) Star(login string, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stars[login] == nil {
		s.stars[login] = map[int64]bool{}
	}
	s.stars[login][id] = true
}

func (s *Server) Unstar(login string, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stars[login], id)
}

// SetIssues replaces the fixture issue list for a repository. Fixture order is preserved across pages.
func (s *Server) SetIssues(repoID int64, issues ...IssueFixture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[repoID] = append([]IssueFixture(nil), issues...)
}

func (s *Server) SetPrivate(id int64, private bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[id].private = private
}

// Install adds an installation owned by (and visible to) one user covering the given repositories.
func (s *Server) Install(owner string, id int64, repoIDs ...int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := &install{id: id, owner: owner, repos: map[int64]bool{}}
	for _, r := range repoIDs {
		in.repos[r] = true
	}
	s.installs[id] = in
}

func (s *Server) Uninstall(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.installs, id)
}

// RevokeUser invalidates every access token issued to a login, as when the user revokes the app.
func (s *Server) RevokeUser(login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, l := range s.access {
		if l == login {
			delete(s.access, tok)
		}
	}
}

// LimitUntil makes every API call fail with a rate limit until t.
func (s *Server) LimitUntil(t time.Time) { s.mu.Lock(); s.limitedUntil = t; s.mu.Unlock() }

// Fail makes every API call fail with 502 while on.
func (s *Server) Fail(on bool) { s.mu.Lock(); s.failing = on; s.mu.Unlock() }

// Hits counts API requests to an exact path since the server started.
func (s *Server) Hits(path string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.hits[path] }

// --- endpoints -----------------------------------------------------------------------------

// canRead mirrors a GitHub App user token: public repos are readable by anyone; private ones need the
// user to own the repo and an installation of the app that covers it (the intersection). Caller holds mu.
func (s *Server) canRead(login string, r *repo) bool {
	if !r.private {
		return true
	}
	if r.owner != login {
		return false
	}
	for _, in := range s.installs {
		if in.owner == login && in.repos[r.id] {
			return true
		}
	}
	return false
}

// json renders a repository as login sees it, including that login's permissions (none for an installation token).
func (s *Server) json(r *repo, login string) map[string]any {
	level := s.level(login, r)
	return map[string]any{"id": r.id, "name": r.name, "full_name": r.owner + "/" + r.name, "description": r.desc, "private": r.private, "has_issues": true,
		"owner":       map[string]any{"id": idFor(r.owner), "login": r.owner, "type": "User"},
		"permissions": map[string]bool{"pull": level != "", "triage": atLeast(level, "triage"), "push": atLeast(level, "write"), "maintain": atLeast(level, "maintain"), "admin": level == "admin"}}
}

// page slices items for ?page&per_page and sets a Link header when more remain.
func page[T any](w http.ResponseWriter, r *http.Request, items []T) []T {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per < 1 || per > 100 {
		per = 30
	}
	if pg < 1 {
		pg = 1
	}
	lo := len(items)
	if pg-1 <= len(items)/per {
		lo = (pg - 1) * per
	}
	hi := min(lo+per, len(items))
	if hi < len(items) {
		w.Header().Set("Link", fmt.Sprintf(`<%s?page=%d&per_page=%d>; rel="next"`, r.URL.Path, pg+1, per))
	}
	return items[lo:hi]
}

func (s *Server) sortedRepos(login string, keep func(*repo) bool) []map[string]any {
	var ids []int64
	for id, r := range s.repos {
		if keep(r) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []map[string]any{}
	for _, id := range ids {
		out = append(out, s.json(s.repos[id], login))
	}
	return out
}

func (s *Server) userRepos(w http.ResponseWriter, r *http.Request, login string) {
	s.mu.Lock()
	items := s.sortedRepos(login, func(x *repo) bool { return x.owner == login && s.canRead(login, x) })
	s.mu.Unlock()
	writeJSON(w, page(w, r, items))
}

// userStarred lists the stars the token can read: public ones, and private ones only where it can read them.
func (s *Server) userStarred(w http.ResponseWriter, r *http.Request, login string) {
	s.mu.Lock()
	items := s.sortedRepos(login, func(x *repo) bool { return s.stars[login][x.id] && s.canRead(login, x) })
	s.mu.Unlock()
	writeJSON(w, page(w, r, items))
}

func (s *Server) userInstallations(w http.ResponseWriter, r *http.Request, login string) {
	s.mu.Lock()
	var ids []int64
	for id, in := range s.installs {
		if in.owner == login {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	items := []map[string]any{}
	for _, id := range ids {
		items = append(items, map[string]any{"id": id, "account": map[string]any{"login": login}})
	}
	writeJSON(w, map[string]any{"total_count": len(items), "installations": page(w, r, items)})
}

func (s *Server) installationRepos(w http.ResponseWriter, r *http.Request, login string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	in, ok := s.installs[id]
	if !ok || in.owner != login {
		s.mu.Unlock()
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	items := s.sortedRepos(login, func(x *repo) bool { return in.repos[x.id] && s.canRead(login, x) })
	s.mu.Unlock()
	writeJSON(w, map[string]any{"total_count": len(items), "repositories": page(w, r, items)})
}

func (s *Server) repoByID(w http.ResponseWriter, r *http.Request, login string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	x, ok := s.repos[id]
	if ok && s.canRead(login, x) {
		out := s.json(x, login)
		s.mu.Unlock()
		writeJSON(w, out)
		return
	}
	s.mu.Unlock()
	http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
}

func (s *Server) repoIssues(w http.ResponseWriter, r *http.Request, login string) {
	if r.Method == http.MethodPost {
		s.createIssue(w, r, login)
		return
	}
	if r.URL.Query().Get("state") != "all" {
		http.Error(w, `{"message":"Bad Request"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	var id int64
	var found bool
	for repoID, x := range s.repos {
		if x.owner == r.PathValue("owner") && x.name == r.PathValue("repo") {
			id, found = repoID, s.canRead(login, x)
			break
		}
	}
	if !found {
		s.mu.Unlock()
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	items := append([]IssueFixture(nil), s.issues[id]...)
	s.mu.Unlock()

	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per < 1 || per > 100 {
		per = 30
	}
	if pg < 1 {
		pg = 1
	}
	lo, hi := min((pg-1)*per, len(items)), min(pg*per, len(items))
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if hi < len(items) {
		w.Header().Set("Link", fmt.Sprintf(`<%s://%s%s?state=all&per_page=%d&page=%d>; rel="next"`, scheme, r.Host, r.URL.Path, per, pg+1))
	}
	out := make([]map[string]any, 0, hi-lo)
	for _, issue := range items[lo:hi] {
		out = append(out, issueJSON(r, issue))
	}
	writeJSON(w, out)
}

// installNew stands in for GitHub's app-install page: ?as=<login> installs the app on every private
// repository that login owns, then returns the browser to the setup URL like GitHub does.
func (s *Server) installNew(w http.ResponseWriter, r *http.Request) {
	login := asLogin(r)
	if login == "" || s.SetupURL == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	id := int64(9000 + len(s.installs) + 1)
	in := &install{id: id, owner: login, repos: map[int64]bool{}}
	for _, x := range s.repos {
		if x.owner == login {
			in.repos[x.id] = true
		}
	}
	s.installs[id] = in
	s.mu.Unlock()
	http.Redirect(w, r, s.SetupURL+"?installation_id="+strconv.FormatInt(id, 10)+"&setup_action=install", http.StatusFound)
}

// idFor gives each fake login a stable numeric GitHub ID.
func idFor(login string) int {
	sum := sha256.Sum256([]byte(login))
	return int(sum[0])<<16 | int(sum[1])<<8 | int(sum[2]) + 1000
}

// asLogin names the fake account a browser acts as: the "as" parameter, else the "fake_as" cookie.
func asLogin(r *http.Request) string {
	if v := r.URL.Query().Get("as"); v != "" {
		return v
	}
	if c, err := r.Cookie("fake_as"); err == nil {
		return c.Value
	}
	return ""
}

// appAuthorized validates the app JWT using the configured public key.
func (s *Server) appAuthorized(r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 || s.AppPublicKey == nil {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(s.AppPublicKey, crypto.SHA256, digest[:], signature) != nil {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	return json.Unmarshal(payload, &claims) == nil && claims.Iss == s.ClientID && claims.Iat <= time.Now().Unix() && claims.Exp > time.Now().Unix()
}
func (s *Server) appInstallations(w http.ResponseWriter, r *http.Request) {
	if !s.appAuthorized(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	s.mu.Lock()
	ids := make([]int64, 0, len(s.installs))
	for id := range s.installs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	items := []map[string]any{}
	for _, id := range ids {
		items = append(items, map[string]any{"id": id, "suspended_at": nil})
	}
	writeJSON(w, page(w, r, items))
}
func (s *Server) appInstallationToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !s.appAuthorized(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	_, ok := s.installs[id]
	s.mu.Unlock()
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]any{"token": fmt.Sprintf("installation_%d", id), "expires_at": time.Now().Add(time.Hour)})
}
func (s *Server) appRepositories(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer installation_")
	id, err := strconv.ParseInt(token, 10, 64)
	s.mu.Lock()
	in, ok := s.installs[id]
	if err != nil || !ok {
		s.mu.Unlock()
		http.Error(w, "unauthorized", 401)
		return
	}
	items := s.sortedRepos("", func(x *repo) bool { return in.repos[x.id] })
	s.mu.Unlock()
	writeJSON(w, map[string]any{"total_count": len(items), "repositories": page(w, r, items)})
}

func (s *Server) appAPI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		limited, failing := s.limitedUntil, s.failing
		s.mu.Unlock()
		if time.Now().Before(limited) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(limited.Unix(), 10))
			http.Error(w, "rate limited", http.StatusForbidden)
			return
		}
		if failing {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		h(w, r)
	}
}

func (s *Server) repositoryInstallation(w http.ResponseWriter, r *http.Request) {
	if !s.appAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, in := range s.installs {
		for repoID := range in.repos {
			repo := s.repos[repoID]
			if repo != nil && repo.owner == r.PathValue("owner") && repo.name == r.PathValue("repo") {
				writeJSON(w, map[string]any{"id": id, "suspended_at": nil})
				return
			}
		}
	}
	http.NotFound(w, r)
}
