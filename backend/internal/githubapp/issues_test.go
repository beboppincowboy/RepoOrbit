package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchIssuesPage(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/example/project/issues" || r.URL.Query().Get("state") != "all" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing authorization")
		}
		w.Header().Set("Link", "<"+server.URL+"/repositories/1183888342/issues?state=all&page=3>; rel=\"next\"")
		_, _ = w.Write([]byte(`[{"id":123,"number":42,"title":"Example","state":"open","html_url":"https://github.com/example/project/issues/42","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","user":{"id":7,"login":"robot","type":"Bot"},"labels":[{"id":8,"name":"bug"}],"assignees":[{"id":9,"login":"person","type":"User"}]},{"id":124,"number":43,"pull_request":{"url":"https://api.github.com/repos/example/project/pulls/43"}}]`))
	}))
	defer server.Close()
	c := New("", "", "", server.URL, "")
	issues, next, err := c.FetchIssuesCursor(context.Background(), "test-token", "example/project", "state=all&per_page=100&page=2")
	if err != nil || next != "state=all&page=3" || len(issues) != 2 {
		t.Fatalf("issues=%+v next=%v err=%v", issues, next, err)
	}
	if issues[0].ID != 123 || issues[0].User.Type != "Bot" || len(issues[0].Labels) != 1 || len(issues[0].Assignees) != 1 || issues[1].PullRequest == nil {
		t.Fatalf("source evidence lost: %+v", issues)
	}
}

func TestFetchIssuesPageRejectsNull(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`null`)) }))
	defer server.Close()
	c := New("", "", "", server.URL, "")
	if _, _, err := c.FetchIssuesCursor(context.Background(), "token", "example/project", ""); err == nil {
		t.Fatal("null is not a completed issue page")
	}
}

func TestIssueCursorFollowsAndValidatesNextLink(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(fmt.Sprint(unsafe), func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "2" {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				target := server.URL + "/repos/a/b/issues?state=all&per_page=100&page=2"
				if unsafe {
					target = "https://attacker.invalid/repos/a/b/issues?page=2"
				}
				w.Header().Set("Link", "<"+target+">; rel=\"next\"")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer server.Close()
			c := New("", "", "", server.URL, "")
			_, cursor, err := c.FetchIssuesCursor(context.Background(), "token", "a/b", "")
			if unsafe {
				if err == nil {
					t.Fatal("accepted foreign pagination link")
				}
				return
			}
			if err != nil || cursor != "state=all&per_page=100&page=2" {
				t.Fatalf("cursor=%q err=%v", cursor, err)
			}
			_, next, err := c.FetchIssuesCursor(context.Background(), "token", "a/b", cursor)
			if err != nil || next != "" {
				t.Fatalf("next=%q err=%v", next, err)
			}
		})
	}
}
