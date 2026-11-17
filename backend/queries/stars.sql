-- name: DeleteStars :exec
DELETE FROM stars WHERE user_id = $1;

-- name: InsertStars :exec
INSERT INTO stars (user_id, repository_id, full_name, description, is_private)
SELECT sqlc.arg(user_id)::bigint, unnest(sqlc.arg(ids)::bigint[]), unnest(sqlc.arg(full_names)::text[]), unnest(sqlc.arg(descriptions)::text[]), unnest(sqlc.arg(privates)::boolean[]);

-- name: UpsertStarSnapshot :exec
INSERT INTO star_snapshots (user_id, refreshed_at) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET refreshed_at = EXCLUDED.refreshed_at;

-- name: GetStarSnapshotRefreshedAt :one
SELECT refreshed_at FROM star_snapshots WHERE user_id = $1;

-- name: ListStars :many
SELECT repository_id, full_name, description, is_private FROM stars WHERE user_id = $1
ORDER BY full_name, repository_id LIMIT sqlc.arg(page_size)::int OFFSET sqlc.arg(page_offset)::int;

-- name: IsStarred :one
SELECT EXISTS (SELECT 1 FROM stars WHERE user_id = $1 AND repository_id = $2);
