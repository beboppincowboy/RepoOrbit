//go:build integration

package repositories_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"

	graph "github.com/repoorbit/repoorbit/backend/graphql"
)

const appURL = "http://app.test"

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	q     *db.Queries
	gh    *fake.Server
	repos *repositories.Service
	srv   *httptest.Server // the Go API, as the browser-proxied path reaches it
	ghSrv *httptest.Server
}

// setup wires the real handlers (auth, repository API, GraphQL) to a migrated database and a fake GitHub.
// TTL is 1ns unless a test asks otherwise, so every read re-checks GitHub.
func setup(t *testing.T, ttl time.Duration) *env {
	t.Helper()
	_, pool := testutil.Database(t)
	gh := fake.New("cid", "secret")
	ghSrv := httptest.NewServer(gh.Handler())
	t.Cleanup(ghSrv.Close)
	client := githubapp.New("cid", "secret", ghSrv.URL, ghSrv.URL, appURL+"/auth/github/callback")
	q := db.New(pool)
	svc := &auth.Service{Pool: pool, Queries: q, GitHub: client, Key: bytes.Repeat([]byte{1}, 32)}
	repos := &repositories.Service{Queries: q, Pool: pool, GitHub: client, TokenSource: svc, TTL: ttl, AppURL: appURL, InstallURL: ghSrv.URL + "/apps/app/installations/new"}
	h := &auth.Handler{Service: svc, AppURL: appURL, Installed: http.HandlerFunc(repos.Installed)}
	mux := http.NewServeMux()
	mux.Handle("/auth/", h.Routes())
	mux.Handle("/api/", h.Protect(repos.Routes()))
	mux.Handle("/graphql", h.Protect(graph.Server(&graph.Resolver{Queries: q, RepoAccess: repos})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh.SetupURL = srv.URL + "/auth/github/installed"
	return &env{t: t, pool: pool, q: q, gh: gh, repos: repos, srv: srv, ghSrv: ghSrv}
}

type user struct {
	e      *env
	base   string // API origin the requests go to
	client *http.Client
	csrf   string
	id     int64
	login  string
}

var noRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// signIn completes the OAuth flow against the fake GitHub as login, returning a user bound to base.
func (e *env) signIn(login string) *user {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	u := &user{e: e, base: e.srv.URL, login: login, client: &http.Client{Jar: jar, CheckRedirect: noRedirect}}
	resp := u.do("GET", "/auth/github/login", "", nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	q.Set("as", login)
	loc.RawQuery = q.Encode()
	gh, err := (&http.Client{CheckRedirect: noRedirect}).Get(loc.String())
	if err != nil {
		e.t.Fatal(err)
	}
	gh.Body.Close()
	cb, _ := url.Parse(gh.Header.Get("Location"))
	if r := u.do("GET", cb.Path+"?"+cb.RawQuery, "", nil); r.StatusCode != 302 || r.Header.Get("Location") != appURL+"/" {
		e.t.Fatalf("sign in as %s: %d %s", login, r.StatusCode, r.Header.Get("Location"))
	}
	var me struct{ CSRFToken string }
	u.json("GET", "/auth/me", "", &me)
	u.csrf = me.CSRFToken
	if err := e.pool.QueryRow(context.Background(), "SELECT id FROM users WHERE login = $1", login).Scan(&u.id); err != nil {
		e.t.Fatal(err)
	}
	return u
}

// through returns the same signed-in user, reaching the API through a reverse proxy like the Next.js rewrite does.
func (u *user) through(proxy string) *user { c := *u; c.base = proxy; return &c }

func (u *user) do(method, path, body string, headers map[string]string) *http.Response {
	u.e.t.Helper()
	req, _ := http.NewRequest(method, u.base+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		u.e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp
}

func (u *user) json(method, path, body string, out any) *http.Response {
	u.e.t.Helper()
	resp := u.do(method, path, body, nil)
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == 200 {
		if err := json.Unmarshal(b, out); err != nil {
			u.e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp
}

// write sends a state-changing request with a valid Origin and CSRF token.
func (u *user) write(method, path string) *http.Response {
	return u.do(method, path, "", map[string]string{"Origin": appURL, "X-CSRF-Token": u.csrf})
}

func (u *user) gql(query string) (data map[string]any, errs []any) {
	u.e.t.Helper()
	body, _ := json.Marshal(map[string]string{"query": query})
	resp := u.do("POST", "/graphql", string(body), map[string]string{"Content-Type": "application/json", "Origin": appURL, "X-CSRF-Token": u.csrf})
	if resp.StatusCode != 200 {
		u.e.t.Fatalf("graphql status %d", resp.StatusCode)
	}
	var out struct {
		Data   map[string]any
		Errors []any
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		u.e.t.Fatalf("%v: %s", err, b)
	}
	return out.Data, out.Errors
}

// proxy returns a reverse proxy to the Go API, standing in for the Next.js rewrite.
func (e *env) proxy() string {
	target, _ := url.Parse(e.srv.URL)
	p := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	e.t.Cleanup(p.Close)
	return p.URL
}

func (e *env) addIssue(repoID int64, title string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO issues (id, repository_id, title, state) VALUES ($1, $2, $3, 'open')", repoID*10, repoID, title); err != nil {
		e.t.Fatal(err)
	}
}

func repoNames(data map[string]any) (names []string, withheld int, retryAt any) {
	conn := data["selectedRepositories"].(map[string]any)
	for _, n := range conn["nodes"].([]any) {
		names = append(names, n.(map[string]any)["fullName"].(string))
	}
	return names, int(conn["withheld"].(float64)), conn["retryAt"]
}

const repoQuery = `{ selectedRepositories(first: 50) { nodes { id fullName private selected syncState } endCursor hasNextPage withheld retryAt } }`

func issueTitles(data map[string]any) (titles []string) {
	for _, n := range data["issues"].(map[string]any)["nodes"].([]any) {
		titles = append(titles, n.(map[string]any)["title"].(string))
	}
	return titles
}

func issuesQuery(ids ...int64) string {
	var list []string
	for _, id := range ids {
		list = append(list, strconv.FormatInt(id, 10))
	}
	return fmt.Sprintf(`{ issues(repositoryIds: ["%s"], first: 50) { nodes { title } withheld retryAt } }`, strings.Join(list, `","`))
}

func (u *user) selectRepo(id int64) int {
	return u.write("PUT", fmt.Sprintf("/api/repositories/%d/selection", id)).StatusCode
}

// AE1, AE3: two users never see each other's private repositories or selections, including through
// GraphQL, guessed IDs, and selection writes. A public repo is readable by both but is not selected by the other.
func TestUsersCannotCrossRead(t *testing.T) {
	e := setup(t, time.Nanosecond)
	secret, pub, bobPub := e.gh.AddRepo("alice", "secret", true), e.gh.AddRepo("alice", "pub", false), e.gh.AddRepo("bob", "pub", false)
	e.gh.Install("alice", 1, secret)
	alice, bob := e.signIn("alice"), e.signIn("bob")
	for _, id := range []int64{secret, pub} {
		if got := alice.selectRepo(id); got != 200 {
			t.Fatalf("alice select %d: %d", id, got)
		}
		e.addIssue(id, fmt.Sprintf("issue in %d", id))
	}
	data, errs := alice.gql(repoQuery)
	if names, _, _ := repoNames(data); len(errs) > 0 || len(names) != 2 {
		t.Fatalf("alice sees %v %v", names, errs)
	}
	if data, _ := bob.gql(repoQuery); func() bool { n, w, _ := repoNames(data); return len(n) != 0 || w != 0 }() {
		t.Fatalf("bob sees alice's selections: %v", data)
	}
	// Guessed IDs: bob asks for alice's issues directly, and for her repositories one by one.
	if data, _ := bob.gql(issuesQuery(secret, pub)); len(issueTitles(data)) != 0 {
		t.Fatalf("bob read alice's issues: %v", data)
	}
	if data, _ := alice.gql(issuesQuery(secret, pub, bobPub)); len(issueTitles(data)) != 2 {
		t.Fatalf("alice issues: %v", data)
	}
	if data, _ := bob.gql(fmt.Sprintf(`{ repository(id: "%d") { fullName } }`, secret)); data["repository"] != nil {
		t.Fatalf("bob read alice's private repository: %v", data)
	}
	// R1: the public repo opens for bob with the not-synchronized state; it is not his selection.
	data, _ = bob.gql(fmt.Sprintf(`{ repository(id: "%d") { fullName selected syncState } }`, pub))
	if r, _ := data["repository"].(map[string]any); r == nil || r["selected"] != false || r["syncState"] != "not-synchronized" {
		t.Fatalf("bob public view: %v", data)
	}
	// Selection writes by the wrong user change nothing.
	if got := bob.selectRepo(secret); got != 404 {
		t.Fatalf("bob select alice's private repo: %d", got)
	}
	if got := bob.selectRepo(pub); got != 403 {
		t.Fatalf("bob select alice's public repo as his own: %d", got)
	}
	if got := bob.write("DELETE", fmt.Sprintf("/api/repositories/%d/selection", pub)).StatusCode; got != 404 {
		t.Fatalf("bob deselect alice's repo: %d", got)
	}
	var active int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM selections WHERE user_id = $1 AND deselected_at IS NULL", alice.id).Scan(&active); err != nil || active != 2 {
		t.Fatalf("alice's selections changed: %d %v", active, err)
	}
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM selections WHERE user_id = $1", bob.id).Scan(&active); err != nil || active != 0 {
		t.Fatalf("bob has selections: %d %v", active, err)
	}
}

// Test 2: uninstall, a public-to-private flip, and user revocation deny reads immediately, with no sync in between.
func TestRevocationDeniesReads(t *testing.T) {
	e := setup(t, time.Nanosecond)
	secret, pub := e.gh.AddRepo("alice", "secret", true), e.gh.AddRepo("alice", "pub", false)
	e.gh.Install("alice", 1, secret, pub)
	alice := e.signIn("alice")
	alice.selectRepo(secret)
	alice.selectRepo(pub)
	e.addIssue(secret, "secret issue")
	e.addIssue(pub, "public issue")

	e.gh.Uninstall(1) // the app is removed: the private repo goes; the public one is still the user's own
	data, _ := alice.gql(repoQuery)
	if names, withheld, _ := repoNames(data); len(names) != 1 || names[0] != "alice/pub" || withheld != 0 {
		t.Fatalf("after uninstall: %v %d", names, withheld)
	}
	if data, _ := alice.gql(issuesQuery(secret, pub)); len(issueTitles(data)) != 1 {
		t.Fatalf("issues after uninstall: %v", data)
	}
	// A repo that turns private without an installation is not served from its cached public classification.
	e.gh.SetPrivate(pub, true)
	if data, _ := alice.gql(repoQuery); func() bool { n, _, _ := repoNames(data); return len(n) != 0 }() {
		t.Fatalf("newly private repo still served: %v", data)
	}
	// Reinstalling restores access without any re-selection.
	e.gh.Install("alice", 2, secret, pub)
	if data, _ := alice.gql(repoQuery); func() bool { n, _, _ := repoNames(data); return len(n) != 2 }() {
		t.Fatalf("after reinstall: %v", data)
	}
	// The user revokes the app: GitHub rejects the token, reads are denied, and the session ends.
	e.gh.RevokeUser("alice")
	if data, _ := alice.gql(repoQuery); func() bool { n, w, _ := repoNames(data); return len(n) != 0 || w != 0 }() {
		t.Fatalf("after revocation: %v", data)
	}
	if got := alice.do("POST", "/graphql", `{"query":"{ viewer { login } }"}`, map[string]string{"Origin": appURL, "X-CSRF-Token": alice.csrf}).StatusCode; got != 401 {
		t.Fatalf("session survived revocation: %d", got)
	}
}

// Test 3: an upstream failure or rate limit never turns into an empty list or an allowed write.
func TestUpstreamFailuresFailClosed(t *testing.T) {
	e := setup(t, time.Nanosecond)
	secret := e.gh.AddRepo("alice", "secret", true)
	other := e.gh.AddRepo("alice", "other", true)
	e.gh.Install("alice", 1, secret, other)
	alice := e.signIn("alice")
	alice.selectRepo(secret)
	e.addIssue(secret, "secret issue")

	e.gh.Fail(true)
	data, _ := alice.gql(repoQuery)
	if names, withheld, retry := repoNames(data); len(names) != 0 || withheld != 1 || retry != nil {
		t.Fatalf("upstream error: %v %d %v", names, withheld, retry)
	}
	if data, _ := alice.gql(issuesQuery(secret)); len(issueTitles(data)) != 0 || data["issues"].(map[string]any)["withheld"].(float64) != 1 {
		t.Fatalf("issues on upstream error: %v", data)
	}
	if got := alice.selectRepo(other); got != 503 {
		t.Fatalf("write on upstream error: %d", got)
	}
	e.gh.Fail(false)

	until := time.Now().Add(time.Hour)
	e.gh.LimitUntil(until)
	data, _ = alice.gql(repoQuery)
	names, withheld, retry := repoNames(data)
	if len(names) != 0 || withheld != 1 || retry == nil {
		t.Fatalf("rate limited: %v %d %v", names, withheld, retry)
	}
	if got, _ := time.Parse(time.RFC3339, retry.(string)); got.Unix() != until.Unix() {
		t.Fatalf("retryAt %v, want %v", retry, until)
	}
	resp := alice.write("PUT", fmt.Sprintf("/api/repositories/%d/selection", other))
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("rate-limited write: %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	e.gh.LimitUntil(time.Time{})
	if data, _ := alice.gql(repoQuery); func() bool { n, w, _ := repoNames(data); return len(n) != 1 || w != 0 }() {
		t.Fatalf("recovery: %v", data)
	}
}

// Test 4: the Go API and the browser-proxied path enforce identical rules.
func TestDirectAndProxiedRequestsAgree(t *testing.T) {
	e := setup(t, time.Nanosecond)
	repo := e.gh.AddRepo("alice", "pub", false)
	alice := e.signIn("alice")
	for name, u := range map[string]*user{"direct": alice, "proxied": alice.through(e.proxy())} {
		anon := &user{e: e, base: u.base, client: &http.Client{CheckRedirect: noRedirect}}
		path := fmt.Sprintf("/api/repositories/%d/selection", repo)
		if got := anon.do("GET", "/api/repositories/owned", "", nil).StatusCode; got != 401 {
			t.Fatalf("%s: signed-out read: %d", name, got)
		}
		if got := u.do("PUT", path, "", nil).StatusCode; got != 403 {
			t.Fatalf("%s: write without csrf/origin: %d", name, got)
		}
		if got := u.do("PUT", path, "", map[string]string{"Origin": "http://evil.test", "X-CSRF-Token": u.csrf}).StatusCode; got != 403 {
			t.Fatalf("%s: cross-origin write: %d", name, got)
		}
		if got := u.write("PUT", path).StatusCode; got != 200 {
			t.Fatalf("%s: write: %d", name, got)
		}
		if got := u.write("DELETE", path).StatusCode; got != 200 {
			t.Fatalf("%s: delete: %d", name, got)
		}
	}
}

// Test 5: the setup callback accepts only installations that appear in the signed-in user's own list.
func TestForgedInstallationRejected(t *testing.T) {
	e := setup(t, time.Nanosecond)
	e.gh.Install("alice", 77)
	alice, bob := e.signIn("alice"), e.signIn("bob")
	for _, c := range []struct {
		u    *user
		id   string
		want string
	}{{bob, "77", "rejected"}, {bob, "abc", "rejected"}, {bob, "", "rejected"}, {alice, "78", "rejected"}, {alice, "77", "ok"}} {
		resp := c.u.do("GET", "/auth/github/installed?setup_action=install&installation_id="+c.id, "", nil)
		if resp.StatusCode != 302 || resp.Header.Get("Location") != appURL+"/repositories?install="+c.want {
			t.Fatalf("%s id=%q: %d %s", c.u.login, c.id, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM installations").Scan(&n); err != nil || n != 1 {
		t.Fatalf("installations stored: %d %v", n, err)
	}
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM installations WHERE user_id = $1", alice.id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("alice's installation: %d %v", n, err)
	}
}

// Test 6 and 8: owned repositories page through GitHub, only explicit choices are stored, a failed refresh
// keeps the user's selections visible, and installing the app makes private repositories appear.
func TestOwnedRepositories(t *testing.T) {
	e := setup(t, time.Nanosecond)
	var ids []int64
	for i := 0; i < 34; i++ {
		ids = append(ids, e.gh.AddRepo("alice", fmt.Sprintf("r%02d", i), false))
	}
	secret := e.gh.AddRepo("alice", "secret", true) // not installed yet
	e.gh.AddRepo("bob", "other", false)
	alice := e.signIn("alice")
	var p1, p2 repositories.OwnedPage
	alice.json("GET", "/api/repositories/owned", "", &p1)
	alice.json("GET", fmt.Sprintf("/api/repositories/owned?page=%d", p1.NextPage), "", &p2)
	if len(p1.Repositories) != 30 || p1.NextPage != 2 || len(p2.Repositories) != 4 || p2.NextPage != 0 {
		t.Fatalf("pages: %d next %d, %d next %d", len(p1.Repositories), p1.NextPage, len(p2.Repositories), p2.NextPage)
	}
	if p1.InstallURL == "" || p1.RefreshFailed || p1.Repositories[0].SyncState != "not-synchronized" {
		t.Fatalf("page 1: %+v", p1)
	}
	var stored int
	if err := e.pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM repositories) + (SELECT count(*) FROM selections)").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("listing stored rows: %d %v", stored, err)
	}
	for _, p := range append(p1.Repositories, p2.Repositories...) {
		if p.FullName == "alice/secret" {
			t.Fatalf("uninstalled private repo listed: %+v", p)
		}
	}
	// Install through the fake app page, as the browser would, then refresh: the private repo appears.
	resp, err := (&http.Client{CheckRedirect: noRedirect}).Get(e.ghSrv.URL + "/apps/app/installations/new?as=alice")
	if err != nil || resp.StatusCode != 302 {
		t.Fatalf("install page: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if r := alice.do("GET", loc.Path+"?"+loc.RawQuery, "", nil); r.Header.Get("Location") != appURL+"/repositories?install=ok" {
		t.Fatalf("setup callback: %s", r.Header.Get("Location"))
	}
	var all []repositories.ListedRepo
	for page := 1; page != 0; {
		var p repositories.OwnedPage
		alice.json("GET", fmt.Sprintf("/api/repositories/owned?page=%d", page), "", &p)
		all, page = append(all, p.Repositories...), p.NextPage
	}
	if len(all) != 35 {
		t.Fatalf("after install: %d repos", len(all))
	}
	// Choices: one public, one private. Only those are stored.
	alice.selectRepo(ids[0])
	alice.selectRepo(secret)
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM repositories").Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("stored repositories: %d %v", stored, err)
	}
	// GitHub fails: the page falls back to the selections, never an empty list. The stored-public row stays; the
	// private one needs a live check, so it is withheld with a notice.
	e.gh.Fail(true)
	var failed repositories.OwnedPage
	alice.json("GET", "/api/repositories/owned", "", &failed)
	if !failed.RefreshFailed || len(failed.Repositories) != 1 || failed.Repositories[0].FullName != "alice/r00" || failed.Withheld != 1 || !failed.Repositories[0].Selected {
		t.Fatalf("failed refresh: %+v", failed)
	}
}

// One Selection per user and repository: owned plus starred is one row, and deselecting keeps it.
func TestSelectionIsOneRowAndSoftDeleted(t *testing.T) {
	e := setup(t, time.Nanosecond)
	repo := e.gh.AddRepo("alice", "pub", false)
	alice := e.signIn("alice")
	ctx := context.Background()
	if got := alice.selectRepo(repo); got != 200 {
		t.Fatal(got)
	}
	// A starred selection of the same repository merges into the owned row.
	if err := e.q.UpsertSelection(ctx, db.UpsertSelectionParams{UserID: alice.id, RepositoryID: repo, Source: "starred", SelectedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
		t.Fatal(err)
	}
	var rows int
	var source string
	if err := e.pool.QueryRow(ctx, "SELECT count(*), min(source) FROM selections").Scan(&rows, &source); err != nil || rows != 1 || source != "owned" {
		t.Fatalf("rows %d source %s: %v", rows, source, err)
	}
	if got := alice.write("DELETE", fmt.Sprintf("/api/repositories/%d/selection", repo)).StatusCode; got != 200 {
		t.Fatal(got)
	}
	if data, _ := alice.gql(repoQuery); func() bool { n, _, _ := repoNames(data); return len(n) != 0 }() {
		t.Fatalf("deselected repo listed: %v", data)
	}
	var deselected bool
	if err := e.pool.QueryRow(ctx, "SELECT deselected_at IS NOT NULL FROM selections").Scan(&deselected); err != nil || !deselected {
		t.Fatalf("row not kept as deselected: %v", err)
	}
	if got := alice.write("DELETE", fmt.Sprintf("/api/repositories/%d/selection", repo)).StatusCode; got != 404 {
		t.Fatalf("second deselect: %d", got)
	}
	if got := alice.selectRepo(repo); got != 200 { // selecting again reactivates the same row
		t.Fatal(got)
	}
	if err := e.pool.QueryRow(ctx, "SELECT count(*), bool_and(deselected_at IS NULL) FROM selections").Scan(&rows, &deselected); err != nil || rows != 1 || !deselected {
		t.Fatalf("reselect: %d %v %v", rows, deselected, err)
	}
}

// A real repository whose ID matches a demo row must fail loudly rather than attach a selection to the demo row.
func TestRepositoryUpsertNeverTouchesDemoRows(t *testing.T) {
	e := setup(t, time.Nanosecond)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, "INSERT INTO repositories (id, name, description, is_demo) VALUES (1, 'orbit/example', 'demo', true)"); err != nil {
		t.Fatal(err)
	}
	_, err := e.q.UpsertRepository(ctx, db.UpsertRepositoryParams{ID: 1, Name: "mojombo/grit", Description: "real"})
	if err != pgx.ErrNoRows {
		t.Fatalf("expected a loud failure, got %v", err)
	}
	var name string
	if err := e.pool.QueryRow(ctx, "SELECT name FROM repositories WHERE id = 1").Scan(&name); err != nil || name != "orbit/example" {
		t.Fatalf("demo row changed: %s %v", name, err)
	}
}

// One listing per user per TTL, even when a page fires many concurrent aliased queries (the web app does).
func TestVisibleSetIsFetchedOncePerTTL(t *testing.T) {
	e := setup(t, time.Minute)
	repo := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 1, repo)
	alice := e.signIn("alice")
	alice.selectRepo(repo) // a write: always live
	e.addIssue(repo, "issue")
	var skew atomic.Int64 // let the cache entry from that write expire, so the next reads start cold
	e.repos.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	skew.Store(int64(2 * time.Minute))
	baseline := e.gh.Hits("/user/installations")
	var aliases []string
	for i := 0; i < 20; i++ {
		aliases = append(aliases, fmt.Sprintf(`r%d: issues(repositoryIds: ["%d"], first: 5) { nodes { title } }`, i, repo))
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ { // concurrent requests from the same user
		wg.Add(1)
		go func() { defer wg.Done(); alice.gql("{ " + strings.Join(aliases, " ") + " }") }()
	}
	wg.Wait()
	if got := e.gh.Hits("/user/installations") - baseline; got != 1 {
		t.Fatalf("installation listings across 60 checks: %d, want 1", got)
	}
}

// A rate-limited GitHub is not re-hit by every read within the TTL.
func TestRateLimitedListingIsNotRefetchedPerRequest(t *testing.T) {
	e := setup(t, time.Minute)
	repo := e.gh.AddRepo("alice", "secret", true)
	e.gh.Install("alice", 1, repo)
	alice := e.signIn("alice")
	alice.selectRepo(repo)
	var skew atomic.Int64
	e.repos.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	skew.Store(int64(2 * time.Minute)) // drop the cache entry the write left behind
	e.gh.LimitUntil(time.Now().Add(time.Hour))
	baseline := e.gh.Hits("/user/repos")
	for i := 0; i < 5; i++ {
		data, _ := alice.gql(repoQuery)
		if names, withheld, retry := repoNames(data); len(names) != 0 || withheld != 1 || retry == nil {
			t.Fatalf("read %d: %v %d %v", i, names, withheld, retry)
		}
	}
	if got := e.gh.Hits("/user/repos") - baseline; got != 1 {
		t.Fatalf("GitHub was called %d times for 5 reads while rate limited, want 1", got)
	}
}
