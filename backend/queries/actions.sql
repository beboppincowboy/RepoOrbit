-- name: GetWorkflowRun :one
SELECT * FROM workflow_runs WHERE repository_id = $1 AND source_id = $2;

-- name: ListWorkflowJobs :many
SELECT * FROM workflow_jobs WHERE repository_id = $1 AND run_id = $2 ORDER BY created_at, source_id;

-- name: ListDeployments :many
SELECT * FROM deployments WHERE repository_id = $1 ORDER BY created_at DESC, source_id DESC LIMIT 50;

-- name: HasDeployment :one
SELECT EXISTS (SELECT 1 FROM deployments WHERE repository_id = $1)::boolean;

-- name: GetActionsCoverage :one
SELECT
  sa.boundary AS actions_boundary,
  sa.watermark AS actions_watermark,
  COALESCE(sa.last_error_code, '')::text AS actions_error_code,
  sd.boundary AS deployments_boundary,
  sd.watermark AS deployments_watermark,
  COALESCE(sd.last_error_code, '')::text AS deployments_error_code
FROM (SELECT 1) AS one
LEFT JOIN activity_resources sa ON sa.repository_id = sqlc.arg(repository_id) AND sa.resource = 'actions'
LEFT JOIN activity_resources sd ON sd.repository_id = sqlc.arg(repository_id) AND sd.resource = 'deployments';

-- name: GetActionsMetrics :one
WITH windowed_runs AS (
  SELECT
    wr.repository_id,
    wr.source_id,
    wr.workflow_id,
    wr.status,
    wr.conclusion,
    wr.created_at,
    wr.run_started_at,
    wr.updated_at
  FROM workflow_runs wr
  WHERE wr.repository_id = sqlc.arg(repository_id)
    AND wr.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
), run_stats AS (
  SELECT
    count(*) FILTER (WHERE wr.status = 'completed' AND wr.conclusion <> '')::bigint AS completed_runs,
    count(*) FILTER (WHERE wr.status = 'completed' AND wr.conclusion = 'success')::bigint AS succeeded_runs,
    count(*) FILTER (WHERE wr.status = 'completed' AND wr.run_started_at IS NOT NULL AND wr.updated_at >= wr.run_started_at)::bigint AS duration_sample,
    COALESCE(floor(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM wr.updated_at - wr.run_started_at)::float8)
      FILTER (WHERE wr.status = 'completed' AND wr.run_started_at IS NOT NULL AND wr.updated_at >= wr.run_started_at)), 0)::bigint AS duration_p50_seconds,
    COALESCE(floor(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM wr.updated_at - wr.run_started_at)::float8)
      FILTER (WHERE wr.status = 'completed' AND wr.run_started_at IS NOT NULL AND wr.updated_at >= wr.run_started_at)), 0)::bigint AS duration_p95_seconds,
    count(*) FILTER (WHERE wr.run_started_at IS NOT NULL AND wr.run_started_at >= wr.created_at)::bigint AS queue_wait_sample,
    COALESCE(floor(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM wr.run_started_at - wr.created_at)::float8)
      FILTER (WHERE wr.run_started_at IS NOT NULL AND wr.run_started_at >= wr.created_at)), 0)::bigint AS queue_wait_p50_seconds
  FROM windowed_runs wr
), latest_per_workflow AS (
  SELECT DISTINCT ON (wr.workflow_id) wr.workflow_id, wr.status, wr.conclusion
  FROM windowed_runs wr
  ORDER BY wr.workflow_id, wr.created_at DESC, wr.source_id DESC
), job_stats AS (
  SELECT
    count(*) FILTER (WHERE j.status = 'completed' AND j.conclusion = 'failure')::bigint AS failed_jobs,
    count(*) FILTER (WHERE j.status <> 'completed')::bigint AS running_jobs
  FROM workflow_jobs j
  JOIN windowed_runs r ON r.repository_id = j.repository_id AND r.source_id = j.run_id
)
SELECT
  rs.completed_runs,
  rs.succeeded_runs,
  rs.duration_sample,
  rs.duration_p50_seconds,
  rs.duration_p95_seconds,
  rs.queue_wait_sample,
  rs.queue_wait_p50_seconds,
  js.failed_jobs,
  js.running_jobs,
  (SELECT count(*)::bigint FROM latest_per_workflow lpw WHERE lpw.status = 'completed' AND lpw.conclusion = 'failure') AS failing_workflows
FROM run_stats rs CROSS JOIN job_stats js;

-- name: ListActionsDaily :many
WITH days AS (
  SELECT ((sqlc.arg(window_start)::timestamptz AT TIME ZONE 'UTC')::date + g)::date AS day
  FROM generate_series(0, sqlc.arg(days)::int - 1) AS g
), runs AS (
  SELECT
    (created_at AT TIME ZONE 'UTC')::date AS day,
    count(*) FILTER (WHERE status = 'completed' AND conclusion = 'success')::bigint AS succeeded,
    count(*) FILTER (WHERE status = 'completed' AND conclusion = 'failure')::bigint AS failed,
    count(*) FILTER (WHERE status = 'completed' AND conclusion <> '' AND conclusion NOT IN ('success', 'failure'))::bigint AS other
  FROM workflow_runs
  WHERE repository_id = sqlc.arg(repository_id)
    AND created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
  GROUP BY 1
)
SELECT
  d.day,
  COALESCE(r.succeeded, 0)::bigint AS succeeded,
  COALESCE(r.failed, 0)::bigint AS failed,
  COALESCE(r.other, 0)::bigint AS other
FROM days d
LEFT JOIN runs r ON r.day = d.day
ORDER BY d.day;

-- name: ListWorkflowFailures :many
WITH windowed_runs AS (
  SELECT *
  FROM workflow_runs
  WHERE repository_id = sqlc.arg(repository_id)
    AND created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
), agg AS (
  SELECT
    workflow_id,
    count(*)::bigint AS total_runs,
    count(*) FILTER (WHERE status = 'completed' AND conclusion = 'failure')::bigint AS failed_runs
  FROM windowed_runs
  GROUP BY workflow_id
), latest AS (
  SELECT DISTINCT ON (workflow_id)
    workflow_id, workflow_name, source_id AS latest_run_id, status AS latest_status,
    conclusion AS latest_conclusion, created_at AS latest_at, html_url
  FROM windowed_runs
  WHERE status = 'completed' AND conclusion = 'failure'
  ORDER BY workflow_id, created_at DESC, source_id DESC
)
SELECT
  l.workflow_id,
  l.workflow_name,
  a.failed_runs,
  a.total_runs,
  l.latest_run_id,
  l.latest_status,
  l.latest_conclusion,
  l.latest_at,
  l.html_url
FROM agg a
JOIN latest l ON l.workflow_id = a.workflow_id
WHERE a.failed_runs > 0
ORDER BY a.failed_runs DESC, l.latest_at DESC, l.workflow_id ASC
LIMIT 20;

-- name: ListRecentWorkflowRuns :many
SELECT *
FROM workflow_runs
WHERE repository_id = sqlc.arg(repository_id)
  AND created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
ORDER BY created_at DESC, source_id DESC
LIMIT 50;

-- name: ListLinkedPullRequestsForIssue :many
SELECT
  p.number,
  p.title,
  p.state,
  p.html_url,
  p.head_sha,
  COALESCE(CASE
    WHEN p.merged_at IS NOT NULL THEN 'merged'
    WHEN p.state <> 'open' THEN 'closed'
    WHEN NOT EXISTS (
      SELECT 1 FROM activity_children c
      WHERE c.repository_id = p.repository_id AND c.resource = 'reviews' AND c.parent_id = p.source_id AND c.watermark IS NOT NULL
    ) THEN NULL
    WHEN 'CHANGES_REQUESTED' = ANY(d.latest) THEN 'changes_requested'
    WHEN 'APPROVED' = ANY(d.latest) THEN 'approved'
    WHEN d.reviewed THEN 'commented'
    ELSE 'awaiting_review'
  END, '')::text AS review_state
FROM issue_pull_request_links l
JOIN activity_pull_requests p ON p.repository_id = l.repository_id AND p.number = l.pull_request_number
CROSS JOIN LATERAL (
  SELECT
    EXISTS (
      SELECT 1 FROM activity_reviews r
      WHERE r.repository_id = p.repository_id AND r.pull_request_id = p.source_id AND r.submitted_at IS NOT NULL
        AND upper(r.state) <> 'PENDING' AND r.author_id IS NOT NULL AND r.author_type <> 'Bot'
        AND r.author_id IS DISTINCT FROM p.author_id
    ) AS reviewed,
    ARRAY(
      SELECT DISTINCT ON (r.author_id) upper(r.state)
      FROM activity_reviews r
      WHERE r.repository_id = p.repository_id AND r.pull_request_id = p.source_id AND r.submitted_at IS NOT NULL
        AND upper(r.state) IN ('APPROVED', 'CHANGES_REQUESTED')
        AND r.author_id IS NOT NULL AND r.author_type <> 'Bot'
        AND r.author_id IS DISTINCT FROM p.author_id
      ORDER BY r.author_id, r.submitted_at DESC, r.source_id DESC
    ) AS latest
) d
WHERE l.repository_id = sqlc.arg(repository_id) AND l.issue_number = sqlc.arg(issue_number)
ORDER BY p.number ASC;

-- name: ListActionsCoverageByRepo :many
SELECT
  repository_id,
  boundary,
  watermark,
  COALESCE(last_error_code, '')::text AS error_code
FROM activity_resources
WHERE repository_id = ANY(sqlc.arg(repository_ids)::bigint[])
  AND resource = 'actions';

-- name: GetCrossActionsTotals :one
WITH windowed_runs AS (
  SELECT wr.repository_id, wr.source_id, wr.workflow_id, wr.status, wr.conclusion, wr.created_at
  FROM workflow_runs wr
  WHERE wr.repository_id = ANY(sqlc.arg(repository_ids)::bigint[])
    AND wr.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
), latest_per_workflow AS (
  SELECT DISTINCT ON (wr.repository_id, wr.workflow_id) wr.status, wr.conclusion
  FROM windowed_runs wr
  ORDER BY wr.repository_id, wr.workflow_id, wr.created_at DESC, wr.source_id DESC
)
SELECT
  count(*) FILTER (WHERE wr.status = 'completed' AND wr.conclusion <> '')::bigint AS completed_runs,
  count(*) FILTER (WHERE wr.status = 'completed' AND wr.conclusion = 'success')::bigint AS succeeded_runs,
  (SELECT count(*)::bigint FROM latest_per_workflow lpw WHERE lpw.status = 'completed' AND lpw.conclusion = 'failure') AS failing_workflows
FROM windowed_runs wr;

-- name: ListWorkflowRunsForPullRequest :many
SELECT
  wr.*,
  COALESCE(fj.name, '')::text AS failing_job_name,
  COALESCE(fj.html_url, '')::text AS failing_job_url,
  (fj.name IS NOT NULL)::boolean AS has_failing_job
FROM workflow_runs wr
LEFT JOIN LATERAL (
  SELECT j.name, j.html_url
  FROM workflow_jobs j
  WHERE wr.status = 'completed'
    AND wr.conclusion = 'failure'
    AND j.repository_id = wr.repository_id
    AND j.run_id = wr.source_id
    AND j.status = 'completed'
    AND j.conclusion = 'failure'
  ORDER BY j.created_at, j.source_id
  LIMIT 1
) fj ON true
WHERE wr.repository_id = sqlc.arg(repository_id)
  AND (
    sqlc.arg(pull_request_number)::bigint = ANY(wr.pull_request_numbers)
    OR (sqlc.arg(head_sha)::text <> '' AND wr.head_sha = sqlc.arg(head_sha)::text)
  )
ORDER BY wr.created_at DESC, wr.source_id DESC
LIMIT 20;

-- name: ListDeploymentsForSHA :many
SELECT *
FROM deployments
WHERE repository_id = sqlc.arg(repository_id)
  AND sqlc.arg(sha)::text <> ''
  AND sha = sqlc.arg(sha)::text
ORDER BY created_at DESC, source_id DESC
LIMIT 20;

-- name: ListCrossFailedWorkflowRuns :many
SELECT
  w.repository_id,
  r.name AS full_name,
  w.source_id AS run_id,
  w.run_number,
  w.workflow_name,
  w.html_url,
  w.updated_at
FROM workflow_runs w
JOIN repositories r ON r.id = w.repository_id
WHERE w.repository_id = ANY(sqlc.arg(repository_ids)::bigint[])
  AND w.status = 'completed'
  AND w.conclusion = 'failure'
  AND w.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
ORDER BY w.updated_at DESC, w.source_id DESC
LIMIT 5;
