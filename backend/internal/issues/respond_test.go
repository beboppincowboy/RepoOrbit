package issues

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/issues/application"
)

// writeResult maps every typed result to today's status, body and RepoOrbit-Write-Disposition header: the header is
// written on status >= 400 when the disposition is set, and on a 500 only when it is possibly-recorded.
func TestWriteResultMapsStatusBodyAndDisposition(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	op := func(state string) application.Operation {
		return application.Operation{ID: 7, RepositoryID: 42, RepositoryName: "o/r", Kind: "update", State: state, IssueNumber: 3,
			Draft: []byte(`{"state":"closed"}`), Outcome: []byte(`{}`), ActorLogin: "alice", CreatedAt: at, UpdatedAt: at}
	}
	opJSON := func(state string) string {
		return `{"id":7,"repositoryId":42,"kind":"update","state":"` + state + `","issueNumber":3,"draft":{"state":"closed"},"outcome":{},"actor":"alice","createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}` + "\n"
	}
	const (
		none = application.DispositionNone
		nr   = application.DispositionNotRecorded
		pr   = application.DispositionPossiblyRecorded
	)
	for _, tc := range []struct {
		name   string
		res    application.Result
		status int
		body   string
		header string
	}{
		{"replay 200", application.Result{Kind: application.ResultOperation, Disposition: pr, Operation: op("succeeded")}, 200, opJSON("succeeded"), ""},
		{"created 201", application.Result{Kind: application.ResultOperation, Disposition: pr, Operation: op("succeeded"), Created: true}, 201, opJSON("succeeded"), ""},
		{"created unknown 202", application.Result{Kind: application.ResultOperation, Disposition: pr, Operation: op("unknown"), Created: true}, 202, opJSON("unknown"), ""},
		{"lost claim 200 stays 200 when unknown", application.Result{Kind: application.ResultOperation, Disposition: pr, Operation: op("unknown")}, 200, opJSON("unknown"), ""},
		{"recheck mismatch", application.Result{Kind: application.ResultMismatch, Operation: op("unknown")}, 409, opJSON("unknown"), ""},
		{"no content", application.Result{Kind: application.ResultNoContent}, 204, "", ""},
		{"not found", application.Result{Kind: application.ResultNotFound, Disposition: nr}, 404, "not found\n", "not-recorded"},
		{"issue not found", application.Result{Kind: application.ResultIssueNotFound, Disposition: nr}, 404, "issue not found\n", "not-recorded"},
		{"issue number required", application.Result{Kind: application.ResultIssueNumberRequired}, 400, "issueNumber is required to recheck a create\n", ""},
		{"access unverified", application.Result{Kind: application.ResultAccessUnverified, Disposition: nr}, 503, "access could not be verified; retry later\n", "not-recorded"},
		{"access check moved", application.Result{Kind: application.ResultAccessCheckMoved, Disposition: nr}, 503, "access changed while it was being checked; retry\n", "not-recorded"},
		{"load access moved", application.Result{Kind: application.ResultLoadAccessMoved}, 503, "access changed while loading; retry\n", ""},
		{"submit access moved", application.Result{Kind: application.ResultSubmitAccessMoved, Disposition: nr}, 503, "access changed while submitting; nothing was sent\n", "not-recorded"},
		{"recorded access moved", application.Result{Kind: application.ResultRecordedAccessMoved, Disposition: pr}, 503,
			"access changed; the operation was recorded and may have been sent: retry with the same Idempotency-Key\n", "possibly-recorded"},
		{"disable access moved", application.Result{Kind: application.ResultDisableAccessMoved}, 503, "access changed; retry\n", ""},
		{"key reuse", application.Result{Kind: application.ResultKeyReuse, Disposition: nr}, 422, "idempotency key reused with a different request\n", "not-recorded"},
		{"forbidden", application.Result{Kind: application.ResultForbidden, Disposition: nr}, 403, "your current GitHub permission does not allow this action\n", "not-recorded"},
		{"status labels forbidden", application.Result{Kind: application.ResultStatusLabelsForbidden}, 403, "only Admin or Maintain users can turn status labels off\n", ""},
		{"sign in required", application.Result{Kind: application.ResultSignInRequired, Disposition: nr}, 401, "sign in required\n", "not-recorded"},
		{"credential unavailable", application.Result{Kind: application.ResultCredentialUnavailable, Disposition: nr}, 503, "GitHub credential unavailable; retry later\n", "not-recorded"},
		{"upstream", application.Result{Kind: application.ResultUpstream, Disposition: nr}, 503, "GitHub could not be reached; nothing was sent\n", "not-recorded"},
		{"conflict", application.Result{Kind: application.ResultConflict, Disposition: nr, Current: application.Current{State: "open", Labels: []string{}, Assignees: []string{"bob"}, Status: "Todo", UpdatedAt: at}}, 409,
			`{"current":{"assignees":["bob"],"labels":[],"state":"open","status":"Todo","updatedAt":"2026-01-02T03:04:05Z"},"error":"changed on GitHub"}` + "\n", "not-recorded"},
		{"unassignable", application.Result{Kind: application.ResultUnassignable, Disposition: nr, Logins: []string{"bob", "carol"}}, 422, "cannot be assigned in this repository: bob, carol\n", "not-recorded"},
		{"duplicate create", application.Result{Kind: application.ResultDuplicateCreate, Disposition: nr, Message: "an earlier create of this issue is still unresolved; recheck it instead of resubmitting", OperationID: 9}, 409,
			`{"error":"an earlier create of this issue is still unresolved; recheck it instead of resubmitting","operationId":9}` + "\n", "not-recorded"},
		{"create changed", application.Result{Kind: application.ResultCreateChanged, Disposition: nr}, 503, "an earlier create of this issue changed while submitting; nothing was sent, retry\n", "not-recorded"},
		{"internal cleared", application.Result{Kind: application.ResultInternal, Disposition: none, Err: errors.New("boom")}, 500, "temporarily unavailable\n", ""},
		{"internal not recorded", application.Result{Kind: application.ResultInternal, Disposition: nr, Err: errors.New("boom")}, 500, "temporarily unavailable\n", ""},
		{"internal possibly recorded", application.Result{Kind: application.ResultInternal, Disposition: pr, Err: errors.New("boom")}, 500, "temporarily unavailable\n", "possibly-recorded"},
		// Recheck answers outside respond carry no disposition, so they get no header.
		{"recheck not found", application.Result{Kind: application.ResultNotFound}, 404, "not found\n", ""},
		{"recheck access unverified", application.Result{Kind: application.ResultAccessUnverified}, 503, "access could not be verified; retry later\n", ""},
		{"recheck access check moved", application.Result{Kind: application.ResultAccessCheckMoved}, 503, "access changed while it was being checked; retry\n", ""},
		{"recheck forbidden", application.Result{Kind: application.ResultForbidden}, 403, "your current GitHub permission does not allow this action\n", ""},
		{"recheck sign in required", application.Result{Kind: application.ResultSignInRequired}, 401, "sign in required\n", ""},
		{"recheck credential unavailable", application.Result{Kind: application.ResultCredentialUnavailable}, 503, "GitHub credential unavailable; retry later\n", ""},
		{"recheck upstream", application.Result{Kind: application.ResultUpstream}, 503, "GitHub could not be reached; nothing was sent\n", ""},
	} {
		rec := httptest.NewRecorder()
		(&Service{GitHubURL: "https://github.com"}).writeResult(rec, tc.res)
		if rec.Code != tc.status || rec.Body.String() != tc.body || rec.Header().Get(headerWriteDisposition) != tc.header {
			t.Errorf("%s: got %d %q header=%q; want %d %q header=%q", tc.name, rec.Code, rec.Body.String(), rec.Header().Get(headerWriteDisposition), tc.status, tc.body, tc.header)
		}
		if tc.res.Kind != application.ResultAccessUnverified && rec.Header().Get("Retry-After") != "" {
			t.Errorf("%s: unexpected Retry-After %q", tc.name, rec.Header().Get("Retry-After"))
		}
	}
}

func TestWriteResultRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Service{}).writeResult(rec, application.Result{Kind: application.ResultAccessUnverified, Disposition: application.DispositionNotRecorded, RetryAt: time.Now().Add(30 * time.Second)})
	if got := rec.Header().Get("Retry-After"); got != "30" && got != "29" {
		t.Fatalf("Retry-After %q, want 30 (29 on a slow run)", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}
