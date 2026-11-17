-- name: ListDemoRepositories :many
SELECT id, name, description FROM repositories WHERE is_demo = true AND id > sqlc.arg(after)::bigint ORDER BY id LIMIT sqlc.arg(page_size)::int;

-- name: ListIssues :many
SELECT i.id, i.repository_id, i.title, i.state FROM issues i JOIN repositories r ON r.id = i.repository_id
WHERE r.is_demo = sqlc.arg(demo)::boolean AND i.repository_id = ANY(sqlc.arg(repository_ids)::bigint[]) AND i.id > sqlc.arg(after)::bigint ORDER BY i.id LIMIT sqlc.arg(page_size)::int;
