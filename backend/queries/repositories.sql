-- name: ListDemoRepositories :many
SELECT id, name, description FROM repositories WHERE is_demo = true AND id > sqlc.arg(after)::bigint ORDER BY id LIMIT sqlc.arg(page_size)::int;

-- name: ListIssues :many
-- Demo and real mode both list the synced evidence the analytics count; each mode sees only its own repositories.
-- A non-null number narrows the listing to that issue number.
SELECT i.source_id AS id, i.repository_id, i.number, i.title, i.state, i.html_url, i.author_login, i.updated_at,
  COALESCE(ARRAY(SELECT l.name FROM activity_issue_labels l WHERE l.issue_id = i.source_id ORDER BY l.name), '{}'::text[])::text[] AS labels,
  COALESCE(ARRAY(SELECT a.login FROM activity_issue_assignees a WHERE a.issue_id = i.source_id ORDER BY a.login), '{}'::text[])::text[] AS assignees
FROM activity_issues i JOIN repositories r ON r.id = i.repository_id
WHERE r.is_demo = sqlc.arg(demo)::boolean AND i.repository_id = ANY(sqlc.arg(repository_ids)::bigint[]) AND i.source_id > sqlc.arg(after)::bigint
  AND (sqlc.narg(number)::bigint IS NULL OR i.number = sqlc.narg(number)::bigint)
ORDER BY i.source_id LIMIT sqlc.arg(page_size)::int;
