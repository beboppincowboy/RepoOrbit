-- name: ListRepositories :many
SELECT id, name, description FROM repositories WHERE id > $1 ORDER BY id LIMIT $2;

-- name: ListIssues :many
SELECT id, repository_id, title, state FROM issues WHERE repository_id = ANY($1::bigint[]) AND id > $2 ORDER BY id LIMIT $3;
