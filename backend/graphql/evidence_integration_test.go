//go:build integration

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

type evidenceHarness struct {
	t         *testing.T
	dbURL     string
	pool      *pgxpool.Pool
	gh        *fake.Server
	client    *githubapp.Client
	limitRepo atomic.Int64
	resetUnix int64
	q         *db.Queries
	authSvc   *auth.Service
	repos     *repositories.Service
	ah        *auth.Handler
	mux       *http.ServeMux
	api       *httptest.Server
}

func newEvidenceHarness(t *testing.T) *evidenceHarness {
	t.Helper()
	dbURL, pool := testutil.Database(t)
	gh := fake.New("cid", "secret")
	h := &evidenceHarness{
		t:         t,
		dbURL:     dbURL,
		pool:      pool,
		gh:        gh,
		resetUnix: time.Now().Add(time.Hour).UTC().Unix(),
	}
	ghHandler := gh.Handler()
	gs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := h.limitRepo.Load(); id != 0 && r.URL.Path == fmt.Sprintf("/repositories/%d", id) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(h.resetUnix, 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		ghHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(gs.Close)

	h.client = githubapp.New("cid", "secret", gs.URL, gs.URL, "http://app.test/auth/github/callback")
	h.q = db.New(pool)
	h.authSvc = &auth.Service{Pool: pool, Queries: h.q, GitHub: h.client, Key: bytes.Repeat([]byte{1}, 32)}
	h.repos = &repositories.Service{Pool: pool, Queries: h.q, GitHub: h.client, TokenSource: h.authSvc, TTL: time.Nanosecond}
	h.ah = &auth.Handler{Service: h.authSvc, AppURL: "http://app.test"}
	h.mux = http.NewServeMux()
	h.mux.Handle("/auth/", h.ah.Routes())
	h.mux.Handle("/graphql", h.ah.Protect(Server(&Resolver{Queries: h.q, RepoAccess: h.repos})))
	h.api = httptest.NewServer(h.mux)
	t.Cleanup(h.api.Close)
	return h
}

func (h *evidenceHarness) addShortTimeoutEndpoint(path string, timeout time.Duration) {
	h.mux.Handle(path, h.ah.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shortCtx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		Server(&Resolver{Queries: h.q, RepoAccess: h.repos}).ServeHTTP(w, r.WithContext(shortCtx))
	})))
}

func (h *evidenceHarness) addSingleConnEndpoint(ctx context.Context, path string) {
	h.t.Helper()
	cfg, err := pgxpool.ParseConfig(h.dbURL)
	if err != nil {
		h.t.Fatal(err)
	}
	cfg.MaxConns = 1
	singlePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(singlePool.Close)
	singleRepos := &repositories.Service{Pool: singlePool, Queries: db.New(singlePool), GitHub: h.client, TokenSource: h.authSvc, TTL: time.Minute}
	h.mux.Handle(path, h.ah.Protect(Server(&Resolver{Queries: db.New(singlePool), RepoAccess: singleRepos})))
}

func (h *evidenceHarness) signIn(login string) *http.Client {
	h.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(h.api.URL + "/auth/github/login")
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	v := loc.Query()
	v.Set("as", login)
	loc.RawQuery = v.Encode()
	resp, err = c.Get(loc.String())
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	cb, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err = c.Get(h.api.URL + cb.RequestURI())
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "http://app.test/" {
		h.t.Fatalf("login %s: %d %s", login, resp.StatusCode, resp.Header.Get("Location"))
	}
	return c
}

func (h *evidenceHarness) userID(ctx context.Context, login string) int64 {
	h.t.Helper()
	var id int64
	if err := h.pool.QueryRow(ctx, `SELECT id FROM users WHERE login=$1`, login).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *evidenceHarness) runAt(c *http.Client, path, query string) string {
	h.t.Helper()
	me, err := c.Get(h.api.URL + "/auth/me")
	if err != nil {
		h.t.Error(err)
		return ""
	}
	var session struct {
		CSRFToken string `json:"csrfToken"`
	}
	err = json.NewDecoder(me.Body).Decode(&session)
	me.Body.Close()
	if err != nil {
		h.t.Error(err)
		return ""
	}
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		h.t.Error(err)
		return ""
	}
	req, err := http.NewRequest("POST", h.api.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Error(err)
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://app.test")
	req.Header.Set("X-CSRF-Token", session.CSRFToken)
	resp, err := c.Do(req)
	if err != nil {
		h.t.Error(err)
		return ""
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Error(err)
		return ""
	}
	if resp.StatusCode != 200 {
		h.t.Errorf("status %d: %s", resp.StatusCode, b)
	}
	return string(b)
}

func (h *evidenceHarness) run(c *http.Client, query string) string {
	h.t.Helper()
	return h.runAt(c, "/graphql", query)
}

func TestEvidenceViewerAuthorizationAndFence(t *testing.T) {
	ctx := context.Background()
	h := newEvidenceHarness(t)
	pool, gh, repos := h.pool, h.gh, h.repos
	signIn, run := h.signIn, h.run
	alice := signIn("alice")
	bob := signIn("bob")
	aliceID := h.userID(ctx, "alice")
	repo := gh.AddRepo("alice", "secret", true)
	gh.Install("alice", 9, repo)
	if err := repos.Connect(ctx, aliceID, repo); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`INSERT INTO activity_pull_requests(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES (100,$1,1,'private PR','open','url',now(),now(),1,'alice','User'),(101,$1,2,'deleted PR','open','url',now(),now(),NULL,NULL,NULL)`, `INSERT INTO activity_reviews(source_id,repository_id,pull_request_id,state,body,html_url,submitted_at,author_id,author_login,author_type) VALUES (200,$1,100,'APPROVED','private review','url',now(),2,'bot','Bot'),(201,$1,100,'PENDING','pending secret','url',now(),3,'alice','User'),(202,$1,100,'COMMENTED','deleted review','url',now(),NULL,NULL,NULL)`, `INSERT INTO activity_comments(source_id,repository_id,issue_number,body,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES (300,$1,1,'private comment','url',now(),now(),1,'alice','User'),(301,$1,1,'deleted comment','url',now(),now(),NULL,NULL,NULL)`} {
		if _, err := pool.Exec(ctx, sql, repo); err != nil {
			t.Fatal(err)
		}
	}
	query := fmt.Sprintf(`{ pullRequestEvidence(repositoryIds:["%d"]) { nodes { title author { id } } withheld } reviewEvidence(repositoryIds:["%d"]) { nodes { body author { type } } withheld } commentEvidence(repositoryIds:["%d"]) { nodes { body author { id } } withheld } }`, repo, repo, repo)
	got := run(alice, query)
	for _, s := range []string{"private PR", "private review", "private comment", "Bot"} {
		if !strings.Contains(got, s) {
			t.Fatalf("authorized missing %s: %s", s, got)
		}
	}
	for _, want := range []string{`"title":"deleted PR","author":null`, `"body":"deleted review","author":null`, `"body":"deleted comment","author":null`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing null actor %s: %s", want, got)
		}
	}
	if strings.Contains(got, "pending secret") {
		t.Fatalf("pending review leaked: %s", got)
	}
	got = run(bob, query)
	if strings.Contains(got, "private ") {
		t.Fatalf("other viewer saw evidence: %s", got)
	}
	overviewQuery := fmt.Sprintf(`{ repositoryOverview(id:"%d", days:7) {
	  openPullRequests { value coverage { covered } }
	  releasesPublished { value coverage { covered } }
	  firstReviewTime { sampleSize coverage { reason } }
	  timeline { title }
	  contributors { login }
	} }`, repo)
	if _, err := pool.Exec(ctx, `INSERT INTO activity_resources(repository_id,resource,boundary,watermark)
	  VALUES ($1,'pull_requests',now()-interval '90 days',now()), ($1,'releases',now()-interval '90 days',now())`, repo); err != nil {
		t.Fatal(err)
	}
	got = run(alice, overviewQuery)
	for _, want := range []string{`"openPullRequests":{"value":2,"coverage":{"covered":true}}`, `"releasesPublished":{"value":0`, `"sampleSize":null`, "children_incomplete"} {
		if !strings.Contains(got, want) {
			t.Fatalf("authorized overview missing %s: %s", want, got)
		}
	}
	if got = run(bob, overviewQuery); !strings.Contains(got, `"repositoryOverview":null`) || strings.Contains(got, "errors") {
		t.Fatalf("unexpected cross-user overview: %s", got)
	}

	// Review state per PR: unknown until that PR's review history has converged, then derived from human reviews.
	reviewStates := fmt.Sprintf(`{ pullRequestEvidence(repositoryIds:["%d"]) { nodes { number reviewState } } }`, repo)
	if got = run(alice, reviewStates); !strings.Contains(got, `{"number":1,"reviewState":null}`) {
		t.Fatalf("review state before review history: %s", got)
	}
	for _, sql := range []string{
		`INSERT INTO activity_pull_requests(source_id,repository_id,number,title,state,html_url,created_at,updated_at,merged_at,author_id,author_login,author_type) VALUES
		 (102,$1,3,'merged PR','closed','url',now(),now(),now(),1,'alice','User'), (103,$1,4,'closed PR','closed','url',now(),now(),NULL,1,'alice','User'),
		 (104,$1,5,'approved PR','open','url',now(),now(),NULL,1,'alice','User'), (105,$1,6,'changes PR','open','url',now(),now(),NULL,1,'alice','User')`,
		`INSERT INTO activity_reviews(source_id,repository_id,pull_request_id,state,body,html_url,submitted_at,author_id,author_login,author_type) VALUES
		 (210,$1,101,'COMMENTED','','url',now(),5,'carol','User'),
		 (211,$1,104,'CHANGES_REQUESTED','','url',now()-interval '2 hours',5,'carol','User'), (212,$1,104,'APPROVED','','url',now()-interval '1 hour',5,'carol','User'),
		 (213,$1,104,'COMMENTED','','url',now(),5,'carol','User'),
		 (214,$1,105,'APPROVED','','url',now()-interval '1 hour',5,'carol','User'), (215,$1,105,'CHANGES_REQUESTED','','url',now(),6,'dave','User')`,
		`INSERT INTO activity_children(repository_id,resource,parent_id,parent_number,watermark)
		 SELECT $1,'reviews',id,id-99,now() FROM unnest(ARRAY[100,101,104,105]::bigint[]) id`,
	} {
		if _, err := pool.Exec(ctx, sql, repo); err != nil {
			t.Fatal(err)
		}
	}
	got = run(alice, reviewStates)
	for _, want := range []string{`{"number":1,"reviewState":"awaiting_review"}`, `{"number":2,"reviewState":"commented"}`, `{"number":3,"reviewState":"merged"}`,
		`{"number":4,"reviewState":"closed"}`, `{"number":5,"reviewState":"approved"}`, `{"number":6,"reviewState":"changes_requested"}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("review state missing %s: %s", want, got)
		}
	}

	// Real GitHub PR ids exceed int4, so the cursor must round-trip as a bigint.
	if _, err := pool.Exec(ctx, `INSERT INTO activity_pull_requests(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type)
	  VALUES (3000000002,$1,7,'large id PR','open','url',now(),now(),1,'alice','User')`, repo); err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct {
		after, want string
	}{
		{"", `"nodes":[{"number":7}],"endCursor":"3000000002","hasNextPage":true`},
		{`, after:"3000000002"`, `"nodes":[{"number":6}],"endCursor":"105","hasNextPage":true`},
		{`, after:"105"`, `"nodes":[{"number":5}],"endCursor":"104","hasNextPage":true`},
	} {
		prs := fmt.Sprintf(`{ pullRequestEvidence(repositoryIds:["%d"], first:1, newestFirst:true%s) { nodes { number } endCursor hasNextPage } }`, repo, page.after)
		if got := run(alice, prs); !strings.Contains(got, page.want) {
			t.Fatalf("newest-first PR page: %s", got)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM activity_pull_requests WHERE source_id=3000000002`); err != nil {
		t.Fatal(err)
	}

	// Cross-repository overview: an uninstalled repository is denied and excluded; the other still counts.
	hidden := gh.AddRepo("alice", "hidden-repo", true)
	gh.Install("alice", 10, hidden)
	if err := repos.Connect(ctx, aliceID, hidden); err != nil {
		t.Fatal(err)
	}
	var aliceGitHub, bobGitHub int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT github_id FROM users WHERE login='alice'), (SELECT github_id FROM users WHERE login='bob')`).Scan(&aliceGitHub, &bobGitHub); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO activity_resources(repository_id,resource,boundary,watermark) VALUES ($1,'issues',now()-interval '90 days',now()), ($2,'issues',now()-interval '90 days',now()), ($2,'pull_requests',now()-interval '90 days',now())`, []any{repo, hidden}},
		{`INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_type) VALUES
		 (900,$1,10,'mine open','open','url',now(),now(),1,'User'), (901,$1,11,'mine closed','closed','url',now(),now(),1,'User'),
		 (902,$1,12,'bobs issue','open','url',now(),now(),1,'User'), (903,$2,1,'hidden assigned','open','url',now(),now(),1,'User')`, []any{repo, hidden}},
		{`INSERT INTO activity_issue_assignees(issue_id,source_id,login,type) VALUES (900,$1,'alice','User'), (901,$1,'alice','User'), (902,$2,'bob','User'), (903,$1,'alice','User')`, []any{aliceGitHub, bobGitHub}},
		{`INSERT INTO activity_resources(repository_id,resource,boundary,watermark) VALUES ($1,'comments',now()-interval '90 days',now()), ($2,'comments',now()-interval '90 days',now())`, []any{repo, hidden}},
	} {
		if _, err := pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}
	gh.Uninstall(10)
	var cross struct {
		Data struct {
			CrossRepositoryOverview struct {
				RepositoriesCounted, RepositoriesSyncing, Withheld int
				OpenIssues, ReviewBacklog                          struct{ Value *int64 }
				NeedsAttention                                     []struct{ Kind, Title string }
				AssignedToMe                                       []struct{ Title, FullName string }
			}
		}
	}
	// The first read after the uninstall discovers the revocation, which moves the fence: nothing is returned.
	if got = run(alice, crossOverviewQuery); !strings.Contains(got, `"crossRepositoryOverview":null`) || strings.Contains(got, "hidden") {
		t.Fatalf("read that discovered the revocation: %s", got)
	}
	got = run(alice, crossOverviewQuery)
	if strings.Contains(got, "hidden") || strings.Contains(got, "errors") {
		t.Fatalf("cross overview leaked or failed: %s", got)
	}
	if err := json.Unmarshal([]byte(got), &cross); err != nil {
		t.Fatal(err)
	}
	c := cross.Data.CrossRepositoryOverview
	if c.RepositoriesCounted != 1 || c.RepositoriesSyncing != 0 || c.Withheld != 0 || c.OpenIssues.Value == nil || *c.OpenIssues.Value != 2 ||
		c.ReviewBacklog.Value == nil || *c.ReviewBacklog.Value != 1 || len(c.AssignedToMe) != 1 || c.AssignedToMe[0].Title != "mine open" ||
		c.AssignedToMe[0].FullName != "alice/secret" || len(c.NeedsAttention) != 3 {
		t.Fatalf("cross overview: %s", got)
	}
	if got = run(bob, crossOverviewQuery); !strings.Contains(got, `"repositoriesCounted":0`) || strings.Contains(got, "mine open") || strings.Contains(got, "secret") {
		t.Fatalf("other viewer cross overview: %s", got)
	}

	// Pause each read after authorization so the final fence must discard its result.
	for _, scenario := range []struct {
		name, query, blockedSQL string
	}{
		{"evidence", fmt.Sprintf(`{ pullRequestEvidence(repositoryIds:["%d"]) { nodes { title } withheld } }`, repo), "%FROM activity_pull_requests p%"},
		{"overview", overviewQuery, "%name: GetMetricCoverage%"},
		{"cross", crossOverviewQuery, "%name: GetMetricCoverage%"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `LOCK TABLE activity_pull_requests IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			result := make(chan string, 1)
			go func() {
				result <- run(alice, scenario.query)
			}()
			deadline := time.Now().Add(2 * time.Second)
			blocked := false
			for time.Now().Before(deadline) {
				if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1)`, scenario.blockedSQL).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !blocked {
				t.Fatal("evidence read did not block")
			}
			fenceTx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = accessfence.InvalidateAccess(ctx, fenceTx, accessfence.ScopeUser, aliceID); err != nil {
				t.Fatal(err)
			}
			if err = fenceTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-result
			if scenario.name == "cross" {
				if !strings.Contains(got, `"crossRepositoryOverview":null`) || !strings.Contains(got, "ACCESS_UNVERIFIED") {
					t.Fatalf("fenced cross overview leaked: %s", got)
				}
				return
			}
			if scenario.name == "overview" {
				if !strings.Contains(got, `"repositoryOverview":null`) || !strings.Contains(got, "ACCESS_UNVERIFIED") {
					t.Fatalf("fenced overview leaked: %s", got)
				}
				return
			}
			if !strings.Contains(got, `"nodes":[],"withheld":1`) {
				t.Fatalf("fenced read leaked: %s", got)
			}
		})
	}
	gh.Uninstall(9)
	got = run(alice, query)
	if strings.Contains(got, "private ") {
		t.Fatalf("revoked installation saw evidence: %s", got)
	}
}

func TestGuardedListInvariantsAndMidReadRevocation(t *testing.T) {
	ctx := context.Background()
	h := newEvidenceHarness(t)
	h.addShortTimeoutEndpoint("/graphql-short", 150*time.Millisecond)
	h.addSingleConnEndpoint(ctx, "/graphql-single")

	pool, gh, repos, q := h.pool, h.gh, h.repos, h.q
	alice := h.signIn("alice")
	bob := h.signIn("bob")
	aliceID := h.userID(ctx, "alice")
	repo := gh.AddRepo("alice", "secret", true)
	gh.Install("alice", 9, repo)
	if err := repos.Connect(ctx, aliceID, repo); err != nil {
		t.Fatal(err)
	}
	pub := gh.AddRepo("carol", "public-star", false)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, 'carol/public-star', '', false, false)`, []any{pub}},
		{`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($2, $1, 'starred', now())`, []any{pub, aliceID}},
		{`INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES
		 (10,$1,1,'issue 1','open','url',now(),now(),1,'alice','User'),
		 (11,$1,2,'issue 2','open','url',now(),now(),1,'alice','User')`, []any{repo}},
		{`INSERT INTO activity_pull_requests(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES
		 (100,$1,1,'pr 1','open','url',now(),now(),1,'alice','User'),
		 (101,$1,2,'pr 2','open','url',now(),now(),1,'alice','User')`, []any{repo}},
		{`INSERT INTO activity_reviews(source_id,repository_id,pull_request_id,state,body,html_url,submitted_at,author_id,author_login,author_type) VALUES
		 (200,$1,100,'APPROVED','rev 1','url',now(),2,'bob','User'),
		 (201,$1,100,'COMMENTED','rev 2','url',now(),2,'bob','User')`, []any{repo}},
		{`INSERT INTO activity_comments(source_id,repository_id,issue_number,body,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES
		 (300,$1,1,'c 1','url',now(),now(),1,'alice','User'),
		 (301,$1,1,'c 2','url',now(),now(),1,'alice','User')`, []any{repo}},
		{`INSERT INTO activity_releases(source_id,repository_id,name,tag_name,body,html_url,created_at,updated_at,published_at,observed_at,prerelease,author_id,author_login,author_type) VALUES
		 (400,$1,'rel 1','v1','release notes','url',now(),now(),now(),now(),false,1,'alice','User'),
		 (401,$1,'rel 2','v2','','url',now(),now(),now(),now(),false,1,'alice','User'),
		 (402,$2,'pub rel','v3','','url',now(),now(),now(),now(),false,1,'carol','User')`, []any{repo, pub}},
		{`INSERT INTO activity_issue_events(source_id,repository_id,issue_id,issue_number,event,url,created_at,observed_at,actor_id,actor_login,actor_type,assignee_id,assignee_login,assignee_type,label) VALUES
		 (500,$1,10,1,'assigned','url',now(),now(),1,'alice','User',2,'bob','User','bug'),
		 (501,$1,10,1,'reopened','url',now(),now(),1,'alice','User',NULL,NULL,NULL,NULL),
		 (502,$2,20,1,'closed','url',now(),now(),1,'carol','User',NULL,NULL,NULL,NULL)`, []any{repo, pub}},
	} {
		if _, err := pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("releaseEvidence_and_issueEventEvidence_authorized_and_unselected", func(t *testing.T) {
		relQ := fmt.Sprintf(`{ releaseEvidence(repositoryIds:["%d"], first: 1) { nodes { id repositoryId name tagName body url prerelease author { id login type } } endCursor hasNextPage withheld retryAt } }`, repo)
		gotRel := h.run(alice, relQ)
		for _, want := range []string{`"id":"400"`, `"name":"rel 1"`, `"tagName":"v1"`, `"body":"release notes"`, `"endCursor":"400"`, `"hasNextPage":true`, `"withheld":0`} {
			if !strings.Contains(gotRel, want) {
				t.Fatalf("authorized releaseEvidence missing %s: %s", want, gotRel)
			}
		}
		if strings.Contains(gotRel, `"errors"`) {
			t.Fatalf("authorized releaseEvidence unexpected error: %s", gotRel)
		}
		bobRelQ := fmt.Sprintf(`{ releaseEvidence(repositoryIds:["%d"], first: 1) { nodes { id } endCursor hasNextPage withheld retryAt } }`, pub)
		gotBobRel := h.run(bob, bobRelQ)
		if strings.Contains(gotBobRel, `"errors"`) || !strings.Contains(gotBobRel, `"nodes":[],"endCursor":null,"hasNextPage":false,"withheld":0,"retryAt":null`) {
			t.Fatalf("unselected accessible releaseEvidence leak or error: %s", gotBobRel)
		}

		evQ := fmt.Sprintf(`{ issueEventEvidence(repositoryIds:["%d"], first: 1) { nodes { id repositoryId issueId issueNumber event url actor { login } assignee { id login type } label } endCursor hasNextPage withheld retryAt } }`, repo)
		gotEv := h.run(alice, evQ)
		for _, want := range []string{`"id":"500"`, `"issueId":"10"`, `"issueNumber":1`, `"event":"assigned"`, `"assignee":{"id":"2","login":"bob","type":"User"}`, `"label":"bug"`, `"endCursor":"500"`, `"hasNextPage":true`, `"withheld":0`} {
			if !strings.Contains(gotEv, want) {
				t.Fatalf("authorized issueEventEvidence missing %s: %s", want, gotEv)
			}
		}
		if strings.Contains(gotEv, `"errors"`) {
			t.Fatalf("authorized issueEventEvidence unexpected error: %s", gotEv)
		}
		bobEvQ := fmt.Sprintf(`{ issueEventEvidence(repositoryIds:["%d"], first: 1) { nodes { id } endCursor hasNextPage withheld retryAt } }`, pub)
		gotBobEv := h.run(bob, bobEvQ)
		if strings.Contains(gotBobEv, `"errors"`) || !strings.Contains(gotBobEv, `"nodes":[],"endCursor":null,"hasNextPage":false,"withheld":0,"retryAt":null`) {
			t.Fatalf("unselected accessible issueEventEvidence leak or error: %s", gotBobEv)
		}
	})

	t.Run("fence_advance_during_selection_lookup", func(t *testing.T) {
		qStr := fmt.Sprintf(`{ issues(repositoryIds:["%d"], first: 1) { nodes { id } endCursor hasNextPage withheld retryAt } }`, repo)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "LOCK TABLE selections IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		resCh := make(chan string, 1)
		go func() {
			resCh <- h.run(alice, qStr)
		}()
		deadline := time.Now().Add(2 * time.Second)
		blocked := false
		for time.Now().Before(deadline) {
			if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%name: ListSelectedIDs%')`).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			t.Fatal("issues query did not block in ListSelectedIDs on selections")
		}
		fenceTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = accessfence.InvalidateAccess(ctx, fenceTx, accessfence.ScopeUser, aliceID); err != nil {
			t.Fatal(err)
		}
		if err = fenceTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		got := <-resCh
		want := `{"nodes":[],"endCursor":null,"hasNextPage":false,"withheld":1,"retryAt":null}`
		if !strings.Contains(got, want) || strings.Contains(got, `"errors"`) {
			t.Fatalf("fence advance during ListSelectedIDs got %s, want %s", got, want)
		}
	})

	run, runAt := h.run, h.runAt
	// Duplicate repositoryIds deduplicates withheld to 1.
	h.limitRepo.Store(pub)
	wantRetry := time.Unix(h.resetUnix, 0).UTC().Format(time.RFC3339)
	if got := run(alice, fmt.Sprintf(`{ issues(repositoryIds: ["%d","%d"], first: 1) { nodes { title } withheld retryAt } }`, pub, pub)); !strings.Contains(got, fmt.Sprintf(`"nodes":[],"withheld":1,"retryAt":"%s"`, wantRetry)) {
		t.Fatalf("duplicate repositoryIds withheld count wrong: %s", got)
	}

	// Mid-read revocation across issues and all four remaining evidence list readers resets cursors and preserves pre-read retryAt.
	for _, tc := range []struct {
		name, field, table, blockedSQL string
	}{
		{"issues", "issues", "activity_issues", "%FROM activity_issues i%"},
		{"reviewEvidence", "reviewEvidence", "activity_reviews", "%FROM activity_reviews%"},
		{"commentEvidence", "commentEvidence", "activity_comments", "%FROM activity_comments%"},
		{"releaseEvidence", "releaseEvidence", "activity_releases", "%FROM activity_releases%"},
		{"issueEventEvidence", "issueEventEvidence", "activity_issue_events", "%FROM activity_issue_events%"},
	} {
		t.Run("mid_read_"+tc.name, func(t *testing.T) {
			qStr := fmt.Sprintf(`{ %s(repositoryIds:["%d","%d"], first: 1) { nodes { id } endCursor hasNextPage withheld retryAt } }`, tc.field, repo, pub)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, "LOCK TABLE "+tc.table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			resCh := make(chan string, 1)
			go func() {
				resCh <- run(alice, qStr)
			}()
			deadline := time.Now().Add(2 * time.Second)
			blocked := false
			for time.Now().Before(deadline) {
				if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1)`, tc.blockedSQL).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !blocked {
				t.Fatalf("%s query did not block on %s", tc.name, tc.table)
			}
			fenceTx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = accessfence.InvalidateAccess(ctx, fenceTx, accessfence.ScopeUser, aliceID); err != nil {
				t.Fatal(err)
			}
			if err = fenceTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-resCh
			want := fmt.Sprintf(`{"nodes":[],"endCursor":null,"hasNextPage":false,"withheld":2,"retryAt":"%s"}`, wantRetry)
			if !strings.Contains(got, want) || strings.Contains(got, `"errors"`) {
				t.Fatalf("%s mid-read revocation got %s, want %s", tc.name, got, want)
			}
		})
	}
	limitRepo := &h.limitRepo
	limitRepo.Store(0)

	// 1-connection pool test: pullRequestEvidence(first: 1) with HasNextPage == true must close rows before fence.Current.
	t.Run("single_conn_pool_releases_before_fence_check", func(t *testing.T) {
		got := runAt(alice, "/graphql-single", fmt.Sprintf(`{ pullRequestEvidence(repositoryIds:["%d"], first: 1) { nodes { number } hasNextPage endCursor } }`, repo))
		if !strings.Contains(got, `"hasNextPage":true`) || strings.Contains(got, `"errors"`) {
			t.Fatalf("1-conn pullRequestEvidence failed: %s", got)
		}
	})

	// Read-phase DB error directly against Server handler with short ctx while activity_issues is locked.
	t.Run("read_phase_db_error_returns_database_unavailable", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "LOCK TABLE activity_issues IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		got := runAt(alice, "/graphql-short", fmt.Sprintf(`{ issues(repositoryIds:["%d"], first: 1) { nodes { id } withheld } }`, repo))
		if !strings.Contains(got, "database unavailable") {
			t.Fatalf("expected database unavailable on read error, got %s", got)
		}
	})

	// Demo-mode pullRequestEvidence with mixed demo + real repo IDs returns only demo PRs with RepoAccess: nil.
	t.Run("demo_pull_request_evidence_mixed_ids", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO repositories(id, name, description, is_private, is_demo) VALUES (1, 'orbit/example', 'demo', false, true) ON CONFLICT DO NOTHING;
			INSERT INTO activity_pull_requests(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES
			 (9001,1,1,'demo PR','open','url',now(),now(),1,'sam','User') ON CONFLICT DO NOTHING;
		`); err != nil {
			t.Fatal(err)
		}
		demoH := Server(&Resolver{Queries: q, Demo: true, DemoPool: pool})
		body, _ := json.Marshal(map[string]string{"query": fmt.Sprintf(`{ pullRequestEvidence(repositoryIds:["1","%d"]) { nodes { title } withheld } }`, repo)})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		demoH.ServeHTTP(w, req)
		got := w.Body.String()
		if !strings.Contains(got, "demo PR") || strings.Contains(got, "pr 1") || strings.Contains(got, `"errors"`) {
			t.Fatalf("demo pullRequestEvidence mixed IDs: %s", got)
		}
	})
}

func TestOverviewGuardedReadsAndEdges(t *testing.T) {
	ctx := context.Background()
	h := newEvidenceHarness(t)
	pool, gh, repos, run := h.pool, h.gh, h.repos, h.run
	limitRepo := &h.limitRepo

	alice := h.signIn("alice")
	bob := h.signIn("bob")
	aliceID, bobID := h.userID(ctx, "alice"), h.userID(ctx, "bob")

	// Setup Alice's sole private installed repo and verify sole-repository installation removal on crossRepositoryOverview.
	priv := gh.AddRepo("alice", "sole-secret", true)
	gh.Install("alice", 20, priv)
	if err := repos.Connect(ctx, aliceID, priv); err != nil {
		t.Fatal(err)
	}
	gh.Uninstall(20)
	crossQ := `{ crossRepositoryOverview(days: 7) { repositoriesCounted repositoriesSyncing withheld retryAt openIssues { value } daily { day } } }`
	if got := run(alice, crossQ); !strings.Contains(got, `"crossRepositoryOverview":null`) || !strings.Contains(got, "ACCESS_UNVERIFIED") || strings.Contains(got, `"retryAt"`) {
		t.Fatalf("sole-repo revocation first read: %s", got)
	}
	if got := run(alice, crossQ); !strings.Contains(got, `"repositoriesCounted":0`) || strings.Contains(got, `"errors"`) {
		t.Fatalf("sole-repo revocation second read: %s", got)
	}

	// Reinstall (first read discovers the installation and advances the fence; second read succeeds) and verify unselected, pre-read unverified with retryAt, follow-suspended, and mid-read attachMetadataStatus revocation.
	gh.Install("alice", 21, priv)
	_ = run(alice, crossQ)
	unselected := gh.AddRepo("carol", "unselected", false)
	beforeGH := gh.Hits("/user/repos") + gh.Hits("/repositories/")
	if got := run(alice, fmt.Sprintf(`{ repositoryOverview(id: "%d", days: 7) { fullName } }`, unselected)); !strings.Contains(got, `"repositoryOverview":null`) || strings.Contains(got, `"errors"`) {
		t.Fatalf("unselected repositoryOverview: %s", got)
	}
	if afterGH := gh.Hits("/user/repos") + gh.Hits("/repositories/"); afterGH != beforeGH {
		t.Fatalf("unselected repositoryOverview made %d GitHub hits, want 0", afterGH-beforeGH)
	}

	// Follow-suspended public star stays readable in both repositoryOverview and crossRepositoryOverview.
	pub := gh.AddRepo("carol", "suspended-star", false)
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, 'carol/suspended-star', '', false, false)`, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO selections (user_id, repository_id, source, selected_at, follow_suspended_at) VALUES ($1, $2, 'starred', now(), now())`, aliceID, pub); err != nil {
		t.Fatal(err)
	}
	if got := run(alice, fmt.Sprintf(`{ repositoryOverview(id: "%d", days: 7) { fullName } }`, pub)); !strings.Contains(got, `"fullName":"carol/suspended-star"`) || strings.Contains(got, `"errors"`) {
		t.Fatalf("suspended-follow repositoryOverview: %s", got)
	}
	if got := run(alice, crossQ); !strings.Contains(got, `"repositoriesSyncing":2`) || strings.Contains(got, `"errors"`) {
		t.Fatalf("suspended-follow crossRepositoryOverview: %s", got)
	}

	// Pre-read rate-limited repositoryOverview returns ACCESS_UNVERIFIED with extensions.retryAt.
	limitRepo.Store(pub)
	wantRetry := time.Unix(h.resetUnix, 0).UTC().Format(time.RFC3339)
	if got := run(alice, fmt.Sprintf(`{ repositoryOverview(id: "%d", days: 7) { fullName } }`, pub)); !strings.Contains(got, "ACCESS_UNVERIFIED") || !strings.Contains(got, fmt.Sprintf(`"retryAt":"%s"`, wantRetry)) {
		t.Fatalf("pre-read rate-limited repositoryOverview: %s", got)
	}

	// All-unverified crossRepositoryOverview (only pub selected on bob, rate-limited) returns non-null overview with withheld=1, retryAt, and full daily series.
	if _, err := pool.Exec(ctx, `INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'starred', now())`, bobID, pub); err != nil {
		t.Fatal(err)
	}
	gotBob := run(bob, crossQ)
	var bobResp struct {
		Data struct {
			CrossRepositoryOverview *struct {
				RepositoriesCounted int
				Withheld            int
				RetryAt             *string
				Daily               []any
			}
		}
		Errors []any
	}
	if err := json.Unmarshal([]byte(gotBob), &bobResp); err != nil {
		t.Fatal(err)
	}
	if len(bobResp.Errors) != 0 || bobResp.Data.CrossRepositoryOverview == nil ||
		bobResp.Data.CrossRepositoryOverview.RepositoriesCounted != 0 ||
		bobResp.Data.CrossRepositoryOverview.Withheld != 1 ||
		bobResp.Data.CrossRepositoryOverview.RetryAt == nil ||
		*bobResp.Data.CrossRepositoryOverview.RetryAt != wantRetry ||
		len(bobResp.Data.CrossRepositoryOverview.Daily) != 7 {
		t.Fatalf("all-unverified crossRepositoryOverview: %s", gotBob)
	}
	limitRepo.Store(0)

	t.Run("mid_read_load_activity_status", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		resCh := make(chan string, 1)
		go func() {
			resCh <- run(alice, fmt.Sprintf(`{ repositoryOverview(id: "%d", days: 7) { fullName activity { synchronized issues { phase } } } }`, priv))
		}()
		deadline := time.Now().Add(2 * time.Second)
		blocked := false
		for time.Now().Before(deadline) {
			if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%name: ListActivityStatuses%')`).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			t.Fatal("repositoryOverview did not block in loadActivityStatus on jobs")
		}
		fenceTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = accessfence.BumpAccess(ctx, fenceTx, accessfence.ScopeUser, aliceID); err != nil {
			t.Fatal(err)
		}
		if err = fenceTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		got := <-resCh
		if !strings.Contains(got, `"repositoryOverview":null`) || !strings.Contains(got, "ACCESS_UNVERIFIED") || strings.Contains(got, `"retryAt"`) {
			t.Fatalf("fenced loadActivityStatus leaked or wrong error: %s", got)
		}
	})

	// Selection limit on crossRepositoryOverview: 100 succeeds, 101 fails with OVERVIEW_SELECTION_LIMIT and 0 GitHub calls.
	t.Run("cross_overview_selection_limit_100_vs_101", func(t *testing.T) {
		for i := 0; i < 99; i++ { // bob already has 1 selection (pub)
			id := gh.AddRepo("bob", fmt.Sprintf("r-%03d", i), false)
			if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, $2, '', false, false)`, id, fmt.Sprintf("bob/r-%03d", i)); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', now())`, bobID, id); err != nil {
				t.Fatal(err)
			}
		}
		if got := run(bob, crossQ); !strings.Contains(got, `"repositoriesSyncing":100`) || strings.Contains(got, `"errors"`) {
			t.Fatalf("100 selections failed: %s", got)
		}
		extra := gh.AddRepo("bob", "r-101", false)
		if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, 'bob/r-101', '', false, false)`, extra); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'owned', now())`, bobID, extra); err != nil {
			t.Fatal(err)
		}
		hitsBefore := gh.Hits("/user/repos") + gh.Hits("/user/installations")
		if got := run(bob, crossQ); !strings.Contains(got, "OVERVIEW_SELECTION_LIMIT") {
			t.Fatalf("101 selections did not return OVERVIEW_SELECTION_LIMIT: %s", got)
		}
		if hitsAfter := gh.Hits("/user/repos") + gh.Hits("/user/installations"); hitsAfter != hitsBefore {
			t.Fatalf("101 selections made %d GitHub hits, want 0", hitsAfter-hitsBefore)
		}
	})
}
