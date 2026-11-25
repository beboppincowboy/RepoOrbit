package graph

import (
	"bytes"
	"encoding/json"
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

// Copies of the operations apps/web/src/lib/api.ts sends; keep them in sync.
const activityFieldsForTest = `synchronized message issues { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } pullRequests { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } releases { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } reviews { complete pendingChildren lastSuccessAt failureCode nextAttemptAt } comments { complete pendingChildren lastSuccessAt failureCode nextAttemptAt } issueEvents { traversal { phase boundary catchupThrough lastSuccessAt failureCode jobState nextAttemptAt } historicalGap } futureResources`

const repositoryOverviewQuery = `query RepositoryOverview($id: ID!, $days: Int!) {
    repositoryOverview(id: $id, days: $days) {
      fullName syncState activity { ` + activityFieldsForTest + ` archived }
      openIssues { value coverage { covered reason through } } staleOpenIssues { value coverage { covered reason through } } pullRequestsMerged { value coverage { covered reason through } }
      firstResponseTime { seconds sampleSize coverage { covered reason through } } mergeTime { seconds sampleSize coverage { covered reason through } } issuesWithoutResponse { value }
      activeBotContributors { value coverage { covered reason through } } contributorsCoverage { covered reason through }
      contributors { login isBot pullRequestsOpened reviewsSubmitted commentsCreated }
      windowStart eventsCoveredFrom
      daily { day issuesOpened pullRequestsMerged }
      timeline { sourceId kind occurredAt title number url actor { login isBot } }
    }
  }`

const workspacePullRequestsQuery = `query WorkspacePullRequests($id: ID!) {
    pullRequestEvidence(repositoryIds: [$id], first: 20) { nodes { id number title state url createdAt mergedAt reviewState author { login type } } withheld hasNextPage }
  }`

const workspaceIssuesQuery = `query WorkspaceIssues($id: ID!, $after: ID) {
    issues(repositoryIds: [$id], first: 20, after: $after) { nodes { id repositoryId number title state url authorLogin updatedAt labels assignees } withheld retryAt hasNextPage endCursor }
  }`

const repositoryBoardQuery = `query RepositoryBoard($id: ID!, $include: [Int!]) {
    repositoryBoard(id: $id, include: $include) {
      statusLabels truncated
      issues { number title url state updatedAt authorLogin labels assignees status multipleStatus }
    }
  }`

const crossOverviewQuery = `query CrossRepositoryOverview($days: Int! = 30) {
    crossRepositoryOverview(days: $days) {
      days windowStart now repositoriesCounted repositoriesSyncing withheld retryAt
      openIssues { value coverage { covered reason through } }
      reviewBacklog { value coverage { covered reason through } }
      needsAttention { kind repositoryId fullName number title url since }
      assignedToMe { repositoryId fullName number title url updatedAt status }
      daily { day issuesOpened pullRequestsMerged }
    }
  }`

func TestFrontendOperationsFitLimits(t *testing.T) {
	for name, query := range map[string]string{"overview": repositoryOverviewQuery, "pulls": workspacePullRequestsQuery, "issues": workspaceIssuesQuery, "cross": crossOverviewQuery, "board": repositoryBoardQuery} {
		body, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"id": "1", "days": 90}})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		Server(&Resolver{}).ServeHTTP(w, req)
		for _, rejected := range []string{"depth", "complexity", "at most one", "Cannot query field", "GRAPHQL_VALIDATION_FAILED"} {
			if strings.Contains(w.Body.String(), rejected) {
				t.Errorf("%s rejected by limits: %s", name, w.Body.String())
				break
			}
		}
	}
}

func TestCrossOverviewCountsAsOneOverview(t *testing.T) {
	body := strings.NewReader(`{"query":"query { a: crossRepositoryOverview(days: 7) { days } b: repositoryOverview(id: 1, days: 7) { days } }"}`)
	req := httptest.NewRequest(http.MethodPost, "/graphql", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	Server(&Resolver{}).ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "request at most one repository overview") {
		t.Fatalf("expected operation limit, got %s", w.Body.String())
	}
}

// Aliases cannot multiply the bounded Board read; one Board beside one overview is fine.
func TestRepositoryBoardAtMostOnePerOperation(t *testing.T) {
	run := func(query string) string {
		body, _ := json.Marshal(map[string]string{"query": query})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		Server(&Resolver{}).ServeHTTP(w, req)
		return w.Body.String()
	}
	for _, q := range []string{
		`query { a: repositoryBoard(id: 1) { truncated } b: repositoryBoard(id: 2) { truncated } }`,
		`query { a: repositoryBoard(id: 1) { truncated } ...B } fragment B on Query { b: repositoryBoard(id: 1, include: [2]) { truncated } }`,
	} {
		if got := run(q); !strings.Contains(got, "request at most one repository board") {
			t.Fatalf("expected operation limit for %s, got %s", q, got)
		}
	}
	if got := run(`query { repositoryBoard(id: 1) { truncated } repositoryOverview(id: 1) { days } }`); strings.Contains(got, "at most one") || strings.Contains(got, "GRAPHQL_VALIDATION_FAILED") {
		t.Fatalf("board beside an overview rejected: %s", got)
	}
}

func TestUnauthenticatedEmptyRepositoryIDsAndDemoEvidenceNilSafety(t *testing.T) {
	run := func(r *Resolver, query string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"query": query})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		Server(r).ServeHTTP(w, req)
		return w.Body.String()
	}

	for _, q := range []string{
		`{ issues(repositoryIds: []) { nodes { id } } }`,
		`{ pullRequestEvidence(repositoryIds: []) { nodes { id } } }`,
		`{ reviewEvidence(repositoryIds: []) { nodes { id } } }`,
		`{ commentEvidence(repositoryIds: []) { nodes { id } } }`,
		`{ releaseEvidence(repositoryIds: []) { nodes { id } } }`,
		`{ issueEventEvidence(repositoryIds: []) { nodes { id } } }`,
	} {
		if got := run(&Resolver{}, q); !strings.Contains(got, "UNAUTHENTICATED") {
			t.Fatalf("empty real-mode query %s did not fail UNAUTHENTICATED: %s", q, got)
		}
	}

	demo := &Resolver{Demo: true}
	for _, tc := range []struct {
		query, want string
	}{
		{`{ reviewEvidence(repositoryIds: ["1"]) { nodes { id } withheld retryAt } }`, `"reviewEvidence":{"nodes":[],"withheld":0,"retryAt":null}`},
		{`{ commentEvidence(repositoryIds: ["1"]) { nodes { id } withheld retryAt } }`, `"commentEvidence":{"nodes":[],"withheld":0,"retryAt":null}`},
		{`{ releaseEvidence(repositoryIds: ["1"]) { nodes { id } withheld retryAt } }`, `"releaseEvidence":{"nodes":[],"withheld":0,"retryAt":null}`},
		{`{ issueEventEvidence(repositoryIds: ["1"]) { nodes { id } withheld retryAt } }`, `"issueEventEvidence":{"nodes":[],"withheld":0,"retryAt":null}`},
	} {
		if got := run(demo, tc.query); !strings.Contains(got, tc.want) || strings.Contains(got, `"errors"`) {
			t.Fatalf("demo evidence %s: got %s, want %s", tc.query, got, tc.want)
		}
	}
	if got := run(demo, `{ reviewEvidence(repositoryIds: ["1"], first: 0) { nodes { id } } }`); !strings.Contains(got, `"errors"`) {
		t.Fatalf("demo evidence accepted invalid page size: %s", got)
	}
}
