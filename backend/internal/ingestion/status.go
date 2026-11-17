package ingestion

import (
	"context"
	"regexp"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/db"
)

// StatusNotScheduled is the State of a selected repository with no refresh job row yet.
const StatusNotScheduled = "not-scheduled"

// MetadataStatus is what the shared repository metadata refresh is doing. It says nothing about issues, pull
// requests, or activity history (see ActivityNotSynchronizedMessage). It carries no credential identity and no raw error.
type MetadataStatus struct {
	State           string     // StatusNotScheduled, or the job state: queued, running, paused, dead
	LastSuccessAt   *time.Time // last completed metadata refresh
	NextAttemptAt   *time.Time // when a queued refresh is due (retry or next poll); nil unless queued
	Paused          bool       // waiting for a usable credential or access; no remote calls are made meanwhile
	FailureCode     *string    // safe machine code from the last failed attempt, e.g. "github_unavailable"
	FollowSuspended bool       // the viewer's starred repository was unstarred; their selection is kept but not used
}

// ActivityNotSynchronizedMessage stays until activity ingestion exists: metadata success never implies it.
const ActivityNotSynchronizedMessage = "Selected, awaiting sync — no activity data yet."

var safeCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// StatusFromRow maps one joined row to the public status. A failure code that is not a short snake_case token is
// replaced by "unknown" so an unexpected writer can never leak raw error text through GraphQL.
func StatusFromRow(row db.ListMetadataStatusesRow) MetadataStatus {
	s := MetadataStatus{State: StatusNotScheduled, FollowSuspended: row.FollowSuspendedAt.Valid}
	if row.JobState.Valid {
		s.State = row.JobState.String
	}
	s.Paused = s.State == "paused"
	if s.State == "queued" && row.JobRunAt.Valid {
		t := row.JobRunAt.Time
		s.NextAttemptAt = &t
	}
	if row.LastSuccessAt.Valid {
		t := row.LastSuccessAt.Time
		s.LastSuccessAt = &t
	}
	if row.LastErrorCode.Valid && row.LastErrorCode.String != "" {
		code := "unknown"
		if safeCode.MatchString(row.LastErrorCode.String) {
			code = row.LastErrorCode.String
		}
		s.FailureCode = &code
	}
	return s
}

// LoadMetadataStatuses returns the status of each repository the user has actively selected, keyed by repository id.
// Repositories the user has not selected are absent. Reads the database only.
func LoadMetadataStatuses(ctx context.Context, q *db.Queries, userID int64, repositoryIDs []int64) (map[int64]MetadataStatus, error) {
	rows, err := q.ListMetadataStatuses(ctx, db.ListMetadataStatusesParams{UserID: userID, RepositoryIds: repositoryIDs})
	if err != nil {
		return nil, err
	}
	out := make(map[int64]MetadataStatus, len(rows))
	for _, row := range rows {
		out[row.RepositoryID] = StatusFromRow(row)
	}
	return out, nil
}
