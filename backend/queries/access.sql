-- name: GetUserGithubID :one
SELECT github_id FROM users WHERE id = $1;

-- name: UpsertInstallation :exec
INSERT INTO installations (user_id, installation_id, account_login) VALUES ($1, $2, $3)
ON CONFLICT (user_id, installation_id) DO UPDATE SET account_login = EXCLUDED.account_login;

-- name: PruneInstallations :exec
DELETE FROM installations WHERE user_id = $1 AND installation_id <> ALL(sqlc.arg(keep)::bigint[]);

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = $1;

-- name: ClearUserTokens :exec
UPDATE users SET access_token_enc = '\x', refresh_token_enc = NULL, access_expires_at = NULL, refresh_expires_at = NULL WHERE id = $1;
