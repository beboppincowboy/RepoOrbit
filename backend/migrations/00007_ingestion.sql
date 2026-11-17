-- +goose Up
-- Durable authorization fence: epochs only ever increase. A missing row means epoch 0.
CREATE TABLE access_fences (
 scope text NOT NULL CHECK (scope IN ('user', 'installation', 'repository')),
 subject_id bigint NOT NULL,
 epoch bigint NOT NULL DEFAULT 0,
 PRIMARY KEY (scope, subject_id)
);
-- NULL means the selection is follow-eligible. Written by star reconciliation (unstar suspends it).
ALTER TABLE selections ADD COLUMN follow_suspended_at timestamptz;
-- Outcome of the latest metadata refresh per repository; error codes are safe, fixed strings.
CREATE TABLE repository_sync (
 repository_id bigint PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
 last_success_at timestamptz,
 last_error_code text
);
-- +goose Down
DROP TABLE repository_sync;
ALTER TABLE selections DROP COLUMN follow_suspended_at;
DROP TABLE access_fences;
