//go:build integration

package ingestion_test

import (
	"context"
	"errors"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"testing"
	"time"
)

type eventFixture struct {
	issueFixture
	releases []githubapp.Release
	events   []githubapp.IssueEvent
	fail     bool
}

func (f *eventFixture) FetchReleasesCursor(context.Context, string, string, string) ([]githubapp.Release, string, error) {
	if f.fail {
		return nil, "", errors.New("permission")
	}
	return f.releases, "", nil
}
func (f *eventFixture) FetchIssueEventsCursor(context.Context, string, string, string) ([]githubapp.IssueEvent, string, error) {
	return f.events, "", nil
}
func TestEventStreamsFenceReplayAndIsolation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	e.store(uid, 70, false)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = ingestion.RequestActivity(ctx, tx, 70, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	actor := &githubapp.IssueUser{ID: 5, Login: "bot", Type: "Bot"}
	f := &eventFixture{fail: true, releases: []githubapp.Release{{ID: 1, Name: "published", CreatedAt: now, UpdatedAt: now, PublishedAt: &now, Author: actor}, {ID: 2, Draft: true}}, events: []githubapp.IssueEvent{{ID: 3, Event: "closed", CreatedAt: now, Issue: &githubapp.EventIssue{ID: 42, Number: 4}, Actor: actor}, {ID: 4, Event: "renamed", CreatedAt: now, Issue: &githubapp.EventIssue{ID: 42, Number: 4}}}}
	e.deps.GitHub = f
	e.deps.Access = f
	job := jobs.Job{Key: "repo:70"}
	fence := txFence{pool: e.pool}
	if err = e.deps.SyncIssueEvents(ctx, job, lostFence{}); !errors.Is(err, jobs.ErrLostClaim) {
		t.Fatalf("lost claim: %v", err)
	}
	if err = e.deps.SyncReleases(ctx, job, fence); err == nil {
		t.Fatal("expected failure")
	}
	run := func(call func() error) {
		t.Helper()
		for i := 0; i < 3; i++ {
			if err := call(); err != nil {
				if _, ok := err.(*jobs.Defer); !ok {
					t.Fatal(err)
				}
			}
		}
	}
	run(func() error { return e.deps.SyncIssueEvents(ctx, job, fence) })
	var n int
	var phase, code string
	if err = e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_issue_events),phase FROM activity_resources WHERE repository_id=70 AND resource='issue_events'`).Scan(&n, &phase); err != nil || n != 1 || phase != "current" {
		t.Fatalf("events=%d phase=%s err=%v", n, phase, err)
	}
	if err = e.pool.QueryRow(ctx, `SELECT last_error_code FROM activity_resources WHERE repository_id=70 AND resource='releases'`).Scan(&code); err != nil || code == "" {
		t.Fatalf("release error=%s err=%v", code, err)
	}
	f.fail = false
	run(func() error { return e.deps.SyncReleases(ctx, job, fence) })
	if err = e.pool.QueryRow(ctx, `SELECT count(*) FROM activity_releases`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("releases=%d err=%v", n, err)
	}
	if _, err = e.pool.Exec(ctx, `UPDATE activity_resources SET phase='backfill' WHERE repository_id=70 AND resource='releases'`); err != nil {
		t.Fatal(err)
	}
	f.releases[0].Name = "older"
	f.releases[0].UpdatedAt = now.Add(-time.Hour)
	if err = e.deps.SyncReleases(ctx, job, fence); err != nil {
		if _, ok := err.(*jobs.Defer); !ok {
			t.Fatal(err)
		}
	}
	var name string
	if err = e.pool.QueryRow(ctx, `SELECT name FROM activity_releases WHERE source_id=1`).Scan(&name); err != nil || name != "published" {
		t.Fatalf("replay=%s err=%v", name, err)
	}
}
