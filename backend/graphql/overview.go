package graph

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/analytics"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

func (r *queryResolver) repositoryOverview(ctx context.Context, id string, days int) (*model.RepositoryOverview, error) {
	if err := analytics.ValidateDays(days); err != nil {
		return nil, err
	}
	ctx, cancel := platform.WithRequestDeadline(ctx)
	defer cancel()

	clock := r.Clock
	if clock == nil {
		clock = jobs.SystemClock{}
	}
	now := clock.Now()

	if r.Demo {
		repo, err := r.Repository(ctx, id)
		if err != nil || repo == nil {
			return nil, err
		}
		repoID, _ := strconv.ParseInt(id, 10, 64)
		overview, err := analytics.Load(ctx, r.DemoPool, repoID, days, now)
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		return &model.RepositoryOverview{Overview: overview, FullName: repo.FullName, SyncState: repo.SyncState, Activity: repo.Activity}, nil
	}

	page, err := r.evidenceAccess(ctx, []string{id}, 1, nil)
	if err != nil {
		return nil, err
	}
	if page.withheld > 0 {
		var retry time.Time
		if page.retry != nil {
			retry, _ = time.Parse(time.RFC3339, *page.retry)
		}
		return nil, unverifiedError(retry)
	}
	if len(page.ids) == 0 {
		return nil, nil
	}
	overview, err := analytics.Load(ctx, r.RepoAccess.Pool, page.ids[0], days, now)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	repo := &model.Repository{ID: id, Activity: activityStatus(false, true)}
	if err := r.attachMetadataStatus(ctx, page.userID, []*model.Repository{repo}); err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	if !r.evidenceCurrent(ctx, page) {
		return nil, unverifiedError(time.Time{})
	}
	return &model.RepositoryOverview{
		Overview: overview, FullName: page.checks[page.ids[0]].Repo.FullName,
		SyncState: string(repositories.SyncStateFor(true)), Activity: repo.Activity,
	}, nil
}

// demoAssigneeID is the GitHub user ID testdata/demo.sql assigns demo issues to, standing in for auth.DemoViewer.
const demoAssigneeID = 1

// crossRepositoryOverview summarizes the viewer's selected repositories that pass the access check, under one fence:
// the rest are counted in Withheld without being named, and a fence move discards everything (ACCESS_UNVERIFIED).
// Demo mode summarizes the demo repositories without an access check.
func (r *queryResolver) crossRepositoryOverview(ctx context.Context, days int) (*model.CrossRepositoryOverview, error) {
	if err := analytics.ValidateDays(days); err != nil {
		return nil, err
	}
	ctx, cancel := platform.WithRequestDeadline(ctx)
	defer cancel()
	clock := r.Clock
	if clock == nil {
		clock = jobs.SystemClock{}
	}
	now := clock.Now()

	if r.Demo {
		rows, err := r.Queries.ListDemoRepositories(ctx, db.ListDemoRepositoriesParams{After: 0, PageSize: maxRepositoryIDs})
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		out, err := analytics.LoadCross(ctx, r.DemoPool, ids, demoAssigneeID, days, now)
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		return &model.CrossRepositoryOverview{CrossOverview: out}, nil
	}

	user, err := r.userID(ctx)
	if err != nil {
		return nil, err
	}
	selected, err := r.Queries.ListAllSelectedIDs(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	if len(selected) > maxRepositoryIDs {
		return nil, fmt.Errorf("the overview supports at most %d selected repositories", maxRepositoryIDs)
	}
	raw := make([]string, len(selected))
	for i, id := range selected {
		raw[i] = strconv.FormatInt(id, 10)
	}
	page, err := r.evidenceAccess(ctx, raw, 1, nil)
	if err != nil {
		return nil, err
	}
	assignee, err := r.Queries.GetUserGithubID(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	out, err := analytics.LoadCross(ctx, r.RepoAccess.Pool, page.ids, assignee, days, now)
	if err != nil {
		return nil, fmt.Errorf("database unavailable")
	}
	if !r.evidenceCurrent(ctx, page) {
		return nil, unverifiedError(time.Time{})
	}
	return &model.CrossRepositoryOverview{CrossOverview: out, Withheld: len(selected) - len(page.ids), RetryAt: page.retry}, nil
}
