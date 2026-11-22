//go:build integration

package issues_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/issues"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

const appURL = "http://app.test"

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	gh     *fake.Server
	client *githubapp.Client
	issues *issues.Service
	srv    *httptest.Server
	around atomic.Pointer[func(http.ResponseWriter, *http.Request, http.Handler)] // see intercept
}

// intercept routes every fake GitHub request through f, which serves it by calling next. It lets a test interleave
// steps with a request deterministically.
func (e *env) intercept(f func(w http.ResponseWriter, r *http.Request, next http.Handler)) {
	e.around.Store(&f)
}

// setup wires the real auth middleware, repository API and issue commands to a migrated database and a fake GitHub,
// the way cmd/api does.
func setup(t *testing.T) *env {
	t.Helper()
	_, pool := testutil.Database(t)
	gh := fake.New("cid", "secret")
	e := &env{t: t, pool: pool, gh: gh}
	ghHandler := gh.Handler()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f := e.around.Load(); f != nil {
			(*f)(w, r, ghHandler)
			return
		}
		ghHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(ghSrv.Close)
	client := githubapp.New("cid", "secret", ghSrv.URL, ghSrv.URL, appURL+"/auth/github/callback")
	q := db.New(pool)
	authSvc := &auth.Service{Pool: pool, Queries: q, GitHub: client, Key: bytes.Repeat([]byte{1}, 32)}
	repos := &repositories.Service{Queries: q, Pool: pool, GitHub: client, TokenSource: authSvc, TTL: time.Nanosecond, AppURL: appURL}
	svc := &issues.Service{Pool: pool, Queries: q, GitHub: client, Access: repos, GitHubURL: "https://github.test"}
	h := &auth.Handler{Service: authSvc, AppURL: appURL}
	api := http.NewServeMux()
	api.Handle("/api/", repos.Routes())
	svc.Register(api)
	mux := http.NewServeMux()
	mux.Handle("/auth/", h.Routes())
	mux.Handle("/api/", h.Protect(api))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e.client, e.issues, e.srv = client, svc, srv
	return e
}

// repo creates a repository on the fake GitHub and its local row (as a selection would).
func (e *env) repo(owner, name string, private bool) int64 {
	e.t.Helper()
	id := e.gh.AddRepo(owner, name, private)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO repositories (id, name, description, is_private) VALUES ($1, $2, '', $3)`, id, owner+"/"+name, private); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// noInstallationAuthority fails if any issue write carried something other than a user token.
func (e *env) noInstallationAuthority() {
	e.t.Helper()
	for _, tok := range e.gh.WriteTokens() {
		if !strings.HasPrefix(tok, "ghu_") {
			e.t.Fatalf("issue write used non-user authority %q", tok)
		}
	}
	for _, path := range []string{"/app/installations", "/installation/repositories"} {
		if n := e.gh.Hits(path); n != 0 {
			e.t.Fatalf("installation authority consulted: %s hit %d times", path, n)
		}
	}
}

type user struct {
	e      *env
	client *http.Client
	csrf   string
	id     int64
	login  string
}

var noRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (e *env) signIn(login string) *user {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	u := &user{e: e, login: login, client: &http.Client{Jar: jar, CheckRedirect: noRedirect}}
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
	if r := u.do("GET", cb.Path+"?"+cb.RawQuery, "", nil); r.StatusCode != 302 {
		e.t.Fatalf("sign in as %s: %d", login, r.StatusCode)
	}
	var me struct{ CSRFToken string }
	if err := json.Unmarshal(u.body(u.do("GET", "/auth/me", "", nil)), &me); err != nil {
		e.t.Fatal(err)
	}
	u.csrf = me.CSRFToken
	if err := e.pool.QueryRow(context.Background(), "SELECT id FROM users WHERE login = $1", login).Scan(&u.id); err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (u *user) do(method, path, body string, headers map[string]string) *http.Response {
	u.e.t.Helper()
	req, _ := http.NewRequest(method, u.e.srv.URL+path, strings.NewReader(body))
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

func (u *user) githubID() int64 {
	var id int64
	if err := u.e.pool.QueryRow(context.Background(), "SELECT github_id FROM users WHERE id = $1", u.id).Scan(&id); err != nil {
		u.e.t.Fatal(err)
	}
	return id
}

func (u *user) body(resp *http.Response) []byte { b, _ := io.ReadAll(resp.Body); return b }

// send is a well-formed state-changing request: same origin, CSRF token, and (when key != "") an idempotency key.
func (u *user) send(path, key, body string) (*http.Response, operation) {
	u.e.t.Helper()
	headers := map[string]string{"Origin": appURL, "X-CSRF-Token": u.csrf, "Content-Type": "application/json"}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	resp := u.do("POST", path, body, headers)
	return resp, decodeOp(u.e.t, resp)
}

func (u *user) get(path string) (*http.Response, operation) {
	u.e.t.Helper()
	resp := u.do("GET", path, "", nil)
	return resp, decodeOp(u.e.t, resp)
}

type issueView struct {
	Number    int64    `json:"number"`
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

type operation struct {
	ID          int64           `json:"id"`
	Kind        string          `json:"kind"`
	State       string          `json:"state"`
	IssueNumber *int64          `json:"issueNumber"`
	Draft       json.RawMessage `json:"draft"`
	Actor       string          `json:"actor"`
	SearchURL   string          `json:"searchUrl"`
	Outcome     struct {
		Issue     *issueView `json:"issue"`
		Unapplied []string   `json:"unapplied"`
		Error     string     `json:"error"`
	} `json:"outcome"`
}

func decodeOp(t *testing.T, resp *http.Response) operation {
	t.Helper()
	var op operation
	b, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(b, &op); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
	}
	return op
}

func create(repoID int64) string { return fmt.Sprintf("/api/repositories/%d/issues", repoID) }
func changes(repoID, number int64) string {
	return fmt.Sprintf("/api/repositories/%d/issues/%d/updates", repoID, number)
}
func opPath(id int64) string { return fmt.Sprintf("/api/write-operations/%d", id) }

func names[T any](items []T, name func(T) string) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, name(it))
	}
	slices.Sort(out)
	return out
}

func labelNames(issue fake.IssueFixture) []string {
	return names(issue.Labels, func(l fake.IssueLabel) string { return l.Name })
}
func assigneeNames(issue fake.IssueFixture) []string {
	return names(issue.Assignees, func(a fake.IssueActor) string { return a.Login })
}

func seedIssue(number int64, author string, title string) fake.IssueFixture {
	now := time.Now().UTC().Add(-time.Hour)
	return fake.IssueFixture{ID: 900000 + number, Number: number, Title: title, State: "open", HTMLURL: fmt.Sprintf("https://github.com/x/y/issues/%d", number),
		CreatedAt: now, UpdatedAt: now, User: fake.IssueActor{ID: 5, Login: author, Type: "User"}}
}

// Covers AE4: an eligible teammate is on the returned GitHub issue and in the local projection.
func TestCreateAssignsEligibleTeammate(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetPermission("bob", repo, "write")

	resp, op := alice.send(create(repo), "create-1", `{"title":"Ship it","body":"details","labels":["task"],"assignees":["bob"]}`)
	if resp.StatusCode != http.StatusCreated || op.State != "succeeded" || op.Kind != "create" || op.IssueNumber == nil || op.Actor != "alice" {
		t.Fatalf("status=%d op=%+v body=%s", resp.StatusCode, op, alice.body(resp))
	}
	if op.Outcome.Issue == nil || !slices.Equal(op.Outcome.Issue.Assignees, []string{"bob"}) || !slices.Equal(op.Outcome.Issue.Labels, []string{"task"}) {
		t.Fatalf("outcome lacks confirmed assignee: %+v", op.Outcome)
	}
	remote := e.gh.Issues(repo)
	if len(remote) != 1 || remote[0].User.Login != "alice" || !slices.Equal(assigneeNames(remote[0]), []string{"bob"}) {
		t.Fatalf("remote issues: %+v", remote)
	}
	if n := e.count(`SELECT count(*) FROM activity_issues i JOIN activity_issue_assignees a ON a.issue_id = i.source_id
		WHERE i.repository_id = $1 AND i.number = $2 AND i.author_login = 'alice' AND a.login = 'bob'`, repo, *op.IssueNumber); n != 1 {
		t.Fatalf("local projection lacks the created issue with its assignee: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM activity_issue_labels l JOIN activity_issues i ON i.source_id = l.issue_id WHERE i.repository_id = $1 AND l.name = 'task'`, repo); n != 1 {
		t.Fatalf("local projection lacks the label: %d", n)
	}
	e.noInstallationAuthority()
}

// Covers AE1: forged commands and a permission lost between render and submit fail, without broader authority.
func TestForgedAndStaleCommandsAreDenied(t *testing.T) {
	e := setup(t)
	e.signIn("alice")
	carol := e.signIn("carol")
	repo := e.repo("alice", "app", false)
	e.gh.SetIssues(repo, seedIssue(1, "alice", "Existing"))
	e.gh.SetPermission("carol", repo, "triage")
	body := `{"labels":{"add":["triaged"]}}`

	noCSRF := carol.do("POST", changes(repo, 1), body, map[string]string{"Origin": appURL, "Idempotency-Key": "forged-1", "Content-Type": "application/json"})
	crossSite := carol.do("POST", changes(repo, 1), body, map[string]string{"Origin": "https://evil.test", "X-CSRF-Token": carol.csrf, "Idempotency-Key": "forged-2", "Content-Type": "application/json"})
	if noCSRF.StatusCode != http.StatusForbidden || crossSite.StatusCode != http.StatusForbidden {
		t.Fatalf("forged requests: no csrf=%d cross-site=%d", noCSRF.StatusCode, crossSite.StatusCode)
	}

	// Render: with triage carol may manage issues, but labels or assignees on a new issue need push.
	var caps struct{ CanCreateIssue, CanCreateIssueWithMetadata, CanManageIssues bool }
	if resp := carol.do("GET", fmt.Sprintf("/api/repositories/%d/issue-capabilities", repo), "", nil); resp.StatusCode != 200 || json.Unmarshal(carol.body(resp), &caps) != nil || !caps.CanManageIssues || caps.CanCreateIssueWithMetadata || !caps.CanCreateIssue {
		t.Fatalf("capabilities at render: %d %+v", resp.StatusCode, caps)
	}
	if resp, _ := carol.send(create(repo), "triage-create", `{"title":"t","labels":["bug"]}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create-time labels with triage only: %d", resp.StatusCode)
	}
	// Submit after GitHub lowered her to read.
	e.gh.SetPermission("carol", repo, "read")
	if resp, _ := carol.send(changes(repo, 1), "stale-1", body); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stale permission submit: %d %s", resp.StatusCode, carol.body(resp))
	}
	if resp, _ := carol.send(create(repo), "stale-2", `{"title":"t","assignees":["alice"]}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("metadata create without push: %d", resp.StatusCode)
	}
	if resp := carol.do("GET", fmt.Sprintf("/api/repositories/%d/issue-capabilities", repo), "", nil); json.Unmarshal(carol.body(resp), &caps) != nil || caps.CanManageIssues || caps.CanCreateIssueWithMetadata || !caps.CanCreateIssue {
		t.Fatalf("capabilities after downgrade: %+v", caps)
	}
	if n := e.count(`SELECT count(*) FROM write_operations`); n != 0 {
		t.Fatalf("denied commands recorded %d operations", n)
	}
	if got := labelNames(e.gh.Issues(repo)[0]); len(got) != 0 {
		t.Fatalf("denied change reached GitHub: %v", got)
	}
	// A reader may still open a plain issue: that is GitHub's own per-action rule.
	if resp, op := carol.send(create(repo), "plain-1", `{"title":"Question","body":"?"}`); resp.StatusCode != http.StatusCreated || op.State != "succeeded" {
		t.Fatalf("plain create by reader: %d %+v", resp.StatusCode, op)
	}
	e.noInstallationAuthority()
}

// GitHub answering 201 while dropping an assignee is never a successful assignment.
func TestSilentAssigneeOmissionIsNotSuccess(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetPermission("bob", repo, "write")
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{DropAssignees: true})

	resp, op := alice.send(create(repo), "drop-1", `{"title":"Ship it","assignees":["bob"]}`)
	if resp.StatusCode != http.StatusCreated || op.State != "failed" || op.IssueNumber == nil || !slices.Contains(op.Outcome.Unapplied, "add assignee bob") {
		t.Fatalf("status=%d op=%+v", resp.StatusCode, op)
	}
	if op.Outcome.Issue == nil || len(op.Outcome.Issue.Assignees) != 0 {
		t.Fatalf("outcome must show the issue as GitHub returned it: %+v", op.Outcome.Issue)
	}
	// The issue exists: resubmitting the same draft returns this operation rather than a second issue.
	if resp, again := alice.send(create(repo), "drop-1", `{"title":"Ship it","assignees":["bob"]}`); resp.StatusCode != 200 || again.ID != op.ID {
		t.Fatalf("resubmit: %d %+v", resp.StatusCode, again)
	}
	if n := len(e.gh.Issues(repo)); n != 1 {
		t.Fatalf("issues on GitHub: %d", n)
	}
}

func TestIdempotencyKeys(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	first, op := alice.send(create(repo), "same-key", `{"title":"One"}`)
	again, dup := alice.send(create(repo), "same-key", `{"title":"One"}`)
	if first.StatusCode != http.StatusCreated || again.StatusCode != http.StatusOK || dup.ID != op.ID || dup.State != "succeeded" {
		t.Fatalf("first=%d again=%d op=%+v dup=%+v", first.StatusCode, again.StatusCode, op, dup)
	}
	if resp, _ := alice.send(create(repo), "same-key", `{"title":"Two"}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("changed payload with same key: %d", resp.StatusCode)
	}
	if resp, _ := alice.send(create(repo), "", `{"title":"No key"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing key: %d", resp.StatusCode)
	}
	if n := len(e.gh.Issues(repo)); n != 1 {
		t.Fatalf("issues on GitHub: %d", n)
	}
	// Keys belong to a user: another user's identical key is a separate operation.
	bob := e.signIn("bob")
	if resp, other := bob.send(create(repo), "same-key", `{"title":"One"}`); resp.StatusCode != http.StatusCreated || other.ID == op.ID {
		t.Fatalf("other user's key: %d %+v", resp.StatusCode, other)
	}
}

// A timeout after the remote create stays unknown, survives a restart, never resends, and resolves only by a verified recheck.
func TestAmbiguousCreateStaysUnknown(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	// Each candidate fails exactly one check: content (#1), author (#2), age (#4, and #3, an identical issue opened
	// 30 seconds before the send); #99 does not exist.
	wrongContent, wrongAuthor, beforeSend, tooOld := seedIssue(1, "alice", "Different title"), seedIssue(2, "mallory", "Ship it"), seedIssue(3, "alice", "Ship it"), seedIssue(4, "alice", "Ship it")
	wrongContent.Body, wrongAuthor.Body, beforeSend.Body, tooOld.Body = "details", "details", "details", "details"
	wrongContent.User.ID, beforeSend.User.ID, tooOld.User.ID = alice.githubID(), alice.githubID(), alice.githubID()
	wrongContent.CreatedAt, wrongAuthor.CreatedAt = time.Now(), time.Now()
	beforeSend.CreatedAt = time.Now().Add(-30 * time.Second)
	e.gh.SetIssues(repo, wrongContent, wrongAuthor, beforeSend, tooOld)
	e.client.HTTP.Timeout = 300 * time.Millisecond
	var stateAtSend string
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{Delay: time.Second, Before: func() {
		_ = e.pool.QueryRow(context.Background(), `SELECT state FROM write_operations`).Scan(&stateAtSend)
	}})

	resp, op := alice.send(create(repo), "slow-1", `{"title":"Ship it","body":"details"}`)
	if resp.StatusCode != http.StatusAccepted || op.State != "unknown" || op.IssueNumber != nil || string(op.Draft) == "null" || op.SearchURL == "" {
		t.Fatalf("status=%d op=%+v", resp.StatusCode, op)
	}
	if stateAtSend != "unknown" {
		t.Fatalf("a process death mid-send would leave %q, not unknown", stateAtSend)
	}
	time.Sleep(time.Second) // let the fake finish creating
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{})
	e.client.HTTP.Timeout = 5 * time.Second
	if n := len(e.gh.Issues(repo)); n != 5 {
		t.Fatalf("issues on GitHub: %d", n)
	}
	if resp, again := alice.send(create(repo), "slow-1", `{"title":"Ship it","body":"details"}`); resp.StatusCode != 200 || again.ID != op.ID || again.State != "unknown" {
		t.Fatalf("same key resubmit: %d %+v", resp.StatusCode, again)
	}
	if resp := alice.do("POST", create(repo), `{"title":"Ship it","body":"details"}`, map[string]string{"Origin": appURL, "X-CSRF-Token": alice.csrf, "Idempotency-Key": "slow-2"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("fresh key, same unresolved content: %d", resp.StatusCode)
	}
	if n := len(e.gh.Issues(repo)); n != 5 {
		t.Fatalf("a resubmission created another issue: %d", n)
	}

	// Restart: a new service over the same database still reports unknown and sends nothing.
	restarted := &issues.Service{Pool: e.pool, Queries: db.New(e.pool), GitHub: e.client, Access: e.issues.Access, GitHubURL: e.issues.GitHubURL}
	api := http.NewServeMux()
	restarted.Register(api)
	e.srv.Config.Handler = (&auth.Handler{Service: e.issues.Access.TokenSource.(*auth.Service), AppURL: appURL}).Protect(api)
	if resp, got := alice.get(opPath(op.ID)); resp.StatusCode != 200 || got.State != "unknown" {
		t.Fatalf("after restart: %d %+v", resp.StatusCode, got)
	}

	recheck := func(number int64) (*http.Response, operation) {
		return alice.send(opPath(op.ID)+"/recheck", "", fmt.Sprintf(`{"issueNumber":%d}`, number))
	}
	for _, wrong := range []int64{1, 2, 3, 4, 99} {
		resp, got := recheck(wrong)
		if resp.StatusCode != http.StatusConflict || got.State != "unknown" || got.IssueNumber != nil {
			t.Fatalf("recheck #%d: %d %+v", wrong, resp.StatusCode, got)
		}
	}
	if n := e.count(`SELECT count(*) FROM write_operations WHERE state = 'unknown' AND draft IS NOT NULL AND issue_number IS NULL`); n != 1 {
		t.Fatalf("mismatch must keep the draft and unknown state: %d", n)
	}
	resp, got := recheck(5)
	if resp.StatusCode != 200 || got.State != "succeeded" || got.IssueNumber == nil || *got.IssueNumber != 5 {
		t.Fatalf("recheck #5: %d %+v", resp.StatusCode, got)
	}
	if n := e.count(`SELECT count(*) FROM activity_issues WHERE repository_id = $1 AND number = 5`, repo); n != 1 {
		t.Fatalf("linked issue not projected: %d", n)
	}
	if n := len(e.gh.Issues(repo)); n != 5 {
		t.Fatalf("issues on GitHub: %d", n)
	}
}

// Classification of definite and ambiguous upstream answers to a create.
func TestCreateErrorClassification(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	for i, c := range []struct {
		fault fake.IssueWriteFault
		state string
	}{
		{fake.IssueWriteFault{Status: http.StatusUnprocessableEntity}, "failed"},
		{fake.IssueWriteFault{Status: http.StatusForbidden}, "failed"},
		{fake.IssueWriteFault{Status: http.StatusBadGateway}, "unknown"},
		{fake.IssueWriteFault{Status: http.StatusBadGateway, CreateDespiteStatus: true}, "unknown"},
	} {
		e.gh.SetIssueWriteFault(c.fault)
		_, op := alice.send(create(repo), fmt.Sprintf("class-%d", i), fmt.Sprintf(`{"title":"Case %d"}`, i))
		if op.State != c.state || op.Outcome.Error == "" {
			t.Fatalf("fault %+v: %+v", c.fault, op)
		}
	}
	if n := len(e.gh.Issues(repo)); n != 1 {
		t.Fatalf("only the applied gateway failure created an issue: %d", n)
	}
}

// Add/remove intent never replaces values someone else changed; the audit row names the actor and holds no secrets.
func TestAddRemovePreservesConcurrentValues(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	for _, login := range []string{"bob", "carol", "erin"} {
		e.gh.SetPermission(login, repo, "write")
	}
	seed := seedIssue(1, "alice", "Existing")
	seed.Labels = []fake.IssueLabel{{ID: 1, Name: "bug"}, {ID: 2, Name: "external"}}
	seed.Assignees = []fake.IssueActor{{ID: 11, Login: "bob", Type: "User"}}
	e.gh.SetIssues(repo, seed)
	// Someone else edits the issue after alice rendered it.
	seed.Labels = append(seed.Labels, fake.IssueLabel{ID: 3, Name: "urgent"})
	seed.Assignees = append(seed.Assignees, fake.IssueActor{ID: 12, Login: "erin", Type: "User"})
	e.gh.SetIssues(repo, seed)

	resp, op := alice.send(changes(repo, 1), "change-1", `{"labels":{"add":["triaged"],"remove":["bug"]},"assignees":{"add":["carol"],"remove":["bob"]}}`)
	if resp.StatusCode != http.StatusCreated || op.State != "succeeded" || op.Kind != "update" {
		t.Fatalf("status=%d op=%+v body=%s", resp.StatusCode, op, alice.body(resp))
	}
	remote := e.gh.Issues(repo)[0]
	if got := labelNames(remote); !slices.Equal(got, []string{"external", "triaged", "urgent"}) {
		t.Fatalf("labels: %v", got)
	}
	if got := assigneeNames(remote); !slices.Equal(got, []string{"carol", "erin"}) {
		t.Fatalf("assignees: %v", got)
	}
	if resp, closed := alice.send(changes(repo, 1), "close-1", `{"state":"closed"}`); resp.StatusCode != http.StatusCreated || closed.State != "succeeded" || closed.Outcome.Issue.State != "closed" {
		t.Fatalf("close: %d %+v", resp.StatusCode, closed)
	}
	if n := e.count(`SELECT count(*) FROM activity_issues WHERE repository_id = $1 AND number = 1 AND state = 'closed'`, repo); n != 1 {
		t.Fatalf("projection not updated by the confirmed close: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM write_operations WHERE user_id = $1 AND state = 'succeeded' AND outcome ? 'issue'`, alice.id); n != 2 {
		t.Fatalf("audit rows for the actor: %d", n)
	}
	var rows string
	if err := e.pool.QueryRow(context.Background(), `SELECT json_agg(o)::text FROM write_operations o`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ghu_", "ghr_", "installation_"} {
		if strings.Contains(rows, secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("secret %q in audit rows or logs", secret)
		}
	}
	e.noInstallationAuthority()
}

// Another viewer cannot see an actor's operations; losing private access wipes only the affected actor's drafts.
func TestOperationsArePrivateToActor(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	bob := e.signIn("bob")
	public := e.repo("alice", "app", false)
	_, op := alice.send(create(public), "mine-1", `{"title":"Mine","body":"secret plan"}`)
	if op.ID == 0 {
		t.Fatal("no operation")
	}
	if resp, _ := bob.get(opPath(op.ID)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("guessed id: %d", resp.StatusCode)
	}
	if resp, _ := bob.send(opPath(op.ID)+"/recheck", "", `{"issueNumber":1}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("guessed recheck: %d", resp.StatusCode)
	}
	resp := bob.do("GET", fmt.Sprintf("/api/repositories/%d/write-operations", public), "", nil)
	if b := bob.body(resp); resp.StatusCode != 200 || strings.Contains(string(b), "secret plan") || strings.Contains(string(b), fmt.Sprintf(`"id":%d`, op.ID)) {
		t.Fatalf("listing leaked another actor's operation: %d %s", resp.StatusCode, b)
	}
	if resp, own := alice.get(opPath(op.ID)); resp.StatusCode != 200 || own.ID != op.ID || !strings.Contains(string(own.Draft), "secret plan") {
		t.Fatalf("own read: %d %+v", resp.StatusCode, own)
	}

	// Private repository: alice reaches it through her installation; bob's operation there is a fixture, since the fake
	// models private reads as owner-only.
	private := e.repo("alice", "vault", true)
	e.gh.Install("alice", 7001, private)
	resp, mine := alice.send(create(private), "vault-1", `{"title":"Quarterly secret","body":"private draft","labels":["confidential"]}`)
	if resp.StatusCode != http.StatusCreated || mine.State != "succeeded" {
		t.Fatalf("private create: %d %+v %s", resp.StatusCode, mine, alice.body(resp))
	}
	fixture := func(userID int64, key string) {
		if _, err := e.pool.Exec(context.Background(), `INSERT INTO write_operations (user_id, repository_id, idempotency_key, kind, request_hash, draft, state) VALUES ($1, $2, $3, 'create', '\x00', '{"title":"x","body":"kept"}', 'failed')`, userID, private, key); err != nil {
			t.Fatal(err)
		}
	}
	fixture(bob.id, "bob-1")
	drafts := func(userID int64) int {
		return e.count(`SELECT count(*) FROM write_operations WHERE user_id = $1 AND repository_id = $2 AND draft IS NOT NULL`, userID, private)
	}
	invalidate := func(scope accessfence.Scope, id int64) {
		if err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) error { return accessfence.InvalidateAccess(context.Background(), tx, scope, id) }); err != nil {
			t.Fatal(err)
		}
	}
	// An installation event names no repositories: the private-repository cleanup job, which knows from the app
	// inventory which installations still cover the repository, wipes drafts (see the ingestion cleanup tests). Here
	// alice reaches the repository through a second installation too, so nothing may be wiped yet.
	e.gh.Install("alice", 7002, private)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO installations (user_id, installation_id, account_login) VALUES ($1, 7002, 'alice')`, alice.id); err != nil {
		t.Fatal(err)
	}
	invalidate(accessfence.ScopeInstallation, 7001)
	if drafts(alice.id) != 1 || drafts(bob.id) != 1 {
		t.Fatalf("installation revocation: alice=%d bob=%d", drafts(alice.id), drafts(bob.id))
	}
	fixture(alice.id, "alice-2")
	invalidate(accessfence.ScopeUser, alice.id)
	if drafts(alice.id) != 0 || drafts(bob.id) != 1 {
		t.Fatalf("user revocation: alice=%d bob=%d", drafts(alice.id), drafts(bob.id))
	}
	// The wipe also redacts the outcome: GitHub's issue number, link and state stay; its text and names go.
	var outcome string
	if err := e.pool.QueryRow(context.Background(), `SELECT outcome::text FROM write_operations WHERE id = $1`, mine.ID).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outcome, "Quarterly") || strings.Contains(outcome, "confidential") || !strings.Contains(outcome, `"number": 1`) {
		t.Fatalf("outcome not redacted: %s", outcome)
	}
	e.gh.Uninstall(7002)
	// Access lost with no webhook: the next read denies and wipes the actor's draft.
	fixture(alice.id, "alice-3")
	e.gh.Uninstall(7001)
	if resp, _ := alice.get(opPath(mine.ID)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("read after access loss: %d", resp.StatusCode)
	}
	if drafts(alice.id) != 0 || drafts(bob.id) != 1 {
		t.Fatalf("denied read: alice=%d bob=%d", drafts(alice.id), drafts(bob.id))
	}
}

// Commands never trust the cached 30-second listing: access removed on GitHub after a cached read denies the write.
func TestCommandsBypassCachedAccess(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "vault", true)
	e.gh.Install("alice", 7001, repo)
	e.issues.Access.TTL = time.Hour
	if resp := alice.do("GET", fmt.Sprintf("/api/repositories/%d/issue-capabilities", repo), "", nil); resp.StatusCode != 200 {
		t.Fatalf("render: %d", resp.StatusCode)
	}
	e.gh.Uninstall(7001) // nothing local learns of this, so the cached listing still holds
	if resp, _ := alice.send(create(repo), "after-uninstall", `{"title":"Private"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create after access loss: %d", resp.StatusCode)
	}
	if n := e.count(`SELECT count(*) FROM write_operations`); n != 0 || len(e.gh.WriteTokens()) != 0 {
		t.Fatalf("recorded %d operations, sent %d writes", n, len(e.gh.WriteTokens()))
	}
}

func TestInvalidInputIsRejected(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetIssues(repo, seedIssue(1, "alice", "Existing"))
	tooMany := `["a","b","c","d","e","f","g","h","i","j","k"]`
	for _, c := range []struct{ path, body string }{
		{create(repo), `{"title":"x","extra":1}`},
		{create(repo), `{"title":""}`},
		{create(repo), `{"title":"   "}`},
		{create(repo), `{"title":"` + strings.Repeat("x", 257) + `"}`},
		{create(repo), `{"title":"x","body":"` + strings.Repeat("x", 65537) + `"}`},
		{create(repo), `{"title":"x","assignees":` + tooMany + `}`},
		{create(repo), `{"title":"x","labels":` + tooMany + `}`},
		{create(repo), `{"title":"x","assignees":["bad login!"]}`},
		{create(repo), `{"title":"x","assignees":["bob","bob"]}`},
		{create(repo), `{"title":"x"} {"title":"y"}`},
		{create(repo), `not json`},
		{changes(repo, 1), `{}`},
		{changes(repo, 1), `{"labels":{"add":["a"],"remove":["a"]}}`},
		{changes(repo, 1), `{"state":"merged"}`},
		{changes(repo, 1), `{"labels":{"add":[""]}}`},
		{changes(repo, 1), `{"assignees":{"add":["x"],"replace":["y"]}}`},
	} {
		if resp, _ := alice.send(c.path, "bad-input", c.body); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s %.80s: %d", c.path, c.body, resp.StatusCode)
		}
	}
	if resp, _ := alice.send(create(repo), "bad key with spaces", `{"title":"x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}
	if resp, _ := alice.send(create(repo), "ineligible-1", `{"title":"x","assignees":["zed"]}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("ineligible assignee: %d", resp.StatusCode)
	}
	if resp, _ := alice.send(create(999999), "unknown-repo", `{"title":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown repository: %d", resp.StatusCode)
	}
	if n := e.count(`SELECT count(*) FROM write_operations`); n != 0 {
		t.Fatalf("rejected input recorded %d operations", n)
	}
	if n := len(e.gh.WriteTokens()); n != 0 {
		t.Fatalf("rejected input reached GitHub writes: %d", n)
	}
}

// Two overlapping creates of the same content under different keys reach GitHub once: the other answers 409.
func TestOverlappingFreshKeyCreatesSendOnce(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetPermission("bob", repo, "write")
	arrived, release, sent := make(chan struct{}, 2), make(chan struct{}), make(chan struct{})
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		wait := func(ch chan struct{}, d time.Duration) {
			select {
			case <-ch:
			case <-time.After(d):
			}
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/assignees/bob"): // the last pre-flight read before the operation is recorded
			arrived <- struct{}{}
			wait(release, 5*time.Second)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"): // keep the first create unresolved
			wait(sent, 2*time.Second)
		}
		next.ServeHTTP(w, r)
	})
	statuses := make(chan int, 2)
	for _, key := range []string{"race-1", "race-2"} {
		go func() {
			resp, _ := alice.send(create(repo), key, `{"title":"Ship it","body":"details","assignees":["bob"]}`)
			statuses <- resp.StatusCode
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("both submissions must pass their pre-flight checks before either records")
		}
	}
	close(release)
	first := <-statuses // the refused one, while the other is still being sent (or, unfixed, both after the hold)
	close(sent)
	got := []int{first, <-statuses}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusCreated, http.StatusConflict}) {
		t.Fatalf("statuses: %v", got)
	}
	if n := len(e.gh.Issues(repo)); n != 1 {
		t.Fatalf("issues on GitHub: %d", n)
	}
}

// A revocation committing while a confirmed outcome is being recorded neither deadlocks nor loses the outcome.
func TestFinishAndRevocationDoNotDeadlock(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "vault", true)
	e.gh.Install("alice", 7001, repo)
	ctx := context.Background()
	revoked := make(chan error, 1)
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{Before: func() {
		// Hold the fence rows the way InvalidateAccess does, let the server record its outcome, then wipe drafts.
		tx, err := e.pool.Begin(ctx)
		if err == nil {
			err = accessfence.BumpAccess(ctx, tx, accessfence.ScopeUser, alice.id)
		}
		if err != nil {
			revoked <- err
			return
		}
		go func() {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				var waiting int
				_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
				if waiting > 0 {
					break
				}
			}
			err := accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeUser, alice.id)
			if err == nil {
				err = tx.Commit(ctx)
			}
			_ = tx.Rollback(ctx)
			revoked <- err
		}()
	}})
	resp, _ := alice.send(create(repo), "deadlock-1", `{"title":"Private","body":"b"}`)
	if err := <-revoked; err != nil {
		t.Fatalf("revocation: %v", err)
	}
	// Access moved during the write: the recorded operation is withheld from the response.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", resp.StatusCode, alice.body(resp))
	}
	if n := e.count(`SELECT count(*) FROM write_operations WHERE state = 'succeeded' AND issue_number IS NOT NULL AND draft IS NULL
		AND outcome ? 'redacted' AND outcome::text NOT LIKE '%Private%'`); n != 1 {
		t.Fatalf("an outcome recorded after the wipe must be kept, redacted: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM activity_issues WHERE repository_id = $1`, repo); n != 0 {
		t.Fatalf("projected under moved access: %d", n)
	}
}

// An installation revocation committing mid-write wipes no draft by itself, so the response must not carry it: the
// operation is withheld, on the submit and on a replay of its key.
func TestSubmitWithholdsOperationWhenAccessMoved(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "vault", true)
	e.gh.Install("alice", 7001, repo)
	var once sync.Once
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{Before: func() {
		once.Do(func() {
			if err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) error {
				return accessfence.InvalidateAccess(context.Background(), tx, accessfence.ScopeInstallation, 7001)
			}); err != nil {
				t.Error(err)
			}
		})
	}})
	resp, _ := alice.send(create(repo), "moved-1", `{"title":"Secret title","body":"b"}`)
	if b := alice.body(resp); resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(b), "Secret title") {
		t.Fatalf("submit across an installation revocation: %d %s", resp.StatusCode, b)
	}
	if n := e.count(`SELECT count(*) FROM write_operations WHERE state = 'succeeded'`); n != 1 {
		t.Fatalf("outcome not recorded: %d", n)
	}
}

// A revocation that commits while access is being checked is never absorbed into the command's fence snapshot.
func TestRevocationDuringAccessCheckIsNotAbsorbed(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("bob", "lib", false) // public and not alice's: the check ends by asking GitHub for the repository
	var once sync.Once
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		if r.URL.Path == fmt.Sprintf("/repositories/%d", repo) {
			once.Do(func() { // GitHub answered "public"; the repository is privatized before the answer arrives
				e.gh.SetPrivate(repo, true)
				if err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) error {
					return accessfence.InvalidateAccess(context.Background(), tx, accessfence.ScopeRepository, repo)
				}); err != nil {
					t.Error(err)
				}
			})
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
	if resp, _ := alice.send(create(repo), "privatized-1", `{"title":"t"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create across a revocation: %d %s", resp.StatusCode, alice.body(resp))
	}
	if n := e.count(`SELECT count(*) FROM write_operations`); n != 0 || len(e.gh.WriteTokens()) != 0 {
		t.Fatalf("recorded %d operations, sent %d writes", n, len(e.gh.WriteTokens()))
	}
}

// Operation reads check access fresh: a cached listing never serves a draft after access ended.
func TestOperationReadsBypassCachedAccess(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "vault", true)
	e.gh.Install("alice", 7001, repo)
	e.issues.Access.TTL = time.Hour
	_, op := alice.send(create(repo), "cached-1", `{"title":"Private","body":"draft"}`)
	if op.State != "succeeded" {
		t.Fatalf("create: %+v", op)
	}
	e.gh.Uninstall(7001) // nothing local learns of this, so the cached listing still holds
	if resp, _ := alice.get(opPath(op.ID)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("read after access loss: %d", resp.StatusCode)
	}
	if resp := alice.do("GET", fmt.Sprintf("/api/repositories/%d/write-operations", repo), "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("list after access loss: %d", resp.StatusCode)
	}
}

// GitHub lets an issue's author close and reopen it without triage; labels and assignees still need triage.
func TestAuthorClosesOwnIssueWithoutTriage(t *testing.T) {
	e := setup(t)
	e.signIn("alice")
	carol := e.signIn("carol")
	repo := e.repo("alice", "app", false) // carol can only read
	mine, theirs := seedIssue(1, "carol", "Mine"), seedIssue(2, "alice", "Theirs")
	mine.User.ID = carol.githubID()
	e.gh.SetIssues(repo, mine, theirs)
	if resp, op := carol.send(changes(repo, 1), "close-own", `{"state":"closed"}`); resp.StatusCode != http.StatusCreated || op.State != "succeeded" {
		t.Fatalf("author close: %d %+v %s", resp.StatusCode, op, carol.body(resp))
	}
	if resp, op := carol.send(changes(repo, 1), "reopen-own", `{"state":"open"}`); resp.StatusCode != http.StatusCreated || op.State != "succeeded" {
		t.Fatalf("author reopen: %d %+v", resp.StatusCode, op)
	}
	for key, c := range map[string]struct {
		number int64
		body   string
	}{"label-own": {1, `{"labels":{"add":["bug"]},"state":"closed"}`}, "close-theirs": {2, `{"state":"closed"}`}} {
		if resp, _ := carol.send(changes(repo, c.number), key, c.body); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %d", key, resp.StatusCode)
		}
	}
}

// holdLabelWrites delays every label add until release closes, then applies it: a write still in flight.
func holdLabelWrites(e *env) (release func()) {
	ch := make(chan struct{})
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			select {
			case <-ch:
			case <-time.After(5 * time.Second):
			}
		}
		next.ServeHTTP(w, r)
	})
	return func() { close(ch) }
}

func (e *env) waitForLabel(repo int64, label string) {
	e.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if slices.Contains(labelNames(e.gh.Issues(repo)[0]), label) {
			return
		}
	}
	e.t.Fatalf("label %q never applied", label)
}

// An ambiguous change whose effect is not visible yet stays unknown; a recheck confirms it once it shows, and never
// fails it while the original request may still land.
func TestAmbiguousUpdateResolvesOnlyWhenConfirmed(t *testing.T) {
	t.Run("timed out", func(t *testing.T) {
		e := setup(t)
		alice := e.signIn("alice")
		repo := e.repo("alice", "app", false)
		e.gh.SetIssues(repo, seedIssue(1, "alice", "Existing"))
		release := holdLabelWrites(e)
		e.client.HTTP.Timeout = 300 * time.Millisecond
		resp, op := alice.send(changes(repo, 1), "slow-change", `{"labels":{"add":["triaged"]}}`)
		if resp.StatusCode != http.StatusAccepted || op.State != "unknown" {
			t.Fatalf("ambiguous change: %d %+v", resp.StatusCode, op)
		}
		e.client.HTTP.Timeout = 5 * time.Second
		if resp, got := alice.send(opPath(op.ID)+"/recheck", "", `{}`); resp.StatusCode != http.StatusConflict || got.State != "unknown" {
			t.Fatalf("recheck before the write lands: %d %+v", resp.StatusCode, got)
		}
		release()
		e.waitForLabel(repo, "triaged")
		if resp, got := alice.send(opPath(op.ID)+"/recheck", "", `{}`); resp.StatusCode != 200 || got.State != "succeeded" {
			t.Fatalf("recheck after the write landed: %d %+v", resp.StatusCode, got)
		}
	})
	t.Run("recheck during send", func(t *testing.T) {
		e := setup(t)
		alice := e.signIn("alice")
		repo := e.repo("alice", "app", false)
		e.gh.SetIssues(repo, seedIssue(1, "alice", "Existing"))
		release := holdLabelWrites(e)
		done := make(chan operation, 1)
		go func() {
			_, op := alice.send(changes(repo, 1), "in-flight", `{"labels":{"add":["triaged"]}}`)
			done <- op
		}()
		var id int64
		for deadline := time.Now().Add(5 * time.Second); id == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			_ = e.pool.QueryRow(context.Background(), `SELECT id FROM write_operations WHERE state = 'unknown'`).Scan(&id)
		}
		if resp, got := alice.send(opPath(id)+"/recheck", "", `{}`); resp.StatusCode != http.StatusConflict || got.State != "unknown" {
			t.Fatalf("recheck while the send is in flight: %d %+v", resp.StatusCode, got)
		}
		release()
		if op := <-done; op.State != "succeeded" {
			t.Fatalf("the original send's confirmed outcome was lost: %+v", op)
		}
	})
}

// A create rechecked after its draft was wiped links the issue but cannot claim the requested assignment.
func TestRecheckAfterWipeDoesNotClaimAssignment(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetPermission("bob", repo, "write")
	e.client.HTTP.Timeout = 300 * time.Millisecond
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{Delay: time.Second})
	_, op := alice.send(create(repo), "wiped-1", `{"title":"Assigned","body":"x","assignees":["bob"]}`)
	if op.State != "unknown" {
		t.Fatalf("create: %+v", op)
	}
	time.Sleep(time.Second)
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{})
	e.client.HTTP.Timeout = 5 * time.Second
	if _, err := e.pool.Exec(context.Background(), `UPDATE write_operations SET draft = NULL`); err != nil { // access lost and regained
		t.Fatal(err)
	}
	resp, got := alice.send(opPath(op.ID)+"/recheck", "", fmt.Sprintf(`{"issueNumber":%d}`, e.gh.Issues(repo)[0].Number))
	if resp.StatusCode != 200 || got.State != "failed" || got.IssueNumber == nil {
		t.Fatalf("recheck: %d %+v", resp.StatusCode, got)
	}
	// Recorded after the wipe, the outcome is stored redacted: the unverified request survives only as a count.
	if n := e.count(`SELECT count(*) FROM write_operations WHERE (outcome->>'unappliedCount')::int = 1`); n != 1 {
		t.Fatalf("unverified assignment not recorded: %d", n)
	}
}

// At GitHub's 10-assignee limit, a swap removes before it adds.
func TestAssigneeSwapAtLimit(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	seed := seedIssue(1, "alice", "Full")
	for i := range 10 {
		seed.Assignees = append(seed.Assignees, fake.IssueActor{ID: int64(100 + i), Login: fmt.Sprintf("u%d", i), Type: "User"})
	}
	e.gh.SetIssues(repo, seed)
	e.gh.SetPermission("carol", repo, "write")
	resp, op := alice.send(changes(repo, 1), "swap-1", `{"assignees":{"add":["carol"],"remove":["u0"]}}`)
	if resp.StatusCode != http.StatusCreated || op.State != "succeeded" {
		t.Fatalf("swap: %d %+v", resp.StatusCode, op)
	}
}

// A create recorded but never marked sent (the process stopped first, so nothing reached GitHub) does not block the
// same content forever: once stale it is failed and a fresh key proceeds. A recent pending create still blocks, with
// its real operation id.
func TestStalePendingCreateDoesNotBlockRetry(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	sum := sha256.Sum256([]byte("Stuck\x00body"))
	pending := func(key string, age time.Duration) int64 {
		var id int64
		if err := e.pool.QueryRow(context.Background(), `INSERT INTO write_operations (user_id, repository_id, idempotency_key, kind, request_hash, content_hash, draft, state, created_at)
			VALUES ($1, $2, $3, 'create', '\x00', $4, '{"title":"Stuck","body":"body"}', 'pending', now() - $5 * interval '1 second') RETURNING id`, alice.id, repo, key, sum[:], int(age.Seconds())).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	recent := pending("recent", 0)
	resp := alice.do("POST", create(repo), `{"title":"Stuck","body":"body"}`, map[string]string{"Origin": appURL, "X-CSRF-Token": alice.csrf, "Content-Type": "application/json", "Idempotency-Key": "retry-0"})
	var conflict struct{ OperationID int64 }
	if err := json.Unmarshal(alice.body(resp), &conflict); resp.StatusCode != http.StatusConflict || err != nil || conflict.OperationID != recent {
		t.Fatalf("recent pending create: %d %+v %v", resp.StatusCode, conflict, err)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM write_operations WHERE id = $1`, recent); err != nil {
		t.Fatal(err)
	}
	stale := pending("stale", 2*time.Minute)
	resp, op := alice.send(create(repo), "retry-1", `{"title":"Stuck","body":"body"}`)
	if resp.StatusCode != http.StatusCreated || op.State != "succeeded" || op.ID == stale {
		t.Fatalf("retry after a stale pending create: %d %+v %s", resp.StatusCode, op, alice.body(resp))
	}
	if n := e.count(`SELECT count(*) FROM write_operations WHERE id = $1 AND state = 'failed' AND sent_at IS NULL`, stale); n != 1 {
		t.Fatal("the stale pending create must be failed, as never sent")
	}
	if n := len(e.gh.Issues(repo)); n != 1 {
		t.Fatalf("issues on GitHub: %d", n)
	}
}

// Two changes to one issue read back within the same GitHub second: the later readback is projected last, so an older
// readback never overwrites a newer one even though their updated_at ties.
func TestLaterReadbackWinsSameSecondProjection(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetIssues(repo, seedIssue(1, "alice", "Existing"))
	second := time.Now().UTC().Truncate(time.Second)
	var aPosted, captured atomic.Bool
	readA, proceed := make(chan struct{}), make(chan struct{})
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if strings.Contains(string(body), `"a"`) {
				aPosted.Store(true)
			}
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		b := rec.Body.Bytes()
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/1") {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				m["updated_at"] = second.Format(time.RFC3339) // GitHub's one-second resolution
				b, _ = json.Marshal(m)
			}
			if aPosted.Load() && captured.CompareAndSwap(false, true) { // A's readback: fetched now, delivered late
				close(readA)
				select {
				case <-proceed:
				case <-time.After(5 * time.Second):
				}
			}
		}
		for k, v := range rec.Header() {
			if k != "Content-Length" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(b)
	})
	doneA, doneB := make(chan int, 1), make(chan int, 1)
	go func() {
		resp, _ := alice.send(changes(repo, 1), "a-1", `{"labels":{"add":["a"]}}`)
		doneA <- resp.StatusCode
	}()
	select {
	case <-readA:
	case <-time.After(5 * time.Second):
		t.Fatal("A never read back")
	}
	go func() {
		resp, _ := alice.send(changes(repo, 1), "b-1", `{"labels":{"add":["b"]}}`)
		doneB <- resp.StatusCode
	}()
	// Unserialized, B finishes now; serialized, B waits for A's projection.
	statusB := 0
	for deadline := time.Now().Add(3 * time.Second); statusB == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		select {
		case statusB = <-doneB:
		default:
			if e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event = 'advisory'`) > 0 {
				deadline = time.Time{}
			}
		}
	}
	close(proceed)
	if s := <-doneA; s != http.StatusCreated {
		t.Fatalf("A: %d", s)
	}
	if statusB == 0 {
		statusB = <-doneB
	}
	if statusB != http.StatusCreated {
		t.Fatalf("B: %d", statusB)
	}
	rows, err := e.pool.Query(context.Background(), `SELECT name FROM activity_issue_labels l JOIN activity_issues i ON i.source_id = l.issue_id WHERE i.repository_id = $1 ORDER BY name`, repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("projected labels %v (%v): an older readback overwrote a newer one", got, err)
	}
}

// A blocker that resolves between the conflicting insert and its lookup may have been sent and may have succeeded:
// the new create is refused (409 naming it), never resent.
func TestBlockerResolvingDuringConflictIsNotResent(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	ctx := context.Background()
	sum := sha256.Sum256([]byte("Stuck\x00body"))
	var blocker int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO write_operations (user_id, repository_id, idempotency_key, kind, request_hash, content_hash, draft, state, created_at)
		VALUES ($1, $2, 'blocker', 'create', '\x00', $3, '{"title":"Stuck","body":"body"}', 'pending', now() - interval '2 minutes') RETURNING id`, alice.id, repo, sum[:]).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	// Row-lock the blocker: the new insert still conflicts at once, but its stale-pending recovery waits on the lock.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM write_operations WHERE id = $1 FOR UPDATE`, blocker); err != nil {
		t.Fatal(err)
	}
	done := make(chan *http.Response, 1)
	go func() {
		done <- alice.do("POST", create(repo), `{"title":"Stuck","body":"body"}`, map[string]string{"Origin": appURL, "X-CSRF-Token": alice.csrf, "Content-Type": "application/json", "Idempotency-Key": "late-1"})
	}()
	waited := false
	for deadline := time.Now().Add(5 * time.Second); !waited && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		waited = e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) > 0
	}
	if !waited {
		t.Fatal("the conflicting create never waited on the blocker")
	}
	// The blocker was sent after all, and succeeded.
	issue := seedIssue(1, "alice", "Stuck")
	issue.Body = "body"
	e.gh.SetIssues(repo, issue)
	if _, err := tx.Exec(ctx, `UPDATE write_operations SET state = 'succeeded', sent_at = now(), issue_number = 1 WHERE id = $1`, blocker); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	resp := <-done
	var conflict struct{ OperationID int64 }
	if err := json.Unmarshal(alice.body(resp), &conflict); resp.StatusCode != http.StatusConflict || err != nil || conflict.OperationID != blocker {
		t.Fatalf("create racing a resolving blocker: %d %+v %v", resp.StatusCode, conflict, err)
	}
	if n := len(e.gh.Issues(repo)); n != 1 {
		t.Fatalf("issues on GitHub: %d", n)
	}
}

// singleConnection rewires the API onto a pool of one connection, so any request that needs a second connection
// while it holds one never completes.
func (e *env) singleConnection() {
	e.t.Helper()
	cfg := e.pool.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(pool.Close)
	q := db.New(pool)
	authSvc := &auth.Service{Pool: pool, Queries: q, GitHub: e.client, Key: bytes.Repeat([]byte{1}, 32)}
	repos := &repositories.Service{Queries: q, Pool: pool, GitHub: e.client, TokenSource: authSvc, TTL: time.Nanosecond, AppURL: appURL}
	svc := &issues.Service{Pool: pool, Queries: q, GitHub: e.client, Access: repos, GitHubURL: e.issues.GitHubURL}
	api := http.NewServeMux()
	api.Handle("/api/", repos.Routes())
	svc.Register(api)
	e.srv.Config.Handler = (&auth.Handler{Service: authSvc, AppURL: appURL}).Protect(api)
}

func within(t *testing.T, d time.Duration, f func() *http.Response) *http.Response {
	t.Helper()
	done := make(chan *http.Response, 1)
	go func() { done <- f() }()
	select {
	case resp := <-done:
		return resp
	case <-time.After(d):
		t.Fatal("request hung: it waits for a second pool connection while holding one")
		return nil
	}
}

// A readback answered 401 revokes the actor only after the locked transaction released its connection.
func TestReadbackUnauthorizedDoesNotStarvePool(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetIssues(repo, seedIssue(1, "alice", "Existing"))
	e.singleConnection()
	var reads atomic.Int32
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/1") && reads.Add(1) == 2 { // the readback
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
	resp := within(t, 10*time.Second, func() *http.Response {
		r, _ := alice.send(changes(repo, 1), "ro-1", `{"labels":{"add":["a"]}}`)
		return r
	})
	if resp.StatusCode != http.StatusServiceUnavailable { // the revocation moved the actor's fence: the operation is withheld
		t.Fatalf("status %d %s", resp.StatusCode, alice.body(resp))
	}
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = $1`, alice.id); n != 0 {
		t.Fatalf("a 401 readback must revoke the actor: %d sessions left", n)
	}
	if n := e.count(`SELECT count(*) FROM write_operations WHERE state = 'unknown'`); n != 1 {
		t.Fatalf("unreadable result must stay unknown: %d", n)
	}
}

// A recheck that cannot confirm answers its 409 after the locked transaction released its connection.
func TestRecheckMismatchDoesNotStarvePool(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.singleConnection()
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{Status: http.StatusBadGateway})
	_, op := alice.send(create(repo), "mm-1", `{"title":"Ship it"}`)
	e.gh.SetIssueWriteFault(fake.IssueWriteFault{})
	if op.State != "unknown" {
		t.Fatalf("setup: %+v", op)
	}
	resp := within(t, 10*time.Second, func() *http.Response {
		r, _ := alice.send(opPath(op.ID)+"/recheck", "", `{"issueNumber":99}`)
		return r
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d %s", resp.StatusCode, alice.body(resp))
	}
}

// A create that fails before anything is sent (here GitHub reports a full name the client cannot address) is failed,
// not unknown: nothing can have been applied.
func TestPreSendCreateErrorIsFailed(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "a/b", false)
	resp, op := alice.send(create(repo), "pre-1", `{"title":"Ship it"}`)
	if resp.StatusCode != http.StatusCreated || op.State != "failed" || op.Outcome.Error == "" {
		t.Fatalf("status %d %+v %s", resp.StatusCode, op, alice.body(resp))
	}
	if n := len(e.gh.WriteTokens()); n != 0 {
		t.Fatalf("writes sent: %d", n)
	}
}

// An operation that stops being pending before it is marked sent (a concurrent stale-pending recovery failed it) is
// answered as a replay, and nothing is sent.
func TestLostSendClaimAnswersRecordedOperation(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	if _, err := e.pool.Exec(context.Background(), `CREATE FUNCTION test_fail_new_op() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.state := 'failed'; RETURN NEW; END $$;
		CREATE TRIGGER test_fail_new_op BEFORE INSERT ON write_operations FOR EACH ROW EXECUTE FUNCTION test_fail_new_op()`); err != nil {
		t.Fatal(err)
	}
	resp, op := alice.send(create(repo), "claim-1", `{"title":"Ship it"}`)
	if resp.StatusCode != http.StatusOK || op.State != "failed" || op.ID == 0 {
		t.Fatalf("status %d %+v %s", resp.StatusCode, op, alice.body(resp))
	}
	if n := len(e.gh.WriteTokens()); n != 0 {
		t.Fatalf("writes sent: %d", n)
	}
}

// An update recorded but never marked sent (the process stopped in between) cannot stay pending forever: once stale,
// a replay of its key or a read of the operation fails it as never sent, and nothing is sent.
func TestStalePendingUpdateIsFailedOnRead(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetIssues(repo, seedIssue(1, "alice", "Existing"))
	body := `{"labels":{"add":["triaged"]}}`
	stall := func(key string) int64 {
		resp, op := alice.send(changes(repo, 1), key, body)
		if resp.StatusCode != http.StatusCreated || op.State != "succeeded" {
			t.Fatalf("update: %d %+v", resp.StatusCode, op)
		}
		// Rewind it to a row recorded two minutes ago and never marked sent.
		if _, err := e.pool.Exec(context.Background(), `UPDATE write_operations SET state='pending', sent_at=NULL, outcome='{}', created_at=now()-interval '2 minutes' WHERE id=$1`, op.ID); err != nil {
			t.Fatal(err)
		}
		return op.ID
	}
	replayed := stall("stuck-replay")
	labels := len(e.gh.Issues(repo)[0].Labels)
	if resp, op := alice.send(changes(repo, 1), "stuck-replay", body); resp.StatusCode != http.StatusOK || op.ID != replayed || op.State != "failed" {
		t.Fatalf("replay of a stale pending update: %d %+v", resp.StatusCode, op)
	}
	read := stall("stuck-read")
	if resp, op := alice.get(opPath(read)); resp.StatusCode != http.StatusOK || op.State != "failed" {
		t.Fatalf("read of a stale pending update: %d %+v", resp.StatusCode, op)
	}
	if n := len(e.gh.Issues(repo)[0].Labels); n != labels {
		t.Fatalf("a stale pending update was resent: %d labels, want %d", n, labels)
	}
}
