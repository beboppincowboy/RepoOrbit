package ingestion

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

func sTs(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func sTxt(s string) pgtype.Text          { return pgtype.Text{String: s, Valid: true} }

func TestStatusFromRow(t *testing.T) {
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		row  db.ListMetadataStatusesRow
		want MetadataStatus
	}{
		{"never scheduled", db.ListMetadataStatusesRow{}, MetadataStatus{State: StatusNotScheduled}},
		{"queued shows retry timing and last success", db.ListMetadataStatusesRow{JobState: sTxt("queued"), JobRunAt: sTs(at), LastSuccessAt: sTs(at.Add(-time.Hour)), LastErrorCode: sTxt("github_unavailable")},
			MetadataStatus{State: "queued", NextAttemptAt: &at, LastSuccessAt: sPtr(at.Add(-time.Hour)), FailureCode: sPtr("github_unavailable")}},
		{"running has no next attempt", db.ListMetadataStatusesRow{JobState: sTxt("running"), JobRunAt: sTs(at)}, MetadataStatus{State: "running"}},
		{"paused is flagged", db.ListMetadataStatusesRow{JobState: sTxt("paused"), LastErrorCode: sTxt("no_credential")}, MetadataStatus{State: "paused", Paused: true, FailureCode: sPtr("no_credential")}},
		{"dead keeps its code", db.ListMetadataStatusesRow{JobState: sTxt("dead"), LastErrorCode: sTxt("repository_gone")}, MetadataStatus{State: "dead", FailureCode: sPtr("repository_gone")}},
		{"suspended follow", db.ListMetadataStatusesRow{FollowSuspendedAt: sTs(at)}, MetadataStatus{State: StatusNotScheduled, FollowSuspended: true}},
		{"unsafe code is never exposed", db.ListMetadataStatusesRow{JobState: sTxt("dead"), LastErrorCode: sTxt("Get \"https://api.github.com/x\": token ghu_secret")}, MetadataStatus{State: "dead", FailureCode: sPtr("unknown")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StatusFromRow(c.row)
			if !equalStatus(got, c.want) {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func sPtr[T any](v T) *T { return &v }

func equalStatus(a, b MetadataStatus) bool {
	eq := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	es := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.State == b.State && a.Paused == b.Paused && a.FollowSuspended == b.FollowSuspended && eq(a.LastSuccessAt, b.LastSuccessAt) && eq(a.NextAttemptAt, b.NextAttemptAt) && es(a.FailureCode, b.FailureCode)
}
