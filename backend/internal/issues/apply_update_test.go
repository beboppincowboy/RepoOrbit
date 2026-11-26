package issues

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// AllStepsAnswered is true exactly when classify calls the failing step "failed" (or nothing failed), and the update
// stops at the first error.
func TestApplyUpdateAllStepsAnswered(t *testing.T) {
	for _, tc := range []struct {
		status   int
		answered bool
		err      string
	}{
		{http.StatusOK, true, ""},
		{http.StatusNotFound, true, "GitHub rejected the request (status 404)"},
		{http.StatusUnprocessableEntity, true, "GitHub rejected the request (status 422)"},
		{http.StatusBadGateway, false, "GitHub answered status 502; the change may or may not have been applied"},
	} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte("{}"))
		}))
		s := &Service{GitHub: githubapp.New("", "", srv.URL, srv.URL, "")}
		d := &updateDraft{State: "open", Labels: domain.Delta{Add: []string{"bug"}}}
		out := s.applyUpdate(context.Background(), 1, "token", "o/r", 7, d)
		srv.Close()
		if out.AllStepsAnswered != tc.answered || out.Error != tc.err {
			t.Errorf("status %d: answered=%v error=%q", tc.status, out.AllStepsAnswered, out.Error)
		}
		want := int32(2) // reopen, add labels
		if tc.status >= 400 {
			want = 1 // stopped at the failing reopen
		}
		if got := calls.Load(); got != want {
			t.Errorf("status %d: %d GitHub calls, want %d", tc.status, got, want)
		}
	}
}
