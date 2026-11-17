-- name: HasEligibleSelection :one
SELECT EXISTS (
 SELECT 1 FROM selections s JOIN repositories r ON r.id = s.repository_id
 WHERE s.repository_id = $1 AND s.deselected_at IS NULL AND s.follow_suspended_at IS NULL AND r.is_demo = false
);

-- name: ListEligibleSelectors :many
SELECT s.user_id FROM selections s JOIN repositories r ON r.id = s.repository_id
WHERE s.repository_id = $1 AND s.deselected_at IS NULL AND s.follow_suspended_at IS NULL AND r.is_demo = false
ORDER BY s.selected_at, s.user_id;

-- name: ListRefreshableRepositoryIDs :many
SELECT DISTINCT s.repository_id FROM selections s JOIN repositories r ON r.id = s.repository_id
WHERE s.deselected_at IS NULL AND s.follow_suspended_at IS NULL AND r.is_demo = false ORDER BY s.repository_id;

-- name: GetSyncRepository :one
SELECT is_private, is_demo FROM repositories WHERE id = $1;

-- name: UpdateRepositoryMetadata :exec
UPDATE repositories SET name = $2, description = $3, is_private = $4 WHERE id = $1 AND is_demo = false;

-- name: MarkRepositoryPrivate :exec
UPDATE repositories SET is_private = true WHERE id = $1 AND is_demo = false;

-- name: RecordSyncSuccess :exec
INSERT INTO repository_sync (repository_id, last_success_at, last_error_code) VALUES ($1, $2, NULL)
ON CONFLICT (repository_id) DO UPDATE SET last_success_at = EXCLUDED.last_success_at, last_error_code = NULL;

-- name: RecordSyncError :exec
INSERT INTO repository_sync (repository_id, last_error_code) VALUES ($1, $2)
ON CONFLICT (repository_id) DO UPDATE SET last_error_code = EXCLUDED.last_error_code;
