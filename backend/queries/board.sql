-- name: GetStatusLabels :one
-- Whether status labels are on for a stored, non-demo repository; no row for one that is not stored or is demo.
SELECT status_labels FROM repositories WHERE id = $1 AND NOT is_demo;

-- name: DisableStatusLabels :execrows
-- Turns status labels off. The caller has checked Admin or Maintain permission and its access fence.
UPDATE repositories SET status_labels = false, status_labels_changed_at = now() WHERE id = $1 AND NOT is_demo;

-- name: EnableStatusLabels :execrows
-- Turns status labels on for a succeeded status_labels operation, unless the setting changed after that operation
-- was recorded (a later Turn off wins over a late recheck). The caller has checked permission and its access fence.
UPDATE repositories r SET status_labels = true, status_labels_changed_at = now()
FROM write_operations o
WHERE r.id = sqlc.arg(repository_id) AND NOT r.is_demo AND o.id = sqlc.arg(operation_id)
 AND (r.status_labels_changed_at IS NULL OR r.status_labels_changed_at < o.created_at);

-- name: GetBoardStatusLabels :one
-- Whether status labels are on for a stored repository of the given mode; no row for a missing repository or one of
-- the other mode. The caller checks access.
SELECT status_labels FROM repositories WHERE id = sqlc.arg(id) AND is_demo = sqlc.arg(demo)::boolean;

-- name: ListRepositoryBoard :many
-- One repository's Board snapshot (KTD7): its open issues (the 500 most recently updated), its closed issues (the 50
-- most recently closed) and the issues numbered in include, each once, newest number first. status and
-- multiple_status come from issue_board_states; status is null and multiple_status false when status labels are off.
-- truncated is set on every row of an open or closed set the cap cut short. Demo and real mode each see only their
-- own repositories. The caller checks access.
SELECT i.number, i.title, i.html_url, i.state, i.author_login, i.updated_at,
  COALESCE(ARRAY(SELECT l.name FROM activity_issue_labels l WHERE l.issue_id = i.source_id ORDER BY l.name), '{}'::text[])::text[] AS labels,
  COALESCE(ARRAY(SELECT a.login FROM activity_issue_assignees a WHERE a.issue_id = i.source_id ORDER BY a.login), '{}'::text[])::text[] AS assignees,
  b.status, COALESCE(b.multiple_status, false)::boolean AS multiple_status, (k.total > k.cap)::boolean AS truncated
FROM (
  SELECT a.source_id, CASE WHEN a.state = 'closed' THEN 50 ELSE 500 END AS cap,
    row_number() OVER (PARTITION BY a.state = 'closed' ORDER BY CASE WHEN a.state = 'closed' THEN a.closed_at ELSE a.updated_at END DESC NULLS LAST, a.source_id DESC) AS rank,
    count(*) OVER (PARTITION BY a.state = 'closed') AS total
  FROM activity_issues a WHERE a.repository_id = sqlc.arg(repository_id)
) k
JOIN activity_issues i ON i.source_id = k.source_id
JOIN repositories r ON r.id = i.repository_id AND r.is_demo = sqlc.arg(demo)::boolean
LEFT JOIN issue_board_states b ON b.issue_id = i.source_id AND r.status_labels
WHERE k.rank <= k.cap OR i.number = ANY(sqlc.arg(include)::bigint[])
ORDER BY i.number DESC;

-- name: ListStatusCleanupCandidates :many
-- The stored issues the app's cleanup looks at in a repository with status labels on: closed issues still carrying a
-- status label, and issues with a removal left unknown. At most 100 per run.
SELECT i.number, i.source_id
FROM activity_issues i
JOIN repositories r ON r.id = i.repository_id AND r.status_labels AND NOT r.is_demo
WHERE i.repository_id = $1 AND (
 (i.state = 'closed' AND EXISTS (SELECT 1 FROM activity_issue_labels l WHERE l.issue_id = i.source_id AND status_label_rank(l.name) IS NOT NULL))
 OR EXISTS (SELECT 1 FROM app_operations o WHERE o.repository_id = i.repository_id AND o.issue_number = i.number AND o.state = 'unknown'))
ORDER BY i.number
LIMIT 100;

-- name: ListUnknownAppOperations :many
-- The app's removals on one issue still without a reliable answer.
SELECT id, label FROM app_operations WHERE repository_id = $1 AND issue_number = $2 AND state = 'unknown';

-- name: StartAppOperation :one
-- Records an app label removal as unknown just before it is sent, reusing the unresolved row a crashed run left.
INSERT INTO app_operations (repository_id, issue_number, label, state) VALUES ($1, $2, $3, 'unknown')
ON CONFLICT (repository_id, issue_number, label) WHERE state = 'unknown' DO UPDATE SET updated_at = now()
RETURNING id;

-- name: FinishAppOperation :exec
UPDATE app_operations SET state = $2, outcome = $3, updated_at = now() WHERE id = $1 AND state = 'unknown';

-- name: ListAppOperations :many
-- The app's writes on one repository, newest first, at most 100. The caller checks the viewer's access.
SELECT id, repository_id, issue_number, label, state, outcome, created_at, updated_at
FROM app_operations WHERE repository_id = $1 ORDER BY id DESC LIMIT 100;
