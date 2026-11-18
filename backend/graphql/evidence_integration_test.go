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
	"strings"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

func TestEvidenceViewerAuthorizationAndFence(t *testing.T) {
	ctx := context.Background()
	_, pool := testutil.Database(t)
	gh := fake.New("cid", "secret")
	gs := httptest.NewServer(gh.Handler())
	defer gs.Close()
	client := githubapp.New("cid", "secret", gs.URL, gs.URL, "http://app.test/auth/github/callback")
	q := db.New(pool)
	authSvc := &auth.Service{Pool: pool, Queries: q, GitHub: client, Key: bytes.Repeat([]byte{1}, 32)}
	repos := &repositories.Service{Pool: pool, Queries: q, GitHub: client, TokenSource: authSvc, TTL: time.Nanosecond}
	ah := &auth.Handler{Service: authSvc, AppURL: "http://app.test"}
	mux := http.NewServeMux()
	mux.Handle("/auth/", ah.Routes())
	mux.Handle("/graphql", ah.Protect(Server(&Resolver{Queries: q, RepoAccess: repos})))
	api := httptest.NewServer(mux)
	defer api.Close()
	signIn := func(login string) *http.Client {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Get(api.URL + "/auth/github/login")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		v := loc.Query()
		v.Set("as", login)
		loc.RawQuery = v.Encode()
		resp, err = c.Get(loc.String())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		cb, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err = c.Get(api.URL + cb.RequestURI())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 302 || resp.Header.Get("Location") != "http://app.test/" {
			t.Fatalf("login %s: %d %s", login, resp.StatusCode, resp.Header.Get("Location"))
		}
		return c
	}
	alice := signIn("alice")
	bob := signIn("bob")
	var aliceID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE login='alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
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
	run := func(c *http.Client, query string) string {
		t.Helper()
		me, err := c.Get(api.URL + "/auth/me")
		if err != nil {
			t.Error(err)
			return ""
		}
		var session struct {
			CSRFToken string `json:"csrfToken"`
		}
		err = json.NewDecoder(me.Body).Decode(&session)
		me.Body.Close()
		if err != nil {
			t.Error(err)
			return ""
		}
		body, _ := json.Marshal(map[string]string{"query": query})
		req, _ := http.NewRequest("POST", api.URL+"/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://app.test")
		req.Header.Set("X-CSRF-Token", session.CSRFToken)
		resp, err := c.Do(req)
		if err != nil {
			t.Error(err)
			return ""
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Error(err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("status %d: %s", resp.StatusCode, b)
		}
		return string(b)
	}
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

	// Pause each read after authorization so the final fence must discard its result.
	for _, scenario := range []struct {
		name, query, blockedSQL string
	}{
		{"evidence", fmt.Sprintf(`{ pullRequestEvidence(repositoryIds:["%d"]) { nodes { title } withheld } }`, repo), "%FROM activity_pull_requests WHERE repository_id%"},
		{"overview", overviewQuery, "%name: GetMetricCoverage%"},
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
