package issues

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/issues/application"
	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// The gateway returns every real GitHub client failure as a *application.RemoteError of the right kind, still
// reachable as *githubapp.APIError, and classifying it gives today's state and reason (application sets
// AllStepsAnswered exactly when the state is "failed").
func TestGatewayMapsClientErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   domain.ErrorKind
		state  string
		reason string
	}{
		{http.StatusOK, 0, "", ""},
		{http.StatusUnauthorized, domain.Unauthorized, "failed", "GitHub rejected the request (status 401)"},
		{http.StatusNotFound, domain.NotFound, "failed", "GitHub rejected the request (status 404)"},
		{http.StatusUnprocessableEntity, domain.HTTPError, "failed", "GitHub rejected the request (status 422)"},
		{http.StatusBadGateway, domain.HTTPError, "unknown", "GitHub answered status 502; the change may or may not have been applied"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte("{}"))
		}))
		g := githubGateway{githubapp.New("", "", srv.URL, srv.URL, "")}
		err := g.SetIssueState(context.Background(), "token", "o/r", 7, "open")
		srv.Close()
		if tc.status == http.StatusOK {
			if err != nil {
				t.Errorf("status 200: got %v, want nil", err)
			}
			continue
		}
		var re *application.RemoteError
		var api *githubapp.APIError
		if !errors.As(err, &re) || re.Kind != tc.kind || re.Status != tc.status || !errors.As(err, &api) || api.Status != tc.status {
			t.Errorf("status %d: got %#v", tc.status, err)
			continue
		}
		if state, reason := domain.Classify(re.Kind, re.Status); state != tc.state || reason != tc.reason {
			t.Errorf("status %d: got %q %q", tc.status, state, reason)
		}
	}
	if err := remoteError(githubapp.ErrNotSent); !errors.Is(err, githubapp.ErrNotSent) {
		t.Errorf("not sent: got %v", err)
	} else if re := (*application.RemoteError)(nil); !errors.As(err, &re) || re.Kind != domain.NotSent || re.Status != 0 {
		t.Errorf("not sent: got %#v", err)
	}
}

// FetchIssue builds the rules' view of the issue and keeps the full GitHub issue as the settlement payload.
func TestGatewayFetchIssue(t *testing.T) {
	var g githubGateway
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":70,"number":7,"title":"T","state":"open","html_url":"https://github.test/o/r/issues/7","repository_url":"` + g.c.APIURL +
			`/repos/O/R","user":{"id":5,"login":"alice"},"labels":[{"name":"bug"}],"assignees":[{"login":"bob"}]}`))
	}))
	defer srv.Close()
	g = githubGateway{githubapp.New("", "", srv.URL, srv.URL, "")}
	issue, err := g.FetchIssue(context.Background(), "token", "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := issue.Payload.(githubapp.Issue)
	if !ok || payload.Number != 7 || issue.AuthorID != 5 || !issue.InRepository || issue.PullRequest ||
		issue.View.Labels[0] != "bug" || issue.View.Assignees[0] != "bob" || issue.View.State != "open" {
		t.Fatalf("got %+v view=%+v", issue, issue.View)
	}
}

// status_labels.go and queries.go still hand application.MapConcurrent raw client errors; a raw 401 must still win.
func TestMapConcurrentRawUnauthorizedWins(t *testing.T) {
	ready := make(chan struct{})
	_, err := application.MapConcurrent(context.Background(), []int{0, 1}, func(ctx context.Context, n int) (int, error) {
		if n == 0 {
			close(ready)
			return 0, errors.New("upstream 502")
		}
		<-ready
		return 0, &githubapp.APIError{Status: http.StatusUnauthorized}
	})
	var api *githubapp.APIError
	if !errors.As(err, &api) || !api.Unauthorized() {
		t.Fatalf("got %v, want the Unauthorized error", err)
	}
}
