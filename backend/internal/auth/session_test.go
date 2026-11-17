//go:build integration

package auth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

const appURL = "http://app.test"

type env struct {
	h      *Handler
	srv    *httptest.Server // the Go API
	gh     *fake.Server
	now    time.Time
	client *http.Client // keeps cookies, never follows redirects
	t      *testing.T
}

func setup(t *testing.T) *env {
	t.Helper()
	_, pool := testutil.Database(t)
	e := &env{t: t, now: time.Now(), gh: fake.New("cid", "secret")}
	ghSrv := httptest.NewServer(e.gh.Handler())
	t.Cleanup(ghSrv.Close)
	svc := &Service{Pool: pool, Queries: db.New(pool), Key: bytes.Repeat([]byte{1}, 32), Now: func() time.Time { return e.now },
		GitHub: githubapp.New("cid", "secret", ghSrv.URL, ghSrv.URL, appURL+"/auth/github/callback")}
	e.h = &Handler{Service: svc, AppURL: appURL}
	mux := http.NewServeMux()
	mux.Handle("/auth/", e.h.Routes())
	mux.Handle("/protected", e.h.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })))
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

func (e *env) do(method, path string, header map[string]string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// authorize walks the login redirect to the fake GitHub and returns the callback URL path GitHub sends the browser to.
func (e *env) authorize(as string) string {
	e.t.Helper()
	resp := e.do("GET", "/auth/github/login?next=https://evil.example", nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || !strings.HasPrefix(loc.Path, "/login/oauth/authorize") {
		e.t.Fatalf("login redirect: %d %s", resp.StatusCode, loc)
	}
	if as != "" {
		q := loc.Query()
		q.Set("as", as)
		loc.RawQuery = q.Encode()
	}
	gh, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(loc.String())
	if err != nil {
		e.t.Fatal(err)
	}
	gh.Body.Close()
	cb, _ := url.Parse(gh.Header.Get("Location"))
	return cb.Path + "?" + cb.RawQuery
}

func (e *env) signIn(as string) *http.Response {
	e.t.Helper()
	resp := e.do("GET", e.authorize(as), nil)
	if resp.StatusCode != 302 || resp.Header.Get("Location") != appURL+"/" {
		e.t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	return resp
}

func (e *env) sessionCookie() string {
	u, _ := url.Parse(e.srv.URL)
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

func TestSignInAndOut(t *testing.T) {
	e := setup(t)
	if got := e.do("GET", "/protected", nil).StatusCode; got != 401 {
		t.Fatalf("signed out: %d", got)
	}
	e.signIn("octocat") // also proves ?next=https://evil.example never changes the destination
	if got := e.do("GET", "/protected", nil).StatusCode; got != 200 {
		t.Fatalf("signed in: %d", got)
	}
	token := e.sessionCookie()
	v, err := e.h.Service.ViewerForSession(context.Background(), token)
	if err != nil || v.Login != "octocat" {
		t.Fatalf("viewer %+v %v", v, err)
	}
	// Logout needs Origin and CSRF.
	if got := e.do("POST", "/auth/logout", nil).StatusCode; got != 403 {
		t.Fatalf("logout without csrf: %d", got)
	}
	csrf := e.h.Service.CSRFToken(token)
	if got := e.do("POST", "/auth/logout", map[string]string{"Origin": "http://evil.test", "X-CSRF-Token": csrf}).StatusCode; got != 403 {
		t.Fatalf("logout cross-origin: %d", got)
	}
	if got := e.do("POST", "/auth/logout", map[string]string{"Origin": appURL, "X-CSRF-Token": "wrong"}).StatusCode; got != 403 {
		t.Fatalf("logout bad token: %d", got)
	}
	if got := e.do("POST", "/auth/logout", map[string]string{"Origin": appURL, "X-CSRF-Token": csrf}).StatusCode; got != 204 {
		t.Fatalf("logout: %d", got)
	}
	// Logout invalidates the session immediately, even if the old cookie is replayed.
	if _, err := e.h.Service.ViewerForSession(context.Background(), token); err != ErrUnauthenticated {
		t.Fatalf("revoked session accepted: %v", err)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/protected", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("replayed cookie: %d", resp.StatusCode)
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	e := setup(t)
	reject := func(name string, resp *http.Response) {
		t.Helper()
		if resp.StatusCode != 302 || resp.Header.Get("Location") != appURL+"/login?error=signin" || e.sessionCookie() != "" {
			t.Fatalf("%s: %d %s session=%q", name, resp.StatusCode, resp.Header.Get("Location"), e.sessionCookie())
		}
	}
	good := e.authorize("octocat")
	cb, _ := url.Parse(good)
	// State in the URL differs from the browser's cookie.
	q := cb.Query()
	q.Set("state", "attacker-state")
	reject("state mismatch", e.do("GET", cb.Path+"?"+q.Encode(), nil))
	// No state cookie at all (a callback finished in a different browser).
	bare := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := bare.Get(e.srv.URL + good)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Location") != appURL+"/login?error=signin" || strings.Contains(resp.Header.Get("Set-Cookie"), sessionCookie) {
		t.Fatalf("callback without state cookie: %d %v", resp.StatusCode, resp.Header)
	}
	// Valid callback once, then replayed.
	e.signIn("octocat")
	e.client.Jar, _ = cookiejar.New(nil)
	// The replay lacks a fresh state cookie and the state row is already consumed.
	reject("reused state", e.do("GET", good, nil))
}

func TestExpiredStateRejected(t *testing.T) {
	e := setup(t)
	good := e.authorize("octocat")
	e.now = e.now.Add(stateLifetime + time.Second)
	if resp := e.do("GET", good, nil); resp.Header.Get("Location") != appURL+"/login?error=signin" {
		t.Fatalf("expired state accepted: %s", resp.Header.Get("Location"))
	}
}

func TestSessionFixedSevenDayLifetime(t *testing.T) {
	e := setup(t)
	e.signIn("octocat")
	token := e.sessionCookie()
	// Use the session daily; use must not extend it.
	for day := 1; day < 7; day++ {
		e.now = e.now.Add(24 * time.Hour)
		if _, err := e.h.Service.ViewerForSession(context.Background(), token); err != nil {
			t.Fatalf("day %d: %v", day, err)
		}
	}
	e.now = e.now.Add(24*time.Hour + time.Second)
	if _, err := e.h.Service.ViewerForSession(context.Background(), token); err != ErrUnauthenticated {
		t.Fatalf("session older than 7 days accepted: %v", err)
	}
}

func TestStoredTokensAreEncrypted(t *testing.T) {
	e := setup(t)
	e.signIn("octocat")
	var enc []byte
	if err := e.h.Service.Pool.QueryRow(context.Background(), "SELECT access_token_enc FROM users").Scan(&enc); err != nil || bytes.Contains(enc, []byte("ghu_")) {
		t.Fatalf("token stored in plaintext or missing: %v", err)
	}
}

func TestConcurrentRefreshesDoNotLoseTheToken(t *testing.T) {
	e := setup(t)
	e.gh.RefreshDelay = 150 * time.Millisecond
	e.signIn("octocat")
	ctx := context.Background()
	v, _ := e.h.Service.ViewerForSession(ctx, e.sessionCookie())
	// Age the stored access token past its lifetime.
	if _, err := e.h.Service.Pool.Exec(ctx, "UPDATE users SET access_expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	tokens := make([]string, 6)
	var wg sync.WaitGroup
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := e.h.Service.AccessToken(ctx, v.UserID)
			if err != nil {
				t.Errorf("refresh %d: %v", i, err)
			}
			tokens[i] = tok
		}()
	}
	wg.Wait()
	if n := e.gh.Refreshes(); n != 1 {
		t.Fatalf("refresh token spent %d times", n)
	}
	for _, tok := range tokens {
		if tok == "" || tok != tokens[0] {
			t.Fatalf("callers saw different tokens: %v", tokens)
		}
	}
	// The stored token is the new one and still works.
	if tok, err := e.h.Service.AccessToken(ctx, v.UserID); err != nil || tok != tokens[0] || e.gh.Refreshes() != 1 {
		t.Fatalf("stored token: %q %v refreshes=%d", tok, err, e.gh.Refreshes())
	}
	if _, err := e.h.Service.GitHub.FetchUser(ctx, tokens[0]); err != nil {
		t.Fatalf("new token rejected: %v", err)
	}
}

func TestMeAndCookieAttributes(t *testing.T) {
	e := setup(t)
	body := func() string {
		resp, err := e.client.Get(e.srv.URL + "/auth/me")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := body(); !strings.Contains(got, `"user":null`) {
		t.Fatalf("signed out: %s", got)
	}
	resp := e.signIn("octocat")
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.MaxAge != int(SessionLifetime.Seconds()) || session.Secure {
		t.Fatalf("session cookie %+v (Secure must be off for an http AppURL)", session)
	}
	if got := body(); !strings.Contains(got, `"login":"octocat"`) || strings.Contains(got, `"csrfToken":null`) {
		t.Fatalf("signed in: %s", got)
	}
	if (&Handler{AppURL: "https://app.test"}).secure() != true {
		t.Fatal("https AppURL must set Secure cookies")
	}
}

func TestSecondUserIsolationAndReLogin(t *testing.T) {
	e := setup(t)
	e.signIn("octocat")
	first := e.sessionCookie()
	e.client.Jar, _ = cookiejar.New(nil)
	e.signIn("octocat") // same GitHub user again: one users row, a new session
	var users int
	_ = e.h.Service.Pool.QueryRow(context.Background(), "SELECT count(*) FROM users").Scan(&users)
	if users != 1 || e.sessionCookie() == first {
		t.Fatalf("users=%d same session=%v", users, e.sessionCookie() == first)
	}
	e.client.Jar, _ = cookiejar.New(nil)
	e.signIn("hubot")
	if e.h.Service.CSRFToken(first) == e.h.Service.CSRFToken(e.sessionCookie()) {
		t.Fatal("sessions share a CSRF token")
	}
	// The first user's CSRF token must not authorize the second user's session.
	if got := e.do("POST", "/auth/logout", map[string]string{"Origin": appURL, "X-CSRF-Token": e.h.Service.CSRFToken(first)}).StatusCode; got != 403 {
		t.Fatalf("cross-user csrf accepted: %d", got)
	}
}

func TestLoginPurgesExpiredRows(t *testing.T) {
	e := setup(t)
	e.signIn("octocat")
	e.now = e.now.Add(SessionLifetime + time.Hour)
	e.authorize("octocat") // BeginLogin sweeps expired sessions and states
	var sessions, states int
	_ = e.h.Service.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM sessions), (SELECT count(*) FROM oauth_states)").Scan(&sessions, &states)
	if sessions != 0 || states != 1 {
		t.Fatalf("sessions=%d states=%d, want 0 and 1 (the new state)", sessions, states)
	}
}

func TestRefreshFailuresKeepStoredTokens(t *testing.T) {
	e := setup(t)
	e.signIn("octocat")
	ctx := context.Background()
	v, _ := e.h.Service.ViewerForSession(ctx, e.sessionCookie())
	pool := e.h.Service.Pool
	var before []byte
	_ = pool.QueryRow(ctx, "SELECT access_token_enc FROM users").Scan(&before)
	expire := "UPDATE users SET access_expires_at = now() - interval '1 hour'"
	// No refresh token stored: the user must sign in again.
	_, _ = pool.Exec(ctx, expire+", refresh_token_enc = NULL")
	if _, err := e.h.Service.AccessToken(ctx, v.UserID); err != ErrUnauthenticated {
		t.Fatalf("no refresh token: %v", err)
	}
	// A refresh GitHub rejects errors out and leaves the stored token untouched.
	sealed, _ := seal(e.h.Service.Key, "ghr_unknown")
	_, _ = pool.Exec(ctx, "UPDATE users SET refresh_token_enc = $1", sealed)
	if _, err := e.h.Service.AccessToken(ctx, v.UserID); err == nil {
		t.Fatal("rejected refresh succeeded")
	}
	var after []byte
	_ = pool.QueryRow(ctx, "SELECT access_token_enc FROM users").Scan(&after)
	if !bytes.Equal(before, after) {
		t.Fatal("failed refresh changed the stored access token")
	}
}

func TestSessionLookupDatabaseErrorIs503(t *testing.T) {
	e := setup(t)
	e.signIn("octocat")
	e.h.Service.Pool.Close() // simulate a database outage
	if got := e.do("GET", "/protected", nil).StatusCode; got != 503 {
		t.Fatalf("protected during outage: %d", got)
	}
	if got := e.do("GET", "/auth/me", nil).StatusCode; got != 503 {
		t.Fatalf("me during outage: %d", got)
	}
}

// Signing in runs OnSignIn in the same transaction as the user upsert and session, so work paused for a missing
// credential resumes with the sign-in; a failing hook means no sign-in at all (no user, no session).
func TestSignInRunsEligibilityHookInItsTransaction(t *testing.T) {
	e := setup(t)
	var seen []int64
	e.h.Service.OnSignIn = func(ctx context.Context, tx pgx.Tx, userID int64) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, userID).Scan(&n); err != nil || n != 1 {
			t.Errorf("hook ran outside the sign-in transaction: sessions=%d err=%v", n, err)
		}
		seen = append(seen, userID)
		return nil
	}
	e.signIn("octocat")
	if len(seen) != 1 {
		t.Fatalf("hook calls: %v", seen)
	}
	e.h.Service.OnSignIn = func(context.Context, pgx.Tx, int64) error { return errors.New("boom") }
	pre := countRows(t, e, "sessions")
	if resp := e.do("GET", e.authorize("hubot"), nil); resp.Header.Get("Location") == appURL+"/" {
		t.Fatal("sign-in succeeded despite a failing hook")
	}
	if countRows(t, e, "sessions") != pre || countRows(t, e, "users WHERE login = 'hubot'") != 0 {
		t.Fatal("a failed sign-in left a user or session behind")
	}
}

func countRows(t *testing.T, e *env, from string) (n int) {
	t.Helper()
	if err := e.h.Service.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+from).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
