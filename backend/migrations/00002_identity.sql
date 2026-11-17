-- +goose Up
-- Demo rows are marked so Demo mode returns only them and real mode never does.
ALTER TABLE repositories ADD COLUMN is_demo boolean NOT NULL DEFAULT false;
UPDATE repositories SET is_demo = true WHERE id IN (1, 2) AND name IN ('orbit/example', 'orbit/launchpad');
CREATE TABLE users (
 id bigserial PRIMARY KEY,
 github_id bigint NOT NULL UNIQUE,
 login text NOT NULL,
 access_token_enc bytea NOT NULL,
 refresh_token_enc bytea,
 access_expires_at timestamptz,
 refresh_expires_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sessions (
 token_hash bytea PRIMARY KEY,
 user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE TABLE oauth_states (
 state_hash bytea PRIMARY KEY,
 code_verifier text NOT NULL,
 expires_at timestamptz NOT NULL
);
-- +goose Down
DROP TABLE oauth_states;
DROP TABLE sessions;
DROP TABLE users;
ALTER TABLE repositories DROP COLUMN is_demo;
