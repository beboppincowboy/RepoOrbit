-- name: InsertWriteOperation :one
-- Returns no row when the user already used the key; the caller then compares request hashes. A second unresolved
-- create of the same content fails on write_operations_unresolved_create. The row starts 'pending'.
INSERT INTO write_operations (user_id, repository_id, idempotency_key, kind, issue_number, request_hash, content_hash, has_requested_metadata, draft, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending')
ON CONFLICT (user_id, idempotency_key) DO NOTHING
RETURNING id;

-- name: GetWriteOperationByKey :one
-- The operation the user recorded under this idempotency key, with its request hash for comparison.
SELECT id, request_hash FROM write_operations WHERE user_id = $1 AND idempotency_key = $2;

-- name: GetWriteOperation :one
-- Scoped to the initiating user: another user's id reads as not found.
SELECT o.id, o.repository_id, o.kind, o.issue_number, o.content_hash, o.has_requested_metadata, o.draft, o.state, o.outcome, o.sent_at, o.created_at, o.updated_at, u.login AS actor_login, u.github_id AS actor_github_id, r.name AS repository_name
FROM write_operations o JOIN users u ON u.id = o.user_id JOIN repositories r ON r.id = o.repository_id WHERE o.id = $1 AND o.user_id = $2;

-- name: ListWriteOperations :many
-- The user's own operations on one repository, newest first, at most 100 (no paging).
SELECT o.id, o.repository_id, o.kind, o.issue_number, o.content_hash, o.has_requested_metadata, o.draft, o.state, o.outcome, o.sent_at, o.created_at, o.updated_at, u.login AS actor_login, u.github_id AS actor_github_id, r.name AS repository_name
FROM write_operations o JOIN users u ON u.id = o.user_id JOIN repositories r ON r.id = o.repository_id WHERE o.user_id = $1 AND o.repository_id = $2 ORDER BY o.id DESC LIMIT 100;

-- name: MarkWriteOperationSent :execrows
-- Moves a pending operation to 'unknown' and stamps sent_at, just before its request is sent. 0 rows: it is no longer
-- pending (FailStalePendingCreate failed it), and nothing may be sent; the caller answers with the row as it stands.
UPDATE write_operations SET state = 'unknown', sent_at = now(), updated_at = now() WHERE id = $1 AND state = 'pending';

-- name: FinishWriteOperation :execrows
-- Only an 'unknown' operation takes an outcome; issue_number stays when the new value is NULL. An outcome recorded
-- after the draft was wiped (the actor lost access meanwhile) is stored redacted.
UPDATE write_operations SET state = $2, outcome = CASE WHEN draft IS NULL THEN redacted_write_outcome(sqlc.arg(outcome)::jsonb) ELSE sqlc.arg(outcome)::jsonb END,
 issue_number = COALESCE(sqlc.narg(issue_number), issue_number), updated_at = now()
WHERE id = $1 AND state = 'unknown';

-- name: FindUnresolvedCreate :one
-- The unresolved create of the same content that blocks a new create under a fresh key.
SELECT id FROM write_operations WHERE user_id = $1 AND repository_id = $2 AND kind = 'create' AND state IN ('pending', 'unknown') AND content_hash = $3 ORDER BY id LIMIT 1;

-- name: FindLatestCreate :one
-- The user's most recent create of the same content in this repository, in any state: what a create that collided
-- with a blocker which resolved before it could be found names instead.
SELECT id FROM write_operations WHERE user_id = $1 AND repository_id = $2 AND kind = 'create' AND content_hash = $3 ORDER BY id DESC LIMIT 1;

-- name: FailStalePendingCreate :execrows
-- A create still pending a minute after it was recorded was never sent: marking it sent (MarkWriteOperationSent,
-- conditional on 'pending') precedes the request, so a process that stopped in between sent nothing. Failing it frees
-- the content for a retry; should the original process resume, its mark finds no pending row and it sends nothing.
-- Run by a new create that collided with that pending create; the threshold is the literal '1 minute' below. Only a
-- non-zero count (this call failed a never-sent create) lets that new create retry its insert.
UPDATE write_operations SET state = 'failed', outcome = '{"error":"abandoned before sending; nothing was sent to GitHub"}', updated_at = now()
WHERE user_id = $1 AND repository_id = $2 AND kind = 'create' AND state = 'pending' AND content_hash = $3 AND created_at < now() - interval '1 minute';

-- name: WipeActorDrafts :exec
-- Wipes the drafts and redacts the outcomes of every operation, in any state, of one user on one repository.
UPDATE write_operations SET draft = NULL, outcome = redacted_write_outcome(outcome) WHERE user_id = $1 AND repository_id = $2 AND draft IS NOT NULL;

-- name: GetRepositoryIsPrivate :one
-- Whether a stored, non-demo repository is private; no row for one that is not stored or is demo.
SELECT is_private FROM repositories WHERE id = $1 AND NOT is_demo;
