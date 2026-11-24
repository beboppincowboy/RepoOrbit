package ingestion

import (
	"context"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

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

// A per-issue comment job queued by a worker from before migration 00016 finishes as a no-op instead of going dead.
func TestSyncCommentsDropsPreMigrationPerIssueJob(t *testing.T) {
	if err := (&Deps{}).SyncComments(context.Background(), jobs.Job{Key: "repo:1:parent:2"}, nil); err != nil {
		t.Fatal(err)
	}
}
