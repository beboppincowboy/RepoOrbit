-- name: InsertWriteOperation :one
-- Returns no row when the user already used the key; the caller then compares request hashes.
INSERT INTO write_operations (user_id, repository_id, idempotency_key, kind, issue_number, request_hash, content_hash, draft, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending')
ON CONFLICT (user_id, idempotency_key) DO NOTHING
RETURNING id;

-- name: GetWriteOperationByKey :one
SELECT id, request_hash FROM write_operations WHERE user_id = $1 AND idempotency_key = $2;

-- name: GetWriteOperation :one
-- Scoped to the initiating user: another user's id reads as not found.
SELECT o.id, o.repository_id, o.kind, o.issue_number, o.content_hash, o.draft, o.state, o.outcome, o.created_at, o.updated_at, u.login AS actor_login, u.github_id AS actor_github_id, r.name AS repository_name
FROM write_operations o JOIN users u ON u.id = o.user_id JOIN repositories r ON r.id = o.repository_id WHERE o.id = $1 AND o.user_id = $2;

-- name: ListWriteOperations :many
SELECT o.id, o.repository_id, o.kind, o.issue_number, o.content_hash, o.draft, o.state, o.outcome, o.created_at, o.updated_at, u.login AS actor_login, u.github_id AS actor_github_id, r.name AS repository_name
FROM write_operations o JOIN users u ON u.id = o.user_id JOIN repositories r ON r.id = o.repository_id WHERE o.user_id = $1 AND o.repository_id = $2 ORDER BY o.id DESC LIMIT 100;

-- name: MarkWriteOperationSent :execrows
UPDATE write_operations SET state = 'unknown', updated_at = now() WHERE id = $1 AND state = 'pending';

-- name: FinishWriteOperation :execrows
-- Only an unresolved operation takes an outcome; issue_number stays when the new value is NULL.
UPDATE write_operations SET state = $2, outcome = $3, issue_number = COALESCE(sqlc.narg(issue_number), issue_number), updated_at = now()
WHERE id = $1 AND state = 'unknown';

-- name: FindUnknownCreate :one
-- An unresolved create of the same content blocks a new create under a fresh key.
SELECT id FROM write_operations WHERE user_id = $1 AND repository_id = $2 AND kind = 'create' AND state = 'unknown' AND content_hash = $3 ORDER BY id LIMIT 1;

-- name: WipeActorDrafts :exec
UPDATE write_operations SET draft = NULL WHERE user_id = $1 AND repository_id = $2 AND draft IS NOT NULL;

-- name: GetWriteRepository :one
SELECT is_private FROM repositories WHERE id = $1 AND NOT is_demo;
