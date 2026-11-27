package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

var ErrPermissionDenied = errors.New("github: permission denied")

type Workflow struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

type WorkflowRun struct {
	ID           int64      `json:"id"`
	WorkflowID   int64      `json:"workflow_id"`
	Name         string     `json:"name"`
	RunNumber    int64      `json:"run_number"`
	Event        string     `json:"event"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	HeadBranch   string     `json:"head_branch"`
	HeadSHA      string     `json:"head_sha"`
	HTMLURL      string     `json:"html_url"`
	CreatedAt    time.Time  `json:"created_at"`
	RunStartedAt *time.Time `json:"run_started_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	PullRequests []struct {
		Number int64 `json:"number"`
	} `json:"pull_requests"`
}

type WorkflowJob struct {
	ID          int64      `json:"id"`
	RunID       int64      `json:"run_id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	HTMLURL     string     `json:"html_url"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type Deployment struct {
	ID          int64     `json:"id"`
	Environment string    `json:"environment"`
	SHA         string    `json:"sha"`
	Ref         string    `json:"ref"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type DeploymentStatus struct {
	ID             int64     `json:"id"`
	State          string    `json:"state"`
	Description    string    `json:"description"`
	TargetURL      string    `json:"target_url"`
	EnvironmentURL string    `json:"environment_url"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (c *Client) deliveryPage(ctx context.Context, token, fullName, suffix, cursor string, out any) (string, error) {
	next, err := c.activityPage(ctx, token, fullName, suffix, cursor, false, out)
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusForbidden && !api.RateLimited() {
		return "", ErrPermissionDenied
	}
	return next, err
}

func (c *Client) FetchWorkflows(ctx context.Context, token, fullName, cursor string) ([]Workflow, string, error) {
	var page struct {
		Workflows []Workflow `json:"workflows"`
	}
	next, err := c.deliveryPage(ctx, token, fullName, "/actions/workflows", cursor, &page)
	if err != nil {
		return nil, "", err
	}
	if page.Workflows == nil {
		return nil, "", errors.New("github: invalid workflow listing")
	}
	for _, v := range page.Workflows {
		if v.ID <= 0 || v.UpdatedAt.IsZero() {
			return nil, "", errors.New("github: invalid workflow")
		}
	}
	return page.Workflows, next, nil
}

func (c *Client) FetchWorkflowRuns(ctx context.Context, token, fullName, cursor string) ([]WorkflowRun, string, error) {
	var page struct {
		Runs []WorkflowRun `json:"workflow_runs"`
	}
	next, err := c.deliveryPage(ctx, token, fullName, "/actions/runs", cursor, &page)
	if err != nil {
		return nil, "", err
	}
	if page.Runs == nil {
		return nil, "", errors.New("github: invalid workflow run listing")
	}
	for _, v := range page.Runs {
		if v.ID <= 0 || v.WorkflowID <= 0 || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
			return nil, "", errors.New("github: invalid workflow run")
		}
		for _, p := range v.PullRequests {
			if p.Number <= 0 {
				return nil, "", errors.New("github: invalid workflow run pull request")
			}
		}
	}
	return page.Runs, next, nil
}

func (c *Client) FetchWorkflowJobs(ctx context.Context, token, fullName string, runID int64, cursor string) ([]WorkflowJob, string, error) {
	if runID <= 0 {
		return nil, "", errors.New("github: invalid run ID")
	}
	var page struct {
		Jobs []WorkflowJob `json:"jobs"`
	}
	next, err := c.deliveryPage(ctx, token, fullName, "/actions/runs/"+strconv.FormatInt(runID, 10)+"/jobs", cursor, &page)
	if err != nil {
		return nil, "", err
	}
	if page.Jobs == nil {
		return nil, "", errors.New("github: invalid job listing")
	}
	for _, v := range page.Jobs {
		if v.ID <= 0 || v.RunID != runID || v.CreatedAt.IsZero() {
			return nil, "", errors.New("github: invalid workflow job")
		}
	}
	return page.Jobs, next, nil
}

func (c *Client) FetchDeployments(ctx context.Context, token, fullName, cursor string) ([]Deployment, string, error) {
	var items []Deployment
	next, err := c.deliveryPage(ctx, token, fullName, "/deployments", cursor, &items)
	if err != nil {
		return nil, "", err
	}
	if items == nil {
		return nil, "", errors.New("github: invalid deployments")
	}
	for _, v := range items {
		if v.ID <= 0 || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
			return nil, "", errors.New("github: invalid deployment")
		}
	}
	return items, next, nil
}

func (c *Client) FetchDeploymentStatuses(ctx context.Context, token, fullName string, deploymentID int64) ([]DeploymentStatus, error) {
	if deploymentID <= 0 {
		return nil, errors.New("github: invalid deployment ID")
	}
	var statuses []DeploymentStatus
	cursor := ""
	seen := map[string]bool{}
	for {
		var page []DeploymentStatus
		next, err := c.deliveryPage(ctx, token, fullName, "/deployments/"+strconv.FormatInt(deploymentID, 10)+"/statuses", cursor, &page)
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, errors.New("github: invalid deployment status listing")
		}
		for _, v := range page {
			if v.ID <= 0 || v.State == "" || v.UpdatedAt.IsZero() {
				return nil, errors.New("github: invalid deployment status")
			}
		}
		statuses = append(statuses, page...)
		if next == "" {
			return statuses, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("github: cyclic deployment status pagination")
		}
		seen[next] = true
		cursor = next
	}
}
