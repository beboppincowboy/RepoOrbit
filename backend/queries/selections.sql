-- name: UpsertRepository :one
-- Never touches demo rows: a collision returns no row so the caller fails loudly.
INSERT INTO repositories (id, name, description, is_private, is_demo) VALUES ($1, $2, $3, $4, false)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description, is_private = EXCLUDED.is_private
WHERE repositories.is_demo = false
RETURNING id;

-- name: UpsertSelection :exec
INSERT INTO selections (user_id, repository_id, source, selected_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, repository_id) DO UPDATE SET
 selected_at = CASE WHEN selections.deselected_at IS NULL THEN selections.selected_at ELSE EXCLUDED.selected_at END,
 deselected_at = NULL,
 source = CASE WHEN selections.source = 'owned' OR EXCLUDED.source = 'owned' THEN 'owned' ELSE 'starred' END;

-- name: DeselectRepository :execrows
UPDATE selections SET deselected_at = sqlc.arg(now)::timestamptz WHERE user_id = $1 AND repository_id = $2 AND deselected_at IS NULL;

-- name: ListSelectedRepositories :many
SELECT r.id, r.name, r.description, r.is_private FROM selections s JOIN repositories r ON r.id = s.repository_id
WHERE s.user_id = $1 AND s.deselected_at IS NULL AND r.is_demo = false AND r.id > sqlc.arg(after)::bigint ORDER BY r.id LIMIT sqlc.arg(page_size)::int;

-- name: ListSelectedIDs :many
SELECT repository_id FROM selections WHERE user_id = $1 AND deselected_at IS NULL AND repository_id = ANY(sqlc.arg(repository_ids)::bigint[]);
