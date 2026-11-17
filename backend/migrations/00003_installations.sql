-- +goose Up
-- Real repositories are stored only when a user selects one. The flag is last-seen visibility, never trusted for access.
ALTER TABLE repositories ADD COLUMN is_private boolean NOT NULL DEFAULT false;
-- An app installation a user has proven they can access (it appeared in their own GET /user/installations).
CREATE TABLE installations (
 user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 installation_id bigint NOT NULL,
 account_login text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (user_id, installation_id)
);
-- +goose Down
DROP TABLE installations;
ALTER TABLE repositories DROP COLUMN is_private;
