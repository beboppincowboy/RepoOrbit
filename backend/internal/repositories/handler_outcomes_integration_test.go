//go:build integration

package repositories_test

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRefreshActivityUnverifiedAccessAnswers503(t *testing.T) {
	e := setup(t, time.Hour)
	wireActivity(e)
	u := e.signIn("alice")
	id := e.gh.AddRepo("alice", "mine", false)
	if r := u.write("PUT", fmt.Sprintf("/api/repositories/%d/selection", id)); r.StatusCode != 200 {
		t.Fatalf("select: %d", r.StatusCode)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM jobs WHERE kind LIKE 'activity_%'`); err != nil {
		t.Fatal(err)
	}
	e.gh.LimitUntil(time.Now().Add(time.Hour))
	r := u.write("POST", fmt.Sprintf("/api/repositories/%d/activity/refresh", id))
	if r.StatusCode != 503 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("rate-limited refresh: %d %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	if n := activityJobs(t, e, id); n != 0 {
		t.Fatalf("jobs created: %d", n)
	}
}

func TestInstalledGitHubErrorRedirectsFailed(t *testing.T) {
	e := setup(t, time.Hour)
	u := e.signIn("alice")
	e.gh.Fail(true)
	r := u.do("GET", "/auth/github/installed?setup_action=install&installation_id=77", "", nil)
	if r.StatusCode != 302 || r.Header.Get("Location") != appURL+"/repositories?install=failed" {
		t.Fatalf("%d %q", r.StatusCode, r.Header.Get("Location"))
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM installations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("installations = %d, %v", n, err)
	}
}
