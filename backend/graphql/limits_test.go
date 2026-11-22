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

const crossOverviewQuery = `query CrossRepositoryOverview($days: Int! = 30) {
    crossRepositoryOverview(days: $days) {
      days windowStart now repositoriesCounted repositoriesSyncing withheld retryAt
      openIssues { value coverage { covered reason through } }
      reviewBacklog { value coverage { covered reason through } }
      needsAttention { kind repositoryId fullName number title url since }
      assignedToMe { repositoryId fullName number title url updatedAt }
      daily { day issuesOpened pullRequestsMerged }
    }
  }`

func TestFrontendOperationsFitLimits(t *testing.T) {
	for name, query := range map[string]string{"overview": repositoryOverviewQuery, "pulls": workspacePullRequestsQuery, "issues": workspaceIssuesQuery, "cross": crossOverviewQuery} {
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
