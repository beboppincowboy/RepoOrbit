package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/repoorbit/repoorbit/backend/graphql/model"
	"github.com/repoorbit/repoorbit/backend/internal/analytics"
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
		return &model.RepositoryOverview{Overview: analytics.UnsynchronizedOverview(days, now), FullName: repo.FullName, SyncState: repo.SyncState, Activity: repo.Activity}, nil
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
