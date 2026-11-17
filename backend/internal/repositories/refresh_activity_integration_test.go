//go:build integration

package repositories_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
)

func wireActivity(e *env) {
	wireEligibility(e)
	e.repos.SignalActivity = func(ctx context.Context, tx pgx.Tx, repoID int64) (string, error) {
		out, err := ingestion.SignalActivity(ctx, tx, repoID, time.Now())
		return string(out), err
	}
}

func activityJobs(t *testing.T, e *env, repoID int64) (n int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE kind LIKE 'activity_%' AND key = $1`, fmt.Sprintf("repo:%d", repoID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func refresh(u *user, id int64) (int, string) {
	r := u.write("POST", fmt.Sprintf("/api/repositories/%d/activity/refresh", id))
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, strings.TrimSpace(string(b))
}

func TestRefreshActivityAcceptedThenCoalesced(t *testing.T) {
	e := setup(t, time.Hour)
	wireActivity(e)
	u := e.signIn("alice")
	id := e.gh.AddRepo("alice", "mine", false)
	if r := u.write("PUT", fmt.Sprintf("/api/repositories/%d/selection", id)); r.StatusCode != 200 {
		t.Fatalf("select: %d", r.StatusCode)
	}
	// Selection already queued activity; drain it so the first refresh is new work.
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM jobs WHERE kind LIKE 'activity_%'`); err != nil {
		t.Fatal(err)
	}
	if code, body := refresh(u, id); code != 202 || !strings.Contains(body, `"accepted"`) || activityJobs(t, e, id) == 0 {
		t.Fatalf("first: %d %s", code, body)
	}
	if code, body := refresh(u, id); code != 202 || !strings.Contains(body, `"coalesced"`) {
		t.Fatalf("second: %d %s", code, body)
	}
}

func TestRefreshActivityHidesUnselectedRepositories(t *testing.T) {
	e := setup(t, time.Hour)
	wireActivity(e)
	alice, bob := e.signIn("alice"), e.signIn("bob")
	id := e.gh.AddRepo("alice", "mine", false)
	if r := alice.write("PUT", fmt.Sprintf("/api/repositories/%d/selection", id)); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM jobs WHERE kind LIKE 'activity_%'`); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		u  *user
		id int64
	}{"other user": {bob, id}, "unknown id": {alice, 424242}} {
		if code, body := refresh(c.u, c.id); code != 404 || body != "not found" {
			t.Fatalf("%s: %d %q", name, code, body)
		}
	}
	if n := activityJobs(t, e, id); n != 0 {
		t.Fatalf("jobs created: %d", n)
	}
}

func TestRefreshActivityDeadStaysDead(t *testing.T) {
	e := setup(t, time.Hour)
	wireActivity(e)
	u := e.signIn("alice")
	id := e.gh.AddRepo("alice", "mine", false)
	u.write("PUT", fmt.Sprintf("/api/repositories/%d/selection", id))
	if _, err := e.pool.Exec(context.Background(), `UPDATE jobs SET state='dead' WHERE kind='activity_issues' AND key=$1`, fmt.Sprintf("repo:%d", id)); err != nil {
		t.Fatal(err)
	}
	if code, body := refresh(u, id); code != 409 || !strings.Contains(body, `"dead"`) {
		t.Fatalf("%d %s", code, body)
	}
	var s string
	_ = e.pool.QueryRow(context.Background(), `SELECT state FROM jobs WHERE kind='activity_issues' AND key=$1`, fmt.Sprintf("repo:%d", id)).Scan(&s)
	if s != "dead" {
		t.Fatalf("state = %s", s)
	}
}

func TestRefreshActivitySignedOut(t *testing.T) {
	e := setup(t, time.Hour)
	wireActivity(e)
	u := e.signIn("alice")
	anon := *u
	anon.csrf = ""
	anon.client = e.signIn("zed").client // different session, then drop cookies below
	anon.client.Jar = nil
	if r := anon.do("POST", "/api/repositories/1/activity/refresh", "", map[string]string{"Origin": appURL}); r.StatusCode != 401 {
		t.Fatalf("status %d", r.StatusCode)
	}
}
