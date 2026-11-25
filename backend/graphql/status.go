package graph

import (
	"context"
	"strconv"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/db"
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
	if len(nodes) == 0 || (!want.metadata && !want.activity) {
		return nil
	}
	ids, parsed := parseNodeIDs(nodes)
	if len(ids) == 0 {
		return nil
	}

	var active []parsedRepoNode
	var activeIDs []int64
	if want.activity {
		active = parsed
		activeIDs = ids
		for _, p := range parsed {
			if p.node.Activity == nil {
				active = make([]parsedRepoNode, 0, len(parsed))
				activeIDs = make([]int64, 0, len(parsed))
				for _, q := range parsed {
					if q.node.Activity != nil {
						active = append(active, q)
						activeIDs = append(activeIDs, q.id)
					}
				}
				break
			}
		}
	}

	wantActivity := want.activity && len(active) > 0
	if !want.metadata && !wantActivity {
		return nil
	}

	var batch pgx.Batch
	if want.metadata {
		batch.Queue(db.ListMetadataStatusesSQL, userID, ids)
	}
	if wantActivity {
		batch.Queue(db.ListActivityStatusesSQL, activeIDs)
		batch.Queue(db.ListReviewStatusesSQL, activeIDs)
		batch.Queue(db.ListRepositoryArchivedSQL, activeIDs)
	}

	br := r.RepoAccess.Pool.SendBatch(ctx, &batch)
	defer br.Close()

	if want.metadata {
		statuses, err := scanMetadataStatuses(br, len(ids))
		if err != nil {
			return err
		}
		for _, p := range parsed {
			if s, ok := statuses[p.id]; ok {
				p.node.MetadataStatus = metadataStatus(s)
			}
		}
	}

	if wantActivity {
		if err := readActivityStatusBatch(br, active); err != nil {
			return err
		}
	}

	return br.Close()
}

func (r *Resolver) loadActivityStatus(ctx context.Context, nodes []*model.Repository) error {
	return r.loadSelectedStatuses(ctx, 0, nodes, statusFields{activity: true})
}

func scanMetadataStatuses(br pgx.BatchResults, capHint int) (map[int64]ingestion.MetadataStatus, error) {
	rows, err := br.Query()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]ingestion.MetadataStatus, capHint)
	for rows.Next() {
		var row db.ListMetadataStatusesRow
		if err := rows.Scan(
			&row.RepositoryID,
			&row.FollowSuspendedAt,
			&row.JobState,
			&row.JobRunAt,
			&row.LastSuccessAt,
			&row.LastErrorCode,
		); err != nil {
			return nil, err
		}
		out[row.RepositoryID] = ingestion.StatusFromRow(row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readActivityStatusBatch(br pgx.BatchResults, nodes []parsedRepoNode) error {
	type resourceKey struct {
		repoID   int64
		resource string
	}
	resources := make(map[resourceKey]*model.ActivityResourceStatus, len(nodes)*5)
	resRows, err := br.Query()
	if err != nil {
		return err
	}
	for resRows.Next() {
		var repoID int64
		var resource, phase string
		var boundary time.Time
		var watermark, success, next *time.Time
		var code, jobState *string
		if err := resRows.Scan(&repoID, &resource, &boundary, &phase, &watermark, &success, &code, &jobState, &next); err != nil {
			resRows.Close()
			return err
		}
		state := ingestion.StatusNotScheduled
		if jobState != nil {
			state = *jobState
		}
		if resource == "pull_requests" && code != nil && *code == ingestion.CodePullRequestsDisabled {
			resources[resourceKey{repoID: repoID, resource: resource}] = &model.ActivityResourceStatus{Phase: "disabled", Boundary: stamp(boundary), JobState: state}
			continue
		}
		resources[resourceKey{repoID: repoID, resource: resource}] = &model.ActivityResourceStatus{
			Phase:          phase,
			Boundary:       stamp(boundary),
			CatchupThrough: rfc3339(watermark),
			LastSuccessAt:  rfc3339(success),
			FailureCode:    safeActivityCode(code),
			JobState:       state,
			NextAttemptAt:  rfc3339(next),
		}
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
	revRows, err := br.Query()
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
	archRows, err := br.Query()
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
		a.Issues = resources[resourceKey{repoID: p.id, resource: "issues"}]
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
