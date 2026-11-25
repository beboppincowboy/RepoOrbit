//go:build integration

package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
	"github.com/repoorbit/repoorbit/backend/internal/testutil"
)

type boardIssueForTest struct {
	Number         int
	Title, State   string
	Labels         []string
	Assignees      []string
	Status         *string
	MultipleStatus bool
}

type boardForTest struct {
	StatusLabels, Truncated bool
	Issues                  []boardIssueForTest
}

func decodeBoard(t *testing.T, raw string) *boardForTest {
	t.Helper()
	var resp struct {
		Data struct{ RepositoryBoard *boardForTest }
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || strings.Contains(raw, `"errors"`) {
		t.Fatalf("board: %s (%v)", raw, err)
	}
	return resp.Data.RepositoryBoard
}

func (b *boardForTest) statuses() map[int]string {
	out := map[int]string{}
	for _, i := range b.Issues {
		s := "<null>"
		if i.Status != nil {
			s = *i.Status
		}
		out[i.Number] = s
	}
	return out
}

const boardFieldsForTest = `statusLabels truncated issues { number title url state updatedAt authorLogin labels assignees status multipleStatus }`

// Covers U6d tests 3 and 8: each issue's Board state comes from the issue_board_states view and its repository's
// status labels flag, the read is bounded and access-checked like the other readers, and Assigned to me carries
// the same status.
func TestRepositoryBoard(t *testing.T) {
	ctx := context.Background()
	h := newEvidenceHarness(t)
	pool, gh, run := h.pool, h.gh, h.run
	alice, bob := h.signIn("alice"), h.signIn("bob")
	aliceID := h.userID(ctx, "alice")
	var aliceGitHub int64
	if err := pool.QueryRow(ctx, `SELECT github_id FROM users WHERE login='alice'`).Scan(&aliceGitHub); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	connect := func(name string, installation int64) int64 {
		t.Helper()
		id := gh.AddRepo("alice", name, true)
		gh.Install("alice", installation, id)
		if err := h.repos.Connect(ctx, aliceID, id); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO activity_resources(repository_id,resource,boundary,watermark) VALUES ($1,'issues',now()-interval '90 days',now())`, id)
		return id
	}
	repo := connect("board", 9)
	exec(`INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,closed_at,author_id,author_login,author_type) VALUES
	 (1,$1,1,'plain','open','url/1',now(),now(),NULL,1,'alice','User'),
	 (2,$1,2,'todo','open','url/2',now(),now(),NULL,1,'alice','User'),
	 (3,$1,3,'progress','open','url/3',now(),now(),NULL,1,'alice','User'),
	 (4,$1,4,'review','open','url/4',now(),now(),NULL,1,'alice','User'),
	 (5,$1,5,'two labels','open','url/5',now(),now(),NULL,1,'alice','User'),
	 (6,$1,6,'closed with label','closed','url/6',now(),now(),now(),1,'alice','User')`, repo)
	exec(`INSERT INTO activity_issue_labels(issue_id,source_id,name) VALUES
	 (2,1,'status:todo'),(2,9,'bug'),(3,2,'Status:In-Progress'),(4,3,'status:review'),(5,1,'status:todo'),(5,3,'Status:Review'),(6,3,'status:review')`)
	exec(`INSERT INTO activity_issue_assignees(issue_id,source_id,login,type) VALUES (3,$1,'alice','User')`, aliceGitHub)
	board := fmt.Sprintf(`{ repositoryBoard(id:"%d") { %s } }`, repo, boardFieldsForTest)

	// Labels off: every issue is listed with a null status, and nothing is flagged.
	got := run(alice, board)
	b := decodeBoard(t, got)
	if b == nil || b.StatusLabels || b.Truncated || len(b.Issues) != 6 || strings.Count(got, `"status":null`) != 6 || strings.Contains(got, `"multipleStatus":true`) {
		t.Fatalf("labels off: %s", got)
	}

	// Turning labels on puts every open issue without a status label in Backlog on the next read (U6d test 3).
	exec(`UPDATE repositories SET status_labels=true WHERE id=$1`, repo)
	got = run(alice, board)
	b = decodeBoard(t, got)
	want := map[int]string{1: "backlog", 2: "todo", 3: "in_progress", 4: "in_review", 5: "in_review", 6: "done"}
	if b == nil || !b.StatusLabels || b.Truncated || fmt.Sprint(b.statuses()) != fmt.Sprint(want) {
		t.Fatalf("labels on: %s", got)
	}
	for _, i := range b.Issues {
		if i.MultipleStatus != (i.Number == 5) {
			t.Fatalf("#%d multipleStatus=%v: %s", i.Number, i.MultipleStatus, got)
		}
	}
	if !strings.Contains(got, `{"number":3,"title":"progress","url":"url/3","state":"open","updatedAt":"`) ||
		!strings.Contains(got, `"authorLogin":"alice","labels":["Status:In-Progress"],"assignees":["alice"],"status":"in_progress","multipleStatus":false}`) ||
		!strings.Contains(got, `"labels":[],"assignees":[],"status":"backlog"`) {
		t.Fatalf("issue fields: %s", got)
	}

	// Assigned to me carries the status the Board reports; a repository with labels off carries null (U6d test 8).
	off := connect("no-labels", 10)
	exec(`INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES (20,$1,1,'off','open','url',now(),now(),1,'alice','User')`, off)
	exec(`INSERT INTO activity_issue_labels(issue_id,source_id,name) VALUES (20,1,'status:todo')`)
	exec(`INSERT INTO activity_issue_assignees(issue_id,source_id,login,type) VALUES (20,$1,'alice','User')`, aliceGitHub)
	assigned := `{ crossRepositoryOverview(days: 7) { assignedToMe { title status } } }`
	if got := run(alice, assigned); !strings.Contains(got, `{"title":"progress","status":"in_progress"}`) || !strings.Contains(got, `{"title":"off","status":null}`) || strings.Contains(got, `"errors"`) {
		t.Fatalf("assigned to me: %s", got)
	}

	// Caps: the 50 most recently closed issues, with include numbers outside the caps returned once and truncated set.
	exec(`INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,closed_at,author_id,author_login,author_type)
	 SELECT 100+g,$1,100+g,'old '||g,'closed','url',now()-interval '1 day',now()-interval '1 day',now()-g*interval '1 hour',1,'alice','User' FROM generate_series(1,51) g`, repo)
	got = run(alice, fmt.Sprintf(`{ repositoryBoard(id:"%d", include:[151, 2, 151]) { %s } }`, repo, boardFieldsForTest))
	b = decodeBoard(t, got)
	count := map[int]int{}
	for _, i := range b.Issues {
		count[i.Number]++
	}
	if !b.Truncated || len(b.Issues) != 56 || count[151] != 1 || count[2] != 1 || count[6] != 1 || count[149] != 1 || count[150] != 0 {
		t.Fatalf("caps: truncated=%v n=%d counts=%v", b.Truncated, len(b.Issues), count)
	}
	tooMany := strings.TrimSuffix(strings.Repeat("1,", 101), ",")
	if got := run(alice, fmt.Sprintf(`{ repositoryBoard(id:"%d", include:[%s]) { truncated } }`, repo, tooMany)); !strings.Contains(got, `"repositoryBoard":null`) || !strings.Contains(got, "at most 100") {
		t.Fatalf("101 include numbers: %s", got)
	}

	// Another viewer, and a repository that does not exist, read null without an error.
	for _, q := range []string{board, `{ repositoryBoard(id:"999999") { truncated } }`} {
		if got := run(bob, q); !strings.Contains(got, `"repositoryBoard":null`) || strings.Contains(got, `"errors"`) {
			t.Fatalf("unselected board: %s", got)
		}
	}

	// Unverified access (a followed public repository whose check is rate limited) is ACCESS_UNVERIFIED with retryAt.
	pub := gh.AddRepo("carol", "followed", false)
	exec(`INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, 'carol/followed', '', false, false)`, pub)
	exec(`INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, 'starred', now())`, aliceID, pub)
	h.limitRepo.Store(pub)
	wantRetry := time.Unix(h.resetUnix, 0).UTC().Format(time.RFC3339)
	if got := run(alice, fmt.Sprintf(`{ repositoryBoard(id:"%d") { truncated } }`, pub)); !strings.Contains(got, `"repositoryBoard":null`) || !strings.Contains(got, "ACCESS_UNVERIFIED") || !strings.Contains(got, wantRetry) {
		t.Fatalf("unverified board: %s", got)
	}
	h.limitRepo.Store(0)

	// A fence move while the Board rows are read withholds the result.
	t.Run("fence_move_mid_read", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `LOCK TABLE activity_issues IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		result := make(chan string, 1)
		go func() { result <- run(alice, board) }()
		blocked := false
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !blocked; time.Sleep(10 * time.Millisecond) {
			if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%name: ListRepositoryBoard%')`).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
		}
		if !blocked {
			t.Fatal("board read did not block")
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
		if got := <-result; !strings.Contains(got, `"repositoryBoard":null`) || !strings.Contains(got, "ACCESS_UNVERIFIED") || strings.Contains(got, "progress") {
			t.Fatalf("fenced board leaked: %s", got)
		}
	})

	// An assigned issue in a repository the viewer lost access to stays out of Assigned to me, status labels or not.
	gone := connect("gone", 11)
	exec(`UPDATE repositories SET status_labels=true WHERE id=$1`, gone)
	exec(`INSERT INTO activity_issues(source_id,repository_id,number,title,state,html_url,created_at,updated_at,author_id,author_login,author_type) VALUES (30,$1,1,'gone issue','open','url',now(),now(),1,'alice','User')`, gone)
	exec(`INSERT INTO activity_issue_assignees(issue_id,source_id,login,type) VALUES (30,$1,'alice','User')`, aliceGitHub)
	if got := run(alice, assigned); !strings.Contains(got, "gone issue") {
		t.Fatalf("assigned before revocation: %s", got)
	}
	gh.Uninstall(11)
	_ = run(alice, assigned) // discovers the revocation, which moves the fence
	if got := run(alice, assigned); strings.Contains(got, "gone issue") || !strings.Contains(got, `"status":"in_progress"`) {
		t.Fatalf("assigned after revocation: %s", got)
	}
}

// Demo mode reads the seeded Board of orbit/example (status labels on, #7 with two labels) without an access check.
func TestDemoRepositoryBoard(t *testing.T) {
	ctx := context.Background()
	url, pool := testutil.Database(t)
	if err := platform.SeedDemo(ctx, url); err != nil {
		t.Fatal(err)
	}
	run := func(query string) string {
		body, _ := json.Marshal(map[string]string{"query": query})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		Server(&Resolver{Queries: db.New(pool), Demo: true, DemoPool: pool}).ServeHTTP(w, req)
		return w.Body.String()
	}
	got := run(`{ repositoryBoard(id:"1") { ` + boardFieldsForTest + ` } }`)
	b := decodeBoard(t, got)
	s := b.statuses()
	if !b.StatusLabels || b.Truncated || len(b.Issues) != 40 || s[1] != "backlog" || s[2] != "in_progress" || s[3] != "done" || s[7] != "in_review" {
		t.Fatalf("demo board: %s", got)
	}
	for _, i := range b.Issues {
		if i.MultipleStatus != (i.Number == 7) {
			t.Fatalf("#%d multipleStatus=%v", i.Number, i.MultipleStatus)
		}
	}
	if got := run(`{ repositoryBoard(id:"2") { statusLabels issues { status } } }`); !strings.Contains(got, `"statusLabels":false`) || strings.Contains(got, `"status":"`) {
		t.Fatalf("demo board without labels: %s", got)
	}
	if got := run(`{ repositoryBoard(id:"999") { truncated } }`); !strings.Contains(got, `"repositoryBoard":null`) || strings.Contains(got, `"errors"`) {
		t.Fatalf("missing demo board: %s", got)
	}
	got = run(`{ crossRepositoryOverview(days: 30) { assignedToMe { fullName number status } } }`)
	if !strings.Contains(got, `{"fullName":"orbit/example","number":8,"status":"in_progress"}`) || !strings.Contains(got, `{"fullName":"orbit/launchpad","number":8,"status":null}`) {
		t.Fatalf("demo assigned to me: %s", got)
	}
}
