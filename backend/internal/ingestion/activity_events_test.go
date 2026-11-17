package ingestion

import "testing"

func TestSupportedIssueEvent(t *testing.T) {
	for _, event := range []string{"closed", "reopened", "assigned", "unassigned", "labeled", "unlabeled"} {
		if !supportedIssueEvent(event) {
			t.Errorf("%s must be imported", event)
		}
	}
	for _, event := range []string{"opened", "renamed", "", "CLOSED"} {
		if supportedIssueEvent(event) {
			t.Errorf("%s must not be inferred", event)
		}
	}
}
