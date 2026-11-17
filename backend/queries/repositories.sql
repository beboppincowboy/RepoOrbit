-- name: ListDemoRepositories :many
SELECT id, name, description FROM repositories WHERE is_demo = true AND id > sqlc.arg(after)::bigint ORDER BY id LIMIT sqlc.arg(page_size)::int;

-- name: ListIssues :many
SELECT id, repository_id, title, state FROM (
 SELECT i.id, i.repository_id, i.title, i.state FROM issues i JOIN repositories r ON r.id = i.repository_id WHERE r.is_demo = sqlc.arg(demo)::boolean
 UNION ALL
 SELECT i.source_id AS id, i.repository_id, i.title, i.state FROM activity_issues i JOIN repositories r ON r.id = i.repository_id WHERE NOT r.is_demo AND NOT sqlc.arg(demo)::boolean
) AS source WHERE repository_id = ANY(sqlc.arg(repository_ids)::bigint[]) AND id > sqlc.arg(after)::bigint ORDER BY id LIMIT sqlc.arg(page_size)::int;
