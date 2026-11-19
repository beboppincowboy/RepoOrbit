package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssueWriteCalls(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.EscapedPath())
		switch r.URL.EscapedPath() {
		case "/repos/o/r/assignees/yes":
			w.WriteHeader(http.StatusNoContent)
		case "/repos/o/r/assignees/down":
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New("", "", "", server.URL, "")
	ctx := context.Background()
	if ok, err := c.IsAssignable(ctx, "t", "o/r", "yes"); !ok || err != nil {
		t.Fatalf("204: %v %v", ok, err)
	}
	if ok, err := c.IsAssignable(ctx, "t", "o/r", "no"); ok || err != nil {
		t.Fatalf("404 is a definite no: %v %v", ok, err)
	}
	var api *APIError
	if ok, err := c.IsAssignable(ctx, "t", "o/r", "down"); ok || !errors.As(err, &api) || api.Status != http.StatusBadGateway {
		t.Fatalf("5xx is unverified, not a no: %v %v", ok, err)
	}
	if err := c.RemoveLabel(ctx, "t", "o/r", 3, "needs review/ui"); err != nil {
		t.Fatalf("a label not on the issue is already removed: %v", err)
	}
	if got[len(got)-1] != "DELETE /repos/o/r/issues/3/labels/needs%20review%2Fui" {
		t.Fatalf("label not escaped into one path segment: %v", got)
	}
	if _, err := c.CreateIssue(ctx, "t", "o/r/x", NewIssue{Title: "x"}); err == nil {
		t.Fatal("accepted a malformed repository name")
	}
}
