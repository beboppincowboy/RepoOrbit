package graph

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
)

// activityStatus is the honest activity answer: synchronized is claimed only by attachEvidenceStatus, when every stream is caught up, whatever
// the metadata refresh did. Demo rows are seeded samples, not GitHub data.
func activityStatus(demo, selected bool) *model.ActivityStatus {
	if demo {
		return &model.ActivityStatus{Synchronized: false, Message: "Demo sample data; not synchronized from GitHub.", FutureResources: []string{"pull requests", "reviews", "comments", "releases", "issue events"}}
	}
	if !selected {
		return &model.ActivityStatus{Synchronized: false, Message: "Not selected; nothing is synchronized.", FutureResources: []string{}}
	}
	return &model.ActivityStatus{Synchronized: false, Message: "Activity import in progress; continuous reconciliation is not yet complete.", FutureResources: []string{}}
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

type statusFields struct {
	metadata bool
	activity bool
}

func collectRepositoryStatusFields(fields []graphql.CollectedField, want *statusFields) {
	for _, f := range fields {
		switch f.Name {
		case "metadataStatus":
			want.metadata = true
		case "activity":
			want.activity = true
		}
	}
}

func repositoryStatusSelection(ctx context.Context) statusFields {
	if !graphql.HasOperationContext(ctx) {
		return statusFields{metadata: true, activity: true}
	}
	var want statusFields
	collectRepositoryStatusFields(graphql.CollectFieldsCtx(ctx, []string{"Repository"}), &want)
	return want
}

func selectedRepositoryStatusSelection(ctx context.Context) statusFields {
	if !graphql.HasOperationContext(ctx) {
		return statusFields{metadata: true, activity: true}
	}
	op := graphql.GetOperationContext(ctx)
	var want statusFields
	for _, f := range graphql.CollectFieldsCtx(ctx, []string{"RepositoryConnection"}) {
		if f.Name != "nodes" {
			continue
		}
		collectRepositoryStatusFields(graphql.CollectFields(op, f.Selections, []string{"Repository"}), &want)
	}
	return want
}

func overviewActivitySelected(ctx context.Context) bool {
	if !graphql.HasOperationContext(ctx) {
		return true
	}
	for _, f := range graphql.CollectFieldsCtx(ctx, []string{"RepositoryOverview"}) {
		if f.Name == "activity" {
			return true
		}
	}
	return false
}

func parseNodeIDs(nodes []*model.Repository) []int64 {
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		if id, err := strconv.ParseInt(n.ID, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *Resolver) loadSelectedStatuses(ctx context.Context, userID int64, nodes []*model.Repository, want statusFields) error {
	if len(nodes) == 0 {
		return nil
	}
	if want.metadata {
		if err := r.loadMetadataStatus(ctx, userID, nodes); err != nil {
			return err
		}
	}
	if want.activity {
		if err := r.loadActivityStatus(ctx, nodes); err != nil {
			return err
		}
	}
	return nil
}

func (r *Resolver) loadMetadataStatus(ctx context.Context, userID int64, nodes []*model.Repository) error {
	if len(nodes) == 0 {
		return nil
	}
	ids := parseNodeIDs(nodes)
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

func (r *Resolver) loadActivityStatus(ctx context.Context, nodes []*model.Repository) error {
	if len(nodes) == 0 {
		return nil
	}
	ids := parseNodeIDs(nodes)
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
		if n.Activity != nil {
			n.Activity.Issues = byID[id]
			if r.RepoAccess != nil {
				if err := r.attachEvidenceStatus(ctx, id, n.Activity); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// attachEvidenceStatus derives child completeness only after every relevant parent traversal has caught up.
func (r *Resolver) attachEvidenceStatus(ctx context.Context, id int64, a *model.ActivityStatus) error {
	resourceStatus := func(resource, kind string) (*model.ActivityResourceStatus, error) {
		var boundary time.Time
		var phase, state string
		var watermark, success, next *time.Time
		var code *string
		err := r.RepoAccess.Pool.QueryRow(ctx, `SELECT a.boundary,a.phase,a.watermark,a.last_success_at,a.last_error_code,COALESCE(j.state,'not-scheduled'),CASE WHEN j.state='queued' THEN j.run_at END FROM activity_resources a LEFT JOIN jobs j ON j.kind=$3 AND j.key='repo:'||a.repository_id::text WHERE a.repository_id=$1 AND a.resource=$2`, id, resource, kind).Scan(&boundary, &phase, &watermark, &success, &code, &state, &next)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if resource == "pull_requests" && code != nil && *code == ingestion.CodePullRequestsDisabled {
			return &model.ActivityResourceStatus{Phase: "disabled", Boundary: stamp(boundary), JobState: state}, nil
		}
		return &model.ActivityResourceStatus{Phase: phase, Boundary: stamp(boundary), CatchupThrough: rfc3339(watermark), LastSuccessAt: rfc3339(success), FailureCode: safeActivityCode(code), JobState: state, NextAttemptAt: rfc3339(next)}, nil
	}
	var err error
	if a.PullRequests, err = resourceStatus("pull_requests", "activity_pull_requests"); err != nil {
		return err
	}
	if a.Releases, err = resourceStatus("releases", "activity_releases"); err != nil {
		return err
	}
	traversal, err := resourceStatus("issue_events", "activity_issue_events")
	if err != nil {
		return err
	}
	if traversal != nil {
		// GitHub retains issue events for a limited window and never emits an "opened" event, so history
		// before the retained window is a permanent gap that traversal cannot close.
		a.IssueEvents = &model.IssueEventStatus{Traversal: traversal, HistoricalGap: true}
	}
	parentComplete := func(s *model.ActivityResourceStatus) bool {
		return s != nil && s.Phase == "current" && s.CatchupThrough != nil
	}
	var pending int
	var childSuccess, childNext *time.Time
	var childCode *string
	err = r.RepoAccess.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT c.complete), CASE WHEN count(*) > 0 AND count(c.last_success_at) = count(*) THEN min(c.last_success_at) END, (array_agg(c.last_error_code ORDER BY c.parent_id) FILTER (WHERE c.last_error_code IS NOT NULL))[1], min(j.run_at) FILTER (WHERE j.state='queued') FROM activity_children c LEFT JOIN jobs j ON j.kind='activity_reviews' AND j.key='repo:'||c.repository_id::text||':parent:'||c.parent_id::text WHERE c.repository_id=$1 AND c.resource='reviews'`, id).Scan(&pending, &childSuccess, &childCode, &childNext)
	if err != nil {
		return err
	}
	complete := (parentComplete(a.PullRequests) || (a.PullRequests != nil && a.PullRequests.Phase == "disabled")) && pending == 0
	a.Reviews = &model.ChildResourceStatus{Complete: complete, PendingChildren: pending, LastSuccessAt: rfc3339(childSuccess), FailureCode: safeActivityCode(childCode), NextAttemptAt: rfc3339(childNext)}
	// Comments are one repository-wide stream; the child shape is kept so clients need no change.
	comments, err := resourceStatus("comments", "activity_comments")
	if err != nil {
		return err
	}
	if comments != nil {
		a.Comments = &model.ChildResourceStatus{Complete: parentComplete(comments) && comments.JobState != "dead" && comments.JobState != "paused", LastSuccessAt: comments.LastSuccessAt, FailureCode: comments.FailureCode, NextAttemptAt: comments.NextAttemptAt}
	}
	if err = r.RepoAccess.Pool.QueryRow(ctx, `SELECT archived FROM repositories WHERE id=$1`, id).Scan(&a.Archived); err != nil {
		return err
	}
	if synchronized(a, time.Now()) {
		a.Synchronized, a.Message = true, "All activity streams are synchronized."
	}
	return nil
}

// synchronized is true only when every stream is caught up, fresh, failure-free and every child history is complete.
// The permanent issue-event gap is reported separately and does not block it. Any stale, failed, paused or dead
// stream keeps the repository from claiming synchronization.
func synchronized(a *model.ActivityStatus, now time.Time) bool {
	fresh := func(s *model.ActivityResourceStatus) bool {
		if s == nil || s.Phase != "current" || s.CatchupThrough == nil || s.LastSuccessAt == nil || s.FailureCode != nil || s.JobState == "dead" || s.JobState == "paused" {
			return false
		}
		t, err := time.Parse(time.RFC3339, *s.LastSuccessAt)
		return err == nil && now.Sub(t) <= 2*ingestion.MetadataPollInterval
	}
	child := func(c *model.ChildResourceStatus) bool { return c != nil && c.Complete && c.FailureCode == nil }
	if a.IssueEvents == nil {
		return false
	}
	return fresh(a.Issues) && (fresh(a.PullRequests) || (a.PullRequests != nil && a.PullRequests.Phase == "disabled")) && fresh(a.Releases) && fresh(a.IssueEvents.Traversal) && child(a.Reviews) && child(a.Comments)
}

func safeActivityCode(code *string) *string {
	if code == nil {
		return nil
	}
	switch *code {
	case "no_credential", "rate_limited", "access_unverified", "access_changed", "upstream_error":
		return code
	}
	unknown := "unknown"
	return &unknown
}
