-- name: InsertWriteOperation :one
INSERT INTO write_operations (user_id, repository_id, idempotency_key, kind, issue_number, request_hash, content_hash, has_requested_metadata, draft, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending')
ON CONFLICT (user_id, idempotency_key) DO NOTHING
RETURNING id;

-- name: GetWriteOperationByKey :one
SELECT id, request_hash,
 (state = 'pending' AND created_at < now() - interval '1 minute') AS stale_pending
FROM write_operations WHERE user_id = $1 AND idempotency_key = $2;

-- name: GetWriteOperationScope :one
SELECT repository_id, state,
 (state = 'pending' AND created_at < now() - interval '1 minute') AS stale_pending
FROM write_operations WHERE id = $1 AND user_id = $2;

-- name: GetWriteOperation :one
SELECT o.id, o.repository_id, o.kind, o.issue_number, o.content_hash, o.has_requested_metadata, o.draft, o.state, o.outcome, o.sent_at, o.created_at, o.updated_at, u.login AS actor_login, u.github_id AS actor_github_id, r.name AS repository_name,
 (o.state = 'pending' AND o.created_at < now() - interval '1 minute') AS stale_pending
FROM write_operations o JOIN users u ON u.id = o.user_id JOIN repositories r ON r.id = o.repository_id WHERE o.id = $1 AND o.user_id = $2;

-- name: ListWriteOperations :many
SELECT o.id, o.repository_id, o.kind, o.issue_number, o.content_hash, o.has_requested_metadata, o.draft, o.state, o.outcome, o.sent_at, o.created_at, o.updated_at, u.login AS actor_login, u.github_id AS actor_github_id, r.name AS repository_name,
 (o.state = 'pending' AND o.created_at < now() - interval '1 minute') AS stale_pending
FROM write_operations o JOIN users u ON u.id = o.user_id JOIN repositories r ON r.id = o.repository_id WHERE o.user_id = $1 AND o.repository_id = $2 ORDER BY o.id DESC LIMIT 100;

-- name: MarkWriteOperationSent :execrows
-- The 1-minute freshness window here must equal the stale_pending threshold in the Get/List queries above and the
-- created_at cutoff in FailStalePendingCreate, FailStalePendingOperationByID and FailAllStalePendingOperations below:
-- a row this refuses to send is exactly a row those report stale and settle as failed.
UPDATE write_operations SET state = 'unknown', sent_at = now(), updated_at = now()
WHERE id = $1 AND state = 'pending' AND created_at >= now() - interval '1 minute';

-- name: FinishWriteOperation :execrows
UPDATE write_operations SET state = $2, outcome = CASE WHEN draft IS NULL THEN redacted_write_outcome(sqlc.arg(outcome)::jsonb) ELSE sqlc.arg(outcome)::jsonb END,
 issue_number = COALESCE(sqlc.narg(issue_number), issue_number), updated_at = now()
WHERE id = $1 AND state = 'unknown';

-- name: FindUnresolvedCreate :one
SELECT id FROM write_operations WHERE user_id = $1 AND repository_id = $2 AND kind = 'create' AND state IN ('pending', 'unknown') AND content_hash = $3 ORDER BY id LIMIT 1;

-- name: FindLatestCreate :one
SELECT id FROM write_operations WHERE user_id = $1 AND repository_id = $2 AND kind = 'create' AND content_hash = $3 ORDER BY id DESC LIMIT 1;

-- name: FailStalePendingCreate :execrows
UPDATE write_operations SET state = 'failed',
 outcome = CASE WHEN draft IS NULL THEN redacted_write_outcome('{"error":"abandoned before sending; nothing was sent to GitHub"}'::jsonb)
  ELSE '{"error":"abandoned before sending; nothing was sent to GitHub"}'::jsonb END,
 updated_at = now()
WHERE user_id = $1 AND repository_id = $2 AND kind = 'create' AND state = 'pending' AND content_hash = $3 AND created_at < now() - interval '1 minute';

-- name: FailStalePendingOperationByID :exec
UPDATE write_operations SET state = 'failed',
 outcome = CASE WHEN draft IS NULL THEN redacted_write_outcome('{"error":"abandoned before sending; nothing was sent to GitHub"}'::jsonb)
  ELSE '{"error":"abandoned before sending; nothing was sent to GitHub"}'::jsonb END,
 updated_at = now()
WHERE id = $1 AND state = 'pending' AND created_at < now() - interval '1 minute';

-- name: FailAllStalePendingOperations :exec
UPDATE write_operations SET state = 'failed',
 outcome = CASE WHEN draft IS NULL THEN redacted_write_outcome('{"error":"abandoned before sending; nothing was sent to GitHub"}'::jsonb)
  ELSE '{"error":"abandoned before sending; nothing was sent to GitHub"}'::jsonb END,
 updated_at = now()
WHERE state = 'pending' AND created_at < now() - interval '1 minute';

-- name: WipeActorDrafts :exec
UPDATE write_operations SET draft = NULL, outcome = redacted_write_outcome(outcome) WHERE user_id = $1 AND repository_id = $2 AND draft IS NOT NULL;

-- name: GetRepositoryIsPrivate :one
SELECT is_private FROM repositories WHERE id = $1 AND NOT is_demo;
