-- name: UpsertUser :one
INSERT INTO users (github_id, login, access_token_enc, refresh_token_enc, access_expires_at, refresh_expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (github_id) DO UPDATE SET login = EXCLUDED.login, access_token_enc = EXCLUDED.access_token_enc,
 refresh_token_enc = EXCLUDED.refresh_token_enc, access_expires_at = EXCLUDED.access_expires_at, refresh_expires_at = EXCLUDED.refresh_expires_at
RETURNING id;

-- name: LockUserTokens :one
SELECT id, access_token_enc, refresh_token_enc, access_expires_at FROM users WHERE id = $1 FOR UPDATE;

-- name: UpdateUserTokens :exec
UPDATE users SET access_token_enc = $2, refresh_token_enc = $3, access_expires_at = $4, refresh_expires_at = $5 WHERE id = $1;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4);

-- name: GetSession :one
SELECT u.id AS user_id, u.login FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > sqlc.arg(now)::timestamptz;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: CreateOAuthState :exec
INSERT INTO oauth_states (state_hash, code_verifier, expires_at) VALUES ($1, $2, $3);

-- name: ConsumeOAuthState :one
DELETE FROM oauth_states WHERE state_hash = $1 AND expires_at > sqlc.arg(now)::timestamptz RETURNING code_verifier;

-- name: DeleteExpiredAuthRows :exec
WITH s AS (DELETE FROM sessions WHERE expires_at <= sqlc.arg(now)::timestamptz)
DELETE FROM oauth_states WHERE expires_at <= sqlc.arg(now)::timestamptz;
