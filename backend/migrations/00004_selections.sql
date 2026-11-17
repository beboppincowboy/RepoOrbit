-- +goose Up
-- One Selection per user and repository, whether it came from ownership or a star. Deselecting keeps the row.
CREATE TABLE selections (
 user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 source text NOT NULL CHECK (source IN ('owned', 'starred')),
 selected_at timestamptz NOT NULL,
 deselected_at timestamptz,
 PRIMARY KEY (user_id, repository_id)
);
CREATE INDEX selections_user_active ON selections (user_id, repository_id) WHERE deselected_at IS NULL;
-- +goose Down
DROP TABLE selections;
