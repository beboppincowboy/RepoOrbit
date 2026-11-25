//go:build integration

package issues_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp/fake"
)

func statusLabelsPath(repoID int64) string {
	return fmt.Sprintf("/api/repositories/%d/status-labels", repoID)
}

func (e *env) statusLabelsOn(repoID int64) bool {
	e.t.Helper()
	var on bool
	if err := e.pool.QueryRow(context.Background(), `SELECT status_labels FROM repositories WHERE id = $1`, repoID).Scan(&on); err != nil {
		e.t.Fatal(err)
	}
	return on
}

func (u *user) disableStatusLabels(repoID int64) *http.Response {
	u.e.t.Helper()
	return u.do("DELETE", statusLabelsPath(repoID), "", map[string]string{"Origin": appURL, "X-CSRF-Token": u.csrf})
}

type statusLabelState struct {
	Enabled   bool `json:"enabled"`
	CanChange bool `json:"canChange"`
	Labels    []struct {
		Name   string `json:"name"`
		Exists bool   `json:"exists"`
	} `json:"labels"`
}

func (u *user) statusLabelState(repoID int64) statusLabelState {
	u.e.t.Helper()
	resp := u.do("GET", statusLabelsPath(repoID), "", nil)
	var s statusLabelState
	if resp.StatusCode != http.StatusOK || json.Unmarshal(u.body(resp), &s) != nil {
		u.e.t.Fatalf("status label state: %d", resp.StatusCode)
	}
	return s
}

// Write or Triage users cannot turn status labels on or off, forged requests fail, and nothing reaches GitHub.
func TestStatusLabelsNeedAdminOrMaintain(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	for login, level := range map[string]string{"bob": "write", "carol": "triage", "dave": "maintain"} {
		e.gh.SetPermission(login, repo, level)
	}
	bob, carol, dave := e.signIn("bob"), e.signIn("carol"), e.signIn("dave")

	if s := bob.statusLabelState(repo); s.CanChange || s.Labels != nil {
		t.Fatalf("a Write user is offered the setup: %+v", s)
	}
	if resp, _ := bob.send(statusLabelsPath(repo), "on-bob", `{}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Write user turned labels on: %d", resp.StatusCode)
	}
	if resp, _ := carol.send(statusLabelsPath(repo), "on-carol", `{}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Triage user turned labels on: %d", resp.StatusCode)
	}
	forged := carol.do("POST", statusLabelsPath(repo), `{}`, map[string]string{"Origin": appURL, "Idempotency-Key": "forged", "Content-Type": "application/json"})
	if forged.StatusCode != http.StatusForbidden {
		t.Fatalf("request without CSRF token: %d", forged.StatusCode)
	}
	if len(e.gh.Labels(repo)) != 0 || e.statusLabelsOn(repo) {
		t.Fatalf("denied requests changed something: labels=%v on=%v", e.gh.Labels(repo), e.statusLabelsOn(repo))
	}

	if resp, op := dave.send(statusLabelsPath(repo), "on-dave", `{}`); resp.StatusCode != http.StatusCreated || op.State != "succeeded" || op.Kind != "status_labels" {
		t.Fatalf("Maintain user could not turn labels on: %d %+v", resp.StatusCode, op)
	}
	if resp := bob.disableStatusLabels(repo); resp.StatusCode != http.StatusForbidden || !e.statusLabelsOn(repo) {
		t.Fatalf("Write user turned labels off: %d", resp.StatusCode)
	}
	if resp := alice.disableStatusLabels(repo); resp.StatusCode != http.StatusNoContent || e.statusLabelsOn(repo) {
		t.Fatalf("Admin could not turn labels off: %d", resp.StatusCode)
	}
	e.noInstallationAuthority()
}

func TestStatusLabelStateUnauthorizedRevokes(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels/status:review"):
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		case strings.HasSuffix(r.URL.Path, "/labels/status:todo"):
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		next.ServeHTTP(w, r)
	})
	if resp := alice.do("GET", statusLabelsPath(repo), "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d %s", resp.StatusCode, alice.body(resp))
	}
	if n := e.count(`SELECT count(*) FROM sessions WHERE user_id = $1`, alice.id); n != 0 {
		t.Fatalf("a 401 label check must revoke the viewer: %d sessions left", n)
	}
}

// Turning labels on creates exactly the missing labels through review and reuses an existing one, whatever its case.
func TestStatusLabelsCreateOnlyMissing(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.AddLabel(repo, "Status:Todo")
	e.gh.AddLabel(repo, "bug")

	s := alice.statusLabelState(repo)
	exists := map[string]bool{}
	for _, l := range s.Labels {
		exists[l.Name] = l.Exists
	}
	if s.Enabled || !s.CanChange || len(exists) != 3 || !exists["status:todo"] || exists["status:in-progress"] || exists["status:review"] {
		t.Fatalf("review state: %+v", s)
	}

	resp, op := alice.send(statusLabelsPath(repo), "on-1", `{}`)
	if resp.StatusCode != http.StatusCreated || op.State != "succeeded" || op.Actor != "alice" || len(op.Outcome.Unapplied) != 0 {
		t.Fatalf("status=%d op=%+v", resp.StatusCode, op)
	}
	var out struct {
		Created []string `json:"created"`
	}
	raw := alice.body(alice.do("GET", opPath(op.ID), "", nil))
	var full struct {
		Outcome json.RawMessage `json:"outcome"`
	}
	if json.Unmarshal(raw, &full) != nil || json.Unmarshal(full.Outcome, &out) != nil || !slices.Equal(out.Created, []string{"status:in-progress", "status:review"}) {
		t.Fatalf("created labels: %s", raw)
	}
	if got := e.gh.Labels(repo); !slices.Equal(got, []string{"Status:Todo", "bug", "status:in-progress", "status:review"}) {
		t.Fatalf("repository labels: %v", got)
	}
	if !e.statusLabelsOn(repo) {
		t.Fatal("status labels not turned on")
	}
	if s := alice.statusLabelState(repo); !s.Enabled || s.Labels != nil {
		t.Fatalf("state after enabling: %+v", s)
	}

	// Turning them on again reuses all three; turning them off writes nothing to GitHub.
	if _, op := alice.send(statusLabelsPath(repo), "on-2", `{}`); op.State != "succeeded" {
		t.Fatalf("second enable: %+v", op)
	}
	if resp := alice.disableStatusLabels(repo); resp.StatusCode != http.StatusNoContent || len(e.gh.Labels(repo)) != 4 {
		t.Fatalf("disable: %d labels=%v", resp.StatusCode, e.gh.Labels(repo))
	}
	var list struct{ Operations []operation }
	if json.Unmarshal(alice.body(alice.do("GET", fmt.Sprintf("/api/repositories/%d/write-operations", repo), "", nil)), &list) != nil ||
		len(list.Operations) != 2 || list.Operations[0].Kind != "status_labels" {
		t.Fatalf("history: %+v", list.Operations)
	}
	e.noInstallationAuthority()
}

// A label create without a reliable answer leaves the operation unknown and the setting off; a recheck that finds all
// three labels turns status labels on, but only for an actor who is still Admin or Maintain.
func TestAmbiguousStatusLabelsResolveOnRecheck(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.gh.SetPermission("dave", repo, "maintain")
	dave := e.signIn("dave")
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/alice/app/labels" {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout) // nothing created, but the client cannot know
			return
		}
		next.ServeHTTP(w, r)
	})
	resp, op := dave.send(statusLabelsPath(repo), "on-1", `{}`)
	if resp.StatusCode != http.StatusAccepted || op.State != "unknown" || e.statusLabelsOn(repo) {
		t.Fatalf("status=%d op=%+v on=%v", resp.StatusCode, op, e.statusLabelsOn(repo))
	}
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) { next.ServeHTTP(w, r) })
	for _, l := range []string{"status:todo", "status:in-progress", "status:review"} {
		e.gh.AddLabel(repo, l)
	}

	e.gh.SetPermission("dave", repo, "write")
	if resp, _ := dave.send(opPath(op.ID)+"/recheck", "", `{}`); resp.StatusCode != http.StatusForbidden || e.statusLabelsOn(repo) {
		t.Fatalf("recheck by a demoted actor: %d", resp.StatusCode)
	}
	e.gh.SetPermission("dave", repo, "maintain")
	resp, op = dave.send(opPath(op.ID)+"/recheck", "", `{}`)
	if resp.StatusCode != http.StatusOK || op.State != "succeeded" || !e.statusLabelsOn(repo) {
		t.Fatalf("recheck: %d %+v on=%v", resp.StatusCode, op, e.statusLabelsOn(repo))
	}
	_ = alice
}

// A recheck right after an unanswered enable does not fail it while labels may still be on their way, and a recheck
// that succeeds after another admin turned labels off does not turn them back on.
func TestStatusLabelsRecheckRespectsInFlightAndTurnOff(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/alice/app/labels" {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		next.ServeHTTP(w, r)
	})
	_, op := alice.send(statusLabelsPath(repo), "on-1", `{}`)
	if op.State != "unknown" {
		t.Fatalf("op=%+v", op)
	}
	e.intercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) { next.ServeHTTP(w, r) })
	if resp, again := alice.send(opPath(op.ID)+"/recheck", "", `{}`); resp.StatusCode != http.StatusConflict || again.State != "unknown" {
		t.Fatalf("early recheck: %d %+v", resp.StatusCode, again)
	}
	if resp := alice.disableStatusLabels(repo); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
	for _, l := range []string{"status:todo", "status:in-progress", "status:review"} {
		e.gh.AddLabel(repo, l)
	}
	if resp, done := alice.send(opPath(op.ID)+"/recheck", "", `{}`); resp.StatusCode != http.StatusOK || done.State != "succeeded" || e.statusLabelsOn(repo) {
		t.Fatalf("late recheck: %d %+v on=%v", resp.StatusCode, done, e.statusLabelsOn(repo))
	}
}

// The live conflict check's Board status (a conflict's current.status) agrees with the issue_board_states view, which
// the Board reads, on the same issues as projected from GitHub.
func TestBoardStatusAgreesWithView(t *testing.T) {
	e := setup(t)
	alice := e.signIn("alice")
	repo := e.repo("alice", "app", false)
	cases := []struct {
		state  string
		labels []string
		want   string
	}{
		{"open", nil, "backlog"},
		{"open", []string{"status:todo"}, "todo"},
		{"open", []string{"bug", "status:in-progress"}, "in_progress"},
		{"open", []string{"Status:Review", "status:todo"}, "in_review"},
		{"closed", []string{"status:review"}, "done"},
	}
	var seeds []fake.IssueFixture
	for i, c := range cases {
		seeds = append(seeds, boardIssue(int64(i+1), c.state, c.labels))
	}
	e.gh.SetIssues(repo, seeds...)
	for i, c := range cases {
		number := int64(i + 1)
		resp, _ := alice.send(changes(repo, number), fmt.Sprintf("conflict-%d", number), `{"labels":{"add":["seen"]},"expect":{"status":"backlog","assignees":["nobody"],"statusLabels":[]}}`)
		live := decodeConflict(t, resp).Current.Status
		// A confirmed update projects the issue as GitHub shows it; "seen" is not a status label.
		if resp, op := alice.send(changes(repo, number), fmt.Sprintf("project-%d", number), `{"labels":{"add":["seen"]}}`); op.State != "succeeded" {
			t.Fatalf("project #%d: %d %+v", number, resp.StatusCode, op)
		}
		var view string
		if err := e.pool.QueryRow(context.Background(), `SELECT v.status FROM issue_board_states v JOIN activity_issues i ON i.source_id = v.issue_id
			WHERE i.repository_id = $1 AND i.number = $2`, repo, number).Scan(&view); err != nil {
			t.Fatal(err)
		}
		if live != c.want || view != c.want {
			t.Fatalf("%s %v: live check %q, view %q, want %q", c.state, c.labels, live, view, c.want)
		}
	}
}
