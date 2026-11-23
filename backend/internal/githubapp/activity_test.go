package githubapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestActivityPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing authorization")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("Link", `</repositories/1183888342/pulls?state=all&per_page=100&page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":1,"number":2,"title":"PR","state":"closed","html_url":"url","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","merged_at":"2026-01-02T00:00:00Z","user":{"id":3,"login":"bot","type":"Bot"}}]`))
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = w.Write([]byte(`[{"id":4,"state":"PENDING","body":"private","html_url":"url","submitted_at":null,"user":{"id":3,"login":"bot","type":"Bot"}}]`))
		default:
			_, _ = w.Write([]byte(`[{"id":5,"body":"hello","html_url":"url","issue_url":"https://api.github.com/repos/a/b/issues/2","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","user":{"id":3,"login":"bot","type":"Bot"}}]`))
		}
	}))
	defer server.Close()
	c := New("", "", "", server.URL, "")
	ctx := context.Background()
	prs, next, err := c.FetchPullRequestsCursor(ctx, "token", "a/b", "")
	if err != nil || len(prs) != 1 || prs[0].MergedAt == nil || prs[0].User.Type != "Bot" || next != "state=all&per_page=100&page=2" {
		t.Fatalf("prs=%+v next=%q err=%v", prs, next, err)
	}
	reviews, _, err := c.FetchReviewsCursor(ctx, "token", "a/b", 2, "")
	if err != nil || len(reviews) != 1 || reviews[0].SubmittedAt != nil || reviews[0].Body != "private" {
		t.Fatalf("reviews=%+v err=%v", reviews, err)
	}
	comments, _, err := c.FetchRepoCommentsCursor(ctx, "token", "a/b", "", time.Time{})
	if err != nil || len(comments) != 1 || comments[0].Body != "hello" || comments[0].IssueNumber != 2 {
		t.Fatalf("comments=%+v err=%v", comments, err)
	}
}
func TestActivityRejectsInvalidPages(t *testing.T) {
	for _, tc := range []struct{ body, link string }{{"null", ""}, {"[{}]", ""}, {"[] trailing", ""}, {"[]", `<https://attacker.invalid/repos/a/b/pulls?state=all&page=2>; rel="next"`}, {"[]", `</repos/a/b/issues?state=all&page=2>; rel="next"`}, {"[]", `</repos/a/b/pulls?page=2>; rel="next"`}, {"[]", "bad link"}} {
		t.Run(tc.body+tc.link, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Link", tc.link)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := New("", "", "", server.URL, "")
			if _, _, err := c.FetchPullRequestsCursor(context.Background(), "token", "a/b", ""); err == nil {
				t.Fatal("accepted invalid page")
			}
		})
	}
}

func TestActivityAllowsNullActorsButRejectsMalformedActors(t *testing.T) {
	for _, actor := range []struct {
		json  string
		valid bool
	}{{"null", true}, {`{"id":0,"login":"","type":""}`, false}} {
		t.Run(actor.json, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls"):
					_, _ = w.Write([]byte(`[{"id":1,"number":2,"title":"PR","state":"open","html_url":"url","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","user":` + actor.json + `}]`))
				case strings.HasSuffix(r.URL.Path, "/reviews"):
					_, _ = w.Write([]byte(`[{"id":2,"state":"APPROVED","html_url":"url","submitted_at":"2026-01-01T00:00:00Z","user":` + actor.json + `}]`))
				default:
					_, _ = w.Write([]byte(`[{"id":3,"html_url":"url","issue_url":"https://api.github.com/repos/a/b/issues/2","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","user":` + actor.json + `}]`))
				}
			}))
			defer server.Close()
			c := New("", "", "", server.URL, "")
			ctx := context.Background()
			_, _, prErr := c.FetchPullRequestsCursor(ctx, "token", "a/b", "")
			_, _, reviewErr := c.FetchReviewsCursor(ctx, "token", "a/b", 2, "")
			_, _, commentErr := c.FetchRepoCommentsCursor(ctx, "token", "a/b", "", time.Time{})
			for _, err := range []error{prErr, reviewErr, commentErr} {
				if (err == nil) != actor.valid {
					t.Fatalf("valid=%v err=%v", actor.valid, err)
				}
			}
		})
	}
}

func TestRepoCommentsRejectsMissingIssueNumber(t *testing.T) {
	for _, issueURL := range []string{"", "https://api.github.com/repos/a/b/issues/", "https://api.github.com/repos/a/b/issues/x", "https://api.github.com/repos/a/b/pulls/3"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/a/b/issues/comments" || r.URL.Query().Get("since") != "2026-01-01T00:00:00Z" || r.URL.Query().Get("sort") != "updated" || r.URL.Query().Get("direction") != "asc" {
				t.Errorf("unexpected request %s", r.URL)
			}
			_, _ = w.Write([]byte(`[{"id":5,"body":"hello","html_url":"url","issue_url":"` + issueURL + `","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","user":null}]`))
		}))
		_, _, err := New("", "", "", server.URL, "").FetchRepoCommentsCursor(context.Background(), "token", "a/b", "", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		server.Close()
		if err == nil {
			t.Fatalf("issue_url %q accepted", issueURL)
		}
	}
}
