package graph

import (
	"context"
	"strconv"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
)

// activityStatus is the honest activity answer: until activity ingestion exists nothing is synchronized, whatever
// the metadata refresh did. Demo rows are seeded samples, not GitHub data.
func activityStatus(demo, selected bool) *model.ActivityStatus {
	if demo {
		return &model.ActivityStatus{Synchronized: false, Message: "Demo sample data; not synchronized from GitHub.", FutureResources: []string{"pull requests", "reviews", "comments", "releases", "issue events"}}
	}
	if !selected {
		return &model.ActivityStatus{Synchronized: false, Message: "Not selected; nothing is synchronized.", FutureResources: []string{"pull requests", "reviews", "comments", "releases", "issue events"}}
	}
	return &model.ActivityStatus{Synchronized: false, Message: "Activity import in progress; other resources are not yet imported.", FutureResources: []string{"pull requests", "reviews", "comments", "releases", "issue events"}}
}

func rfc3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func metadataStatus(s ingestion.MetadataStatus) *model.MetadataStatus {
	return &model.MetadataStatus{State: s.State, LastSuccessAt: rfc3339(s.LastSuccessAt), NextAttemptAt: rfc3339(s.NextAttemptAt), Paused: s.Paused, FailureCode: s.FailureCode, FollowSuspended: s.FollowSuspended}
}

// attachMetadataStatus sets MetadataStatus on every node the user has selected. Reads the database only.
func (r *Resolver) attachMetadataStatus(ctx context.Context, userID int64, nodes []*model.Repository) error {
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		if id, err := strconv.ParseInt(n.ID, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	statuses, err := ingestion.LoadMetadataStatuses(ctx, r.Queries, userID, ids)
	if err != nil {
		return err
	}
	activity, err := r.Queries.ListActivityStatuses(ctx, ids)
	if err != nil {
		return err
	}
	byID := make(map[int64]*model.ActivityResourceStatus, len(activity))
	for _, row := range activity {
		state := "not-scheduled"
		if row.JobState.Valid {
			state = row.JobState.String
		}
		s := &model.ActivityResourceStatus{Phase: row.Phase, Boundary: row.Boundary.Time.UTC().Format(time.RFC3339), JobState: state}
		if row.Watermark.Valid {
			s.CatchupThrough = rfc3339(&row.Watermark.Time)
		}
		if row.LastSuccessAt.Valid {
			s.LastSuccessAt = rfc3339(&row.LastSuccessAt.Time)
		}
		if row.LastErrorCode.Valid {
			code := "unknown"
			switch row.LastErrorCode.String {
			case "no_credential", "rate_limited", "access_unverified", "access_changed", "upstream_error":
				code = row.LastErrorCode.String
			}
			s.FailureCode = &code
		}
		if state == "queued" && row.JobRunAt.Valid {
			s.NextAttemptAt = rfc3339(&row.JobRunAt.Time)
		}
		byID[row.RepositoryID] = s
	}
	for _, n := range nodes {
		id, _ := strconv.ParseInt(n.ID, 10, 64)
		if s, ok := statuses[id]; ok {
			n.MetadataStatus = metadataStatus(s)
		}
		if n.Activity != nil {
			n.Activity.Issues = byID[id]
		}
	}
	return nil
}
