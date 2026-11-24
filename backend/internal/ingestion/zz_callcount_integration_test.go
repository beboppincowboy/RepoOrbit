//go:build integration

package ingestion_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

// Consecutive private sync pages each authorize directly and use an installation credential.
func TestScratchSyncPagesShareAccessListing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.signIn("alice")
	repoID := e.gh.AddRepo("alice", "big", true)
	e.gh.Install("alice", 9, repoID)
	e.store(uid, repoID, true)
	now := time.Now().UTC()
	e.gh.SetIssues(repoID, fake.IssueFixture{ID: 1, Number: 1, Title: "x", State: "open", CreatedAt: now, UpdatedAt: now, User: fake.IssueActor{ID: 9, Login: "bot", Type: "Bot"}})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e.gh.AppPublicKey = &key.PublicKey
	e.deps.AppKey = key
	e.deps.InstallationCredentials = githubapp.NewInstallationCredentials(e.ghc, key)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := ingestion.RequestActivity(ctx, tx, repoID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	job := jobs.Job{Key: fmt.Sprintf("repo:%d", repoID)}
	for range 2 {
		err := e.deps.SyncIssues(ctx, job, txFence{pool: e.pool})
		if deferJob, ok := err.(*jobs.Defer); !ok || deferJob.Reason != "continue" {
			t.Fatalf("sync did not advance: %v", err)
		}
	}
	e.ghc.HTTP.Transport = afterActivityFetch{after: func() { e.gh.Uninstall(9) }}
	var previous, current string
	scan := func() string {
		t.Helper()
		var state string
		if err := e.pool.QueryRow(ctx, `SELECT json_build_array(phase, page, cursor, generation, previous_scan, current_scan)::text FROM activity_resources WHERE repository_id=$1 AND resource='issues'`, repoID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	previous = scan()
	err = e.deps.SyncIssues(ctx, job, txFence{pool: e.pool})
	if deferJob, ok := err.(*jobs.Defer); !ok || deferJob.Reason != ingestion.CodeAccessMoved {
		t.Fatalf("revocation during fetch was not detected: %v", err)
	}
	current = scan()
	if previous != current {
		t.Fatalf("revocation advanced the checkpoint: %s to %s", previous, current)
	}
	for _, endpoint := range []string{"/user/installations", "/user/repos", "/user/installations/9/repositories"} {
		if got := e.gh.Hits(endpoint); got != 0 {
			t.Fatalf("%s: %d listings, want zero", endpoint, got)
		}
	}
	for endpoint, want := range map[string]int{
		fmt.Sprintf("/repositories/%d", repoID): 6,
		"/repos/alice/big/installation":         5,
		"/repos/alice/big/issues":               3,
		"/app/installations/9/access_tokens":    1,
	} {
		if got := e.gh.Hits(endpoint); got != want {
			t.Fatalf("%s: %d calls, want %d", endpoint, got, want)
		}
	}
}

type afterActivityFetch struct {
	after func()
}

func (rt afterActivityFetch) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if req.URL.Path == "/repos/alice/big/issues" && err == nil && resp.StatusCode == http.StatusOK {
		rt.after()
	}
	return resp, err
}
