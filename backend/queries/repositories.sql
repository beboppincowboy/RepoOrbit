-- name: ListDemoRepositories :many
SELECT id, name, description FROM repositories WHERE is_demo = true AND id > sqlc.arg(after)::bigint ORDER BY id LIMIT sqlc.arg(page_size)::int;

-- name: ListIssues :many
-- Demo and real mode both list the synced evidence the analytics count; each mode sees only its own repositories.
SELECT i.source_id AS id, i.repository_id, i.title, i.state FROM activity_issues i JOIN repositories r ON r.id = i.repository_id
WHERE r.is_demo = sqlc.arg(demo)::boolean AND i.repository_id = ANY(sqlc.arg(repository_ids)::bigint[]) AND i.source_id > sqlc.arg(after)::bigint
ORDER BY i.source_id LIMIT sqlc.arg(page_size)::int;
