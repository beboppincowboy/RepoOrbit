package issues

import (
	"errors"
	"fmt"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

// classify maps GitHub client errors onto domain.Classify; applyUpdate sets AllStepsAnswered exactly when it says failed.
func TestClassifyMapsGitHubErrors(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		state, reason string
	}{
		{"not sent", fmt.Errorf("add labels: %w", githubapp.ErrNotSent), "failed", "the request could not be prepared; nothing was sent to GitHub"},
		{"unauthorized", fmt.Errorf("wrapped: %w", &githubapp.APIError{Status: 401}), "failed", "GitHub rejected the request (status 401)"},
		{"not found", &githubapp.APIError{Status: 404}, "failed", "GitHub rejected the request (status 404)"},
		{"5xx", &githubapp.APIError{Status: 502}, "unknown", "GitHub answered status 502; the change may or may not have been applied"},
		{"transport", errors.New("connection reset"), "unknown", "no reliable answer from GitHub; the change may or may not have been applied"},
		{"nil", nil, "unknown", "no reliable answer from GitHub; the change may or may not have been applied"},
	} {
		if state, reason := classify(tc.err); state != tc.state || reason != tc.reason {
			t.Errorf("%s: got %q %q", tc.name, state, reason)
		}
	}
}
