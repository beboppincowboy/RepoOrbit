-- +goose Up
-- The latest completed snapshot of a user's GitHub stars. Replaced whole in one transaction, only after every page was read, so a failed refresh leaves the previous snapshot (and its age) intact.
-- Starring is not an access grant: rows hold only what the user's own star list showed, and private ones are re-verified before they are shown.
CREATE TABLE star_snapshots (
 user_id bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 refreshed_at timestamptz NOT NULL
);
CREATE TABLE stars (
 user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 repository_id bigint NOT NULL,
 full_name text NOT NULL,
 description text NOT NULL,
 is_private boolean NOT NULL,
 PRIMARY KEY (user_id, repository_id)
);
CREATE INDEX stars_user_name ON stars (user_id, full_name, repository_id);
-- +goose Down
DROP TABLE stars;
DROP TABLE star_snapshots;
