package graph

import (
	"context"
	"strconv"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
)

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

type parsedRepoNode struct {
	id   int64
	node *model.Repository
}

func parseNodeIDs(nodes []*model.Repository) ([]int64, []parsedRepoNode) {
	ids := make([]int64, 0, len(nodes))
	parsed := make([]parsedRepoNode, 0, len(nodes))
	for _, n := range nodes {
		if id, err := strconv.ParseInt(n.ID, 10, 64); err == nil {
			ids = append(ids, id)
			parsed = append(parsed, parsedRepoNode{id: id, node: n})
		}
	}
	return ids, parsed
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
	ids, parsed := parseNodeIDs(nodes)
	statuses, err := ingestion.LoadMetadataStatuses(ctx, r.Queries, userID, ids)
	if err != nil {
		return err
	}
	for _, p := range parsed {
		if s, ok := statuses[p.id]; ok {
			p.node.MetadataStatus = metadataStatus(s)
		}
	}
	return nil
}

func (r *Resolver) loadActivityStatus(ctx context.Context, nodes []*model.Repository) error {
	if len(nodes) == 0 {
		return nil
	}
	ids, parsed := parseNodeIDs(nodes)
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
			s.FailureCode = safeActivityCode(&row.LastErrorCode.String)
		}
		if state == "queued" && row.JobRunAt.Valid {
			s.NextAttemptAt = rfc3339(&row.JobRunAt.Time)
		}
		byID[row.RepositoryID] = s
	}
	active := make([]parsedRepoNode, 0, len(parsed))
	for _, p := range parsed {
		if p.node.Activity != nil {
			p.node.Activity.Issues = byID[p.id]
			active = append(active, p)
		}
	}
	if r.RepoAccess != nil && len(active) > 0 {
		if err := r.attachEvidenceStatuses(ctx, active); err != nil {
			return err
		}
	}
	return nil
}

func (r *Resolver) attachEvidenceStatuses(ctx context.Context, nodes []parsedRepoNode) error {
	ids := make([]int64, len(nodes))
	for i, p := range nodes {
		ids[i] = p.id
	}

	type resourceKey struct {
		repoID   int64
		resource string
	}
	resources := make(map[resourceKey]*model.ActivityResourceStatus, len(nodes)*4)
	resRows, err := r.RepoAccess.Pool.Query(ctx, `SELECT a.repository_id,a.resource,a.boundary,a.phase,a.watermark,a.last_success_at,a.last_error_code,COALESCE(j.state,'not-scheduled'),CASE WHEN j.state='queued' THEN j.run_at END FROM activity_resources a LEFT JOIN jobs j ON j.kind='activity_'||a.resource AND j.key='repo:'||a.repository_id::text WHERE a.repository_id=ANY($1::bigint[]) AND a.resource IN ('pull_requests','releases','issue_events','comments')`, ids)
	if err != nil {
		return err
	}
	for resRows.Next() {
		var repoID int64
		var resource, phase, state string
		var boundary time.Time
		var watermark, success, next *time.Time
		var code *string
		if err := resRows.Scan(&repoID, &resource, &boundary, &phase, &watermark, &success, &code, &state, &next); err != nil {
			resRows.Close()
			return err
		}
		if resource == "pull_requests" && code != nil && *code == ingestion.CodePullRequestsDisabled {
			resources[resourceKey{repoID: repoID, resource: resource}] = &model.ActivityResourceStatus{Phase: "disabled", Boundary: stamp(boundary), JobState: state}
			continue
		}
		resources[resourceKey{repoID: repoID, resource: resource}] = &model.ActivityResourceStatus{Phase: phase, Boundary: stamp(boundary), CatchupThrough: rfc3339(watermark), LastSuccessAt: rfc3339(success), FailureCode: safeActivityCode(code), JobState: state, NextAttemptAt: rfc3339(next)}
	}
	resRows.Close()
	if err := resRows.Err(); err != nil {
		return err
	}

	type reviewSummary struct {
		pending int
		success *time.Time
		code    *string
		next    *time.Time
	}
	reviews := make(map[int64]reviewSummary, len(nodes))
	revRows, err := r.RepoAccess.Pool.Query(ctx, `SELECT c.repository_id,count(*) FILTER (WHERE NOT c.complete), CASE WHEN count(*) > 0 AND count(c.last_success_at) = count(*) THEN min(c.last_success_at) END, (array_agg(c.last_error_code ORDER BY c.parent_id) FILTER (WHERE c.last_error_code IS NOT NULL))[1], min(j.run_at) FILTER (WHERE j.state='queued') FROM activity_children c LEFT JOIN jobs j ON j.kind='activity_reviews' AND j.key='repo:'||c.repository_id::text||':parent:'||c.parent_id::text WHERE c.repository_id=ANY($1::bigint[]) AND c.resource='reviews' GROUP BY c.repository_id`, ids)
	if err != nil {
		return err
	}
	for revRows.Next() {
		var repoID int64
		var s reviewSummary
		if err := revRows.Scan(&repoID, &s.pending, &s.success, &s.code, &s.next); err != nil {
			revRows.Close()
			return err
		}
		reviews[repoID] = s
	}
	revRows.Close()
	if err := revRows.Err(); err != nil {
		return err
	}

	archived := make(map[int64]bool, len(nodes))
	archRows, err := r.RepoAccess.Pool.Query(ctx, `SELECT id, archived FROM repositories WHERE id=ANY($1::bigint[])`, ids)
	if err != nil {
		return err
	}
	for archRows.Next() {
		var repoID int64
		var isArchived bool
		if err := archRows.Scan(&repoID, &isArchived); err != nil {
			archRows.Close()
			return err
		}
		archived[repoID] = isArchived
	}
	archRows.Close()
	if err := archRows.Err(); err != nil {
		return err
	}

	parentComplete := func(s *model.ActivityResourceStatus) bool {
		return s != nil && s.Phase == "current" && s.CatchupThrough != nil
	}
	now := time.Now()
	for _, p := range nodes {
		a := p.node.Activity
		a.PullRequests = resources[resourceKey{repoID: p.id, resource: "pull_requests"}]
		a.Releases = resources[resourceKey{repoID: p.id, resource: "releases"}]
		if traversal := resources[resourceKey{repoID: p.id, resource: "issue_events"}]; traversal != nil {
			// GitHub retains issue events for a limited window and never emits an "opened" event, so history
			// before the retained window is a permanent gap that traversal cannot close.
			a.IssueEvents = &model.IssueEventStatus{Traversal: traversal, HistoricalGap: true}
		}
		rev := reviews[p.id]
		complete := (parentComplete(a.PullRequests) || (a.PullRequests != nil && a.PullRequests.Phase == "disabled")) && rev.pending == 0
		a.Reviews = &model.ChildResourceStatus{Complete: complete, PendingChildren: rev.pending, LastSuccessAt: rfc3339(rev.success), FailureCode: safeActivityCode(rev.code), NextAttemptAt: rfc3339(rev.next)}
		if comments := resources[resourceKey{repoID: p.id, resource: "comments"}]; comments != nil {
			a.Comments = &model.ChildResourceStatus{Complete: parentComplete(comments) && comments.JobState != "dead" && comments.JobState != "paused", LastSuccessAt: comments.LastSuccessAt, FailureCode: comments.FailureCode, NextAttemptAt: comments.NextAttemptAt}
		}
		isArchived, ok := archived[p.id]
		if !ok {
			return pgx.ErrNoRows
		}
		a.Archived = isArchived
		if synchronized(a, now) {
			a.Synchronized, a.Message = true, "All activity streams are synchronized."
		}
	}
	return nil
}

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
