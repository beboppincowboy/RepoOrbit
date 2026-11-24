-- name: GetStatusLabels :one
-- Whether status labels are on for a stored, non-demo repository; no row for one that is not stored or is demo.
SELECT status_labels FROM repositories WHERE id = $1 AND NOT is_demo;

-- name: SetStatusLabels :execrows
-- Turns status labels on or off. The caller has checked Admin or Maintain permission and its access fence.
UPDATE repositories SET status_labels = $2 WHERE id = $1 AND NOT is_demo;

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

-- name: ListStaleStatusLabels :many
-- The status labels still on stored closed issues of a repository with status labels on: what the app's cleanup
-- removes. At most 100 per run; the job runs again while any remain.
SELECT i.number, l.name AS label
FROM activity_issues i
JOIN repositories r ON r.id = i.repository_id AND r.status_labels AND NOT r.is_demo
JOIN activity_issue_labels l ON l.issue_id = i.source_id AND status_label_rank(l.name) IS NOT NULL
WHERE i.repository_id = $1 AND i.state = 'closed'
ORDER BY i.number, l.name
LIMIT 100;

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
