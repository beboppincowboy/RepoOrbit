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
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

func (r *queryResolver) repositoryOverview(ctx context.Context, id string, days int) (*model.RepositoryOverview, error) {
	if err := analytics.ValidateMetricWindowDays(days); err != nil {
		return nil, err
	}
	repoID, err := parsePositiveID(&id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withReadDeadline(ctx)
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
		overview, err := analytics.LoadRepositoryOverview(ctx, r.DemoPool, repoID, days, now)
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		return &model.RepositoryOverview{Overview: overview, FullName: repo.FullName, SyncState: repo.SyncState, Activity: repo.Activity}, nil
	}

	auth, err := r.authorizeSelected(ctx, []int64{repoID})
	if err != nil {
		return nil, err
	}
	if len(auth.ids) == 0 {
		if auth.withheld > 0 {
			return nil, unverifiedError(auth.retryAt)
		}
		return nil, nil
	}
	return guardedRead(ctx, r.RepoAccess.Pool, auth,
		func() (*model.RepositoryOverview, error) {
			targetID := auth.ids[0]
			overview, err := analytics.LoadRepositoryOverview(ctx, r.RepoAccess.Pool, targetID, days, now)
			if err != nil {
				return nil, fmt.Errorf("database unavailable")
			}
			repo := &model.Repository{ID: strconv.FormatInt(targetID, 10), Activity: activityStatus(false, true)}
			if overviewActivitySelected(ctx) {
				if err := r.loadActivityStatus(ctx, []*model.Repository{repo}); err != nil {
					return nil, fmt.Errorf("database unavailable")
				}
			}
			return &model.RepositoryOverview{
				Overview: overview, FullName: auth.checks[targetID].Repo.FullName,
				SyncState: string(repositories.SyncStateFor(true)), Activity: repo.Activity,
			}, nil
		},
		func() (*model.RepositoryOverview, error) {
			return nil, unverifiedError(time.Time{})
		},
	)
}

// demoAssigneeID is the GitHub user ID testdata/demo.sql assigns demo issues to, standing in for auth.DemoViewer.
const demoAssigneeID = 1

// crossRepositoryOverview summarizes the viewer's selected repositories that pass the access check, under one fence:
// unverified repositories are counted in Withheld without being named, and a fence move discards everything (ACCESS_UNVERIFIED).
// Demo mode summarizes the demo repositories without an access check.
func (r *queryResolver) crossRepositoryOverview(ctx context.Context, days int) (*model.CrossRepositoryOverview, error) {
	if err := analytics.ValidateMetricWindowDays(days); err != nil {
		return nil, err
	}
	ctx, cancel := withReadDeadline(ctx)
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
		out, err := analytics.LoadCrossRepositoryOverview(ctx, r.DemoPool, ids, demoAssigneeID, days, now)
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
		return nil, codedError("OVERVIEW_SELECTION_LIMIT", fmt.Sprintf("The overview supports up to %d selected repositories. Disconnect or unfollow repositories to view your overview.", maxRepositoryIDs))
	}
	auth, err := r.authorizePreselected(ctx, user, selected)
	if err != nil {
		return nil, err
	}
	return guardedRead(ctx, r.RepoAccess.Pool, auth,
		func() (*model.CrossRepositoryOverview, error) {
			var assignee int64
			if len(auth.ids) > 0 {
				var err error
				assignee, err = r.Queries.GetUserGithubID(ctx, user)
				if err != nil {
					return nil, fmt.Errorf("database unavailable")
				}
			}
			out, err := analytics.LoadCrossRepositoryOverview(ctx, r.RepoAccess.Pool, auth.ids, assignee, days, now)
			if err != nil {
				return nil, fmt.Errorf("database unavailable")
			}
			return &model.CrossRepositoryOverview{CrossOverview: out, Withheld: auth.withheld, RetryAt: auth.retryAtString()}, nil
		},
		func() (*model.CrossRepositoryOverview, error) {
			return nil, unverifiedError(time.Time{})
		},
	)
}
