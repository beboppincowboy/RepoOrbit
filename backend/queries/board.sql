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

-- name: ListBoardIssues :many
-- Every stored issue of a repository with status labels on, with its Board state (R15): 'done' when closed whatever
-- its labels, else the furthest-along status label ('in_review', 'in_progress', 'todo'), else 'backlog'. multiple_status
-- flags an open issue carrying more than one status label. No rows when status labels are off. The caller checks access.
SELECT i.number, i.title, i.html_url, i.state,
 (CASE WHEN i.state = 'closed' THEN 'done' WHEN s.top = 3 THEN 'in_review' WHEN s.top = 2 THEN 'in_progress' WHEN s.top = 1 THEN 'todo' ELSE 'backlog' END)::text AS status,
 (i.state <> 'closed' AND s.n > 1)::boolean AS multiple_status
FROM activity_issues i
JOIN repositories r ON r.id = i.repository_id AND r.status_labels
CROSS JOIN LATERAL (SELECT max(status_label_rank(l.name)) AS top, count(DISTINCT status_label_rank(l.name)) AS n
 FROM activity_issue_labels l WHERE l.issue_id = i.source_id) s
WHERE i.repository_id = $1
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
