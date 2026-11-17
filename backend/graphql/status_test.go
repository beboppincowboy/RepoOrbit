package graph

import (
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
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
	if a := activityStatus(false, true); a.Synchronized || a.Message == "" {
		t.Fatalf("%+v", a)
	}
	if a := activityStatus(false, false); a.Synchronized || a.Message == "" {
		t.Fatalf("%+v", a)
	}
	if a := activityStatus(true, true); a.Synchronized {
		t.Fatalf("%+v", a)
	}
}

func TestSynchronizedRequiresEveryStreamFreshAndComplete(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ok := func() *model.ActivityResourceStatus {
		at, th := now.Add(-time.Minute).Format(time.RFC3339), now.Add(-time.Minute).Format(time.RFC3339)
		return &model.ActivityResourceStatus{Phase: "current", CatchupThrough: &th, LastSuccessAt: &at, JobState: "queued"}
	}
	full := func() *model.ActivityStatus {
		return &model.ActivityStatus{Issues: ok(), PullRequests: ok(), Releases: ok(), IssueEvents: &model.IssueEventStatus{Traversal: ok(), HistoricalGap: true},
			Reviews: &model.ChildResourceStatus{Complete: true}, Comments: &model.ChildResourceStatus{Complete: true}}
	}
	if !synchronized(full(), now) {
		t.Fatal("fully caught-up repository must be synchronized despite the permanent gap")
	}
	code := "upstream_error"
	for name, mutate := range map[string]func(*model.ActivityStatus){
		"stale": func(a *model.ActivityStatus) {
			old := now.Add(-time.Hour).Format(time.RFC3339)
			a.Releases.LastSuccessAt = &old
		},
		"failed":   func(a *model.ActivityStatus) { a.Issues.FailureCode = &code },
		"dead":     func(a *model.ActivityStatus) { a.PullRequests.JobState = "dead" },
		"catchup":  func(a *model.ActivityStatus) { a.IssueEvents.Traversal.Phase = "catchup" },
		"children": func(a *model.ActivityStatus) { a.Comments.Complete = false },
		"missing":  func(a *model.ActivityStatus) { a.Releases = nil },
	} {
		a := full()
		mutate(a)
		if synchronized(a, now) {
			t.Errorf("%s must not be synchronized", name)
		}
	}
}
