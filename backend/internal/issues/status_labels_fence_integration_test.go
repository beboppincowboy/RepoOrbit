//go:build integration

package issues_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
)

// Access that moves after the labels are created but before settlement leaves the operation unknown with the
// "access changed…" error and status labels off; the answer is the recorded-access-moved 503.
func TestStatusLabelsAccessMovedBeforeSettlement(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	posts := 0
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		next.ServeHTTP(w, r)
		if r.Method == http.MethodPost && r.URL.Path == "/repos/alice/app/labels" {
			if posts++; posts == 3 {
				tx, err := e.pool.Begin(context.Background())
				if err == nil {
					_ = accessfence.BumpAccess(context.Background(), tx, accessfence.ScopeUser, alice.id)
					_ = tx.Commit(context.Background())
				}
			}
		}
	})
	resp, _ := alice.send(statusLabelsPath(repo), "on-1", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("RepoOrbit-Write-Disposition") != "possibly-recorded" {
		t.Fatalf("status=%d disposition=%q", resp.StatusCode, resp.Header.Get("RepoOrbit-Write-Disposition"))
	}
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) { next.ServeHTTP(w, r) })
	var list struct{ Operations []operation }
	if json.Unmarshal(alice.body(alice.do("GET", fmt.Sprintf("/api/repositories/%d/write-operations", repo), "", nil)), &list) != nil || len(list.Operations) != 1 {
		t.Fatalf("history: %+v", list.Operations)
	}
	op := list.Operations[0]
	if op.Kind != "status_labels" || op.State != "unknown" || op.Outcome.Error != "access changed before status labels could be turned on; recheck" {
		t.Fatalf("op=%+v", op)
	}
	if e.statusLabelsOn(repo) {
		t.Fatal("status labels turned on after access moved")
	}
}
