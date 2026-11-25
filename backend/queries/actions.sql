-- name: ListWorkflows :many
SELECT * FROM workflows WHERE repository_id = $1 ORDER BY name, source_id;

-- name: ListWorkflowRuns :many
SELECT * FROM workflow_runs WHERE repository_id = $1 AND created_at >= $2 AND created_at <= $3
ORDER BY created_at DESC, source_id DESC;

-- name: GetWorkflowRun :one
SELECT * FROM workflow_runs WHERE repository_id = $1 AND source_id = $2;

-- name: ListWorkflowJobs :many
SELECT * FROM workflow_jobs WHERE repository_id = $1 AND run_id = $2 ORDER BY created_at, source_id;

-- name: ListDeployments :many
SELECT * FROM deployments WHERE repository_id = $1 ORDER BY created_at DESC, source_id DESC;

-- name: ListIssuePullRequestLinks :many
SELECT * FROM issue_pull_request_links WHERE repository_id = $1 AND issue_number = $2 ORDER BY pull_request_number;
