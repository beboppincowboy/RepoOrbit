package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/analytics"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

func readAuthorizedRepo[T any](r *queryResolver, ctx context.Context, rawID string, repoID int64, load func(*pgxpool.Pool, int64) (*T, error)) (*T, error) {
	if r.Demo {
		repo, err := r.Repository(ctx, rawID)
		if err != nil || repo == nil {
			return nil, err
		}
		out, err := load(r.DemoPool, repoID)
		if err != nil {
			return nil, fmt.Errorf("database unavailable")
		}
		return out, nil
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
		func() (*T, error) {
			out, err := load(r.RepoAccess.Pool, auth.ids[0])
			if err != nil {
				return nil, fmt.Errorf("database unavailable")
			}
			return out, nil
		},
		func() (*T, error) {
			return nil, unverifiedError(time.Time{})
		},
	)
}

func (r *queryResolver) repositoryActions(ctx context.Context, id string, days int) (*analytics.RepositoryActions, error) {
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
	return readAuthorizedRepo(r, ctx, id, repoID, func(pool *pgxpool.Pool, targetID int64) (*analytics.RepositoryActions, error) {
		return analytics.LoadRepositoryActions(ctx, pool, targetID, days, now)
	})
}

func (r *queryResolver) workflowRun(ctx context.Context, repositoryID, runIDStr string) (*analytics.WorkflowRunDetail, error) {
	repoID, err := parsePositiveID(&repositoryID)
	if err != nil {
		return nil, err
	}
	runID, err := parsePositiveID(&runIDStr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withReadDeadline(ctx)
	defer cancel()

	return readAuthorizedRepo(r, ctx, repositoryID, repoID, func(pool *pgxpool.Pool, targetID int64) (*analytics.WorkflowRunDetail, error) {
		return analytics.LoadWorkflowRunDetail(ctx, pool, targetID, runID)
	})
}

func (r *queryResolver) issueDelivery(ctx context.Context, repositoryID string, issueNumber int) (*analytics.IssueDelivery, error) {
	if issueNumber < 1 {
		return nil, fmt.Errorf("issueNumber must be positive")
	}
	repoID, err := parsePositiveID(&repositoryID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withReadDeadline(ctx)
	defer cancel()

	return readAuthorizedRepo(r, ctx, repositoryID, repoID, func(pool *pgxpool.Pool, targetID int64) (*analytics.IssueDelivery, error) {
		return analytics.LoadIssueDelivery(ctx, pool, targetID, int64(issueNumber))
	})
}
