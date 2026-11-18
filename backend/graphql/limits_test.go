package graph

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPageLimit(t *testing.T) {
	for _, n := range []int{0, -1, 101} {
		if _, err := pageLimit(n); err == nil {
			t.Fatalf("accepted %d", n)
		}
	}
	if n, err := pageLimit(100); err != nil || n != 100 {
		t.Fatalf("valid page: %d %v", n, err)
	}
}

func TestOverviewRejectsMultipleWindowsInOneOperation(t *testing.T) {
	body := strings.NewReader(`{"query":"query { first: repositoryOverview(id: 1, days: 7) { days } ...Second } fragment Second on Query { second: repositoryOverview(id: 1, days: 30) { days } }"}`)
	req := httptest.NewRequest(http.MethodPost, "/graphql", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	Server(&Resolver{}).ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "request at most one repository overview") {
		t.Fatalf("expected operation limit, got %s", w.Body.String())
	}
}
