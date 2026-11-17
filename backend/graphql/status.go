package graph

import (
	"context"
	"strconv"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
)

// activityStatus is the honest activity answer: until activity ingestion (U4) exists nothing is synchronized, whatever
// the metadata refresh did. Demo rows are seeded samples, not GitHub data.
func activityStatus(demo, selected bool) *model.ActivityStatus {
	if demo {
		return &model.ActivityStatus{Synchronized: false, Message: "Demo sample data; not synchronized from GitHub."}
	}
	if !selected {
		return &model.ActivityStatus{Synchronized: false, Message: "Not selected; nothing is synchronized."}
	}
	return &model.ActivityStatus{Synchronized: false, Message: ingestion.ActivityNotSynchronizedMessage}
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
	for _, n := range nodes {
		id, _ := strconv.ParseInt(n.ID, 10, 64)
		if s, ok := statuses[id]; ok {
			n.MetadataStatus = metadataStatus(s)
		}
	}
	return nil
}
