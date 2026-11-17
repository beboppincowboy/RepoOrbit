package graph

import (
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
)

func TestMetadataStatusMapsEveryFieldSeparatelyFromActivity(t *testing.T) {
	last := time.Date(2026, 11, 16, 9, 0, 0, 0, time.FixedZone("x", 3600))
	next := last.Add(15 * time.Minute)
	code := "github_unavailable"
	got := metadataStatus(ingestion.MetadataStatus{State: "queued", LastSuccessAt: &last, NextAttemptAt: &next, FailureCode: &code, FollowSuspended: true})
	if got.State != "queued" || got.Paused || !got.FollowSuspended || *got.LastSuccessAt != "2026-11-16T08:00:00Z" || *got.NextAttemptAt != "2026-11-16T08:15:00Z" || *got.FailureCode != code {
		t.Fatalf("%+v", got)
	}
	if s := metadataStatus(ingestion.MetadataStatus{State: "paused", Paused: true}); !s.Paused || s.LastSuccessAt != nil || s.NextAttemptAt != nil || s.FailureCode != nil {
		t.Fatalf("%+v", s)
	}
}

// Metadata success must never read as activity success.
func TestActivityIsNeverSynchronizedUntilActivityIngestionExists(t *testing.T) {
	if a := activityStatus(false, true); a.Synchronized || a.Message != "Selected, awaiting sync — no activity data yet." {
		t.Fatalf("%+v", a)
	}
	if a := activityStatus(false, false); a.Synchronized || a.Message == "" {
		t.Fatalf("%+v", a)
	}
	if a := activityStatus(true, true); a.Synchronized {
		t.Fatalf("%+v", a)
	}
}
