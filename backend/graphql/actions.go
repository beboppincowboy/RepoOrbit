package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/analytics"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

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

	if r.Demo {
		repo, err := r.Repository(ctx, id)
		if err != nil || repo == nil {
			return nil, err
		}
		out, err := analytics.LoadRepositoryActions(ctx, r.DemoPool, repoID, days, now)
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
		func() (*analytics.RepositoryActions, error) {
			out, err := analytics.LoadRepositoryActions(ctx, r.RepoAccess.Pool, auth.ids[0], days, now)
			if err != nil {
				return nil, fmt.Errorf("database unavailable")
			}
			return out, nil
		},
		func() (*analytics.RepositoryActions, error) {
			return nil, unverifiedError(time.Time{})
		},
	)
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

	if r.Demo {
		repo, err := r.Repository(ctx, repositoryID)
		if err != nil || repo == nil {
			return nil, err
		}
		out, err := analytics.LoadWorkflowRunDetail(ctx, r.DemoPool, repoID, runID)
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
		func() (*analytics.WorkflowRunDetail, error) {
			out, err := analytics.LoadWorkflowRunDetail(ctx, r.RepoAccess.Pool, auth.ids[0], runID)
			if err != nil {
				return nil, fmt.Errorf("database unavailable")
			}
			return out, nil
		},
		func() (*analytics.WorkflowRunDetail, error) {
			return nil, unverifiedError(time.Time{})
		},
	)
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

	if r.Demo {
		repo, err := r.Repository(ctx, repositoryID)
		if err != nil || repo == nil {
			return nil, err
		}
		out, err := analytics.LoadIssueDelivery(ctx, r.DemoPool, repoID, int64(issueNumber))
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
		func() (*analytics.IssueDelivery, error) {
			out, err := analytics.LoadIssueDelivery(ctx, r.RepoAccess.Pool, auth.ids[0], int64(issueNumber))
			if err != nil {
				return nil, fmt.Errorf("database unavailable")
			}
			return out, nil
		},
		func() (*analytics.IssueDelivery, error) {
			return nil, unverifiedError(time.Time{})
		},
	)
}
