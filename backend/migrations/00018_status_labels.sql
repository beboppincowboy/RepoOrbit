-- +goose Up
-- Status labels (ADR 0003): a repository's Board columns come from its status:todo, status:in-progress and
-- status:review labels once an Admin or Maintain user turns them on. Only the flag is stored; each issue's state is
-- derived from its synced labels and open/closed state, never stored per issue.
ALTER TABLE repositories ADD COLUMN status_labels boolean NOT NULL DEFAULT false;
-- When the setting last changed, so an enable operation finished late (a recheck) cannot undo a later Turn off.
ALTER TABLE repositories ADD COLUMN status_labels_changed_at timestamptz;

-- The rank of a status label (1 todo, 2 in progress, 3 in review), NULL for any other label. GitHub label names are
-- case-insensitive, so the match is too. Every status-label check goes through this function.
-- +goose StatementBegin
CREATE FUNCTION status_label_rank(name text) RETURNS integer LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE lower(name) WHEN 'status:todo' THEN 1 WHEN 'status:in-progress' THEN 2 WHEN 'status:review' THEN 3 END
$$;
-- +goose StatementEnd

-- Turning status labels on is a reviewed user write that creates the missing labels.
ALTER TABLE write_operations DROP CONSTRAINT write_operations_kind_check;
ALTER TABLE write_operations ADD CONSTRAINT write_operations_kind_check CHECK (kind IN ('create', 'update', 'status_labels'));

-- Writes the GitHub App makes with no user behind it: removing a status label from an issue that was closed on GitHub
-- (R20). Kept apart from write_operations, whose rows always belong to a user. 'unknown' while the removal may have
-- been sent; a retry of the cleanup job resolves it (removing a label is idempotent). Holds no tokens or content beyond
-- the issue number and the fixed status label name. Deleted after the same 30 days as write_operations.
CREATE TABLE app_operations (
 id bigserial PRIMARY KEY,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 issue_number bigint NOT NULL,
 label text NOT NULL,
 state text NOT NULL CHECK (state IN ('unknown', 'succeeded', 'failed')),
 outcome jsonb NOT NULL DEFAULT '{}', -- {error} when it did not succeed
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
-- One unresolved removal per issue and label: a retried job reuses it instead of adding another.
CREATE UNIQUE INDEX app_operations_unresolved ON app_operations (repository_id, issue_number, label) WHERE state = 'unknown';
CREATE INDEX app_operations_repository ON app_operations (repository_id, id);

-- +goose Down
DROP TABLE app_operations;
DELETE FROM write_operations WHERE kind = 'status_labels';
ALTER TABLE write_operations DROP CONSTRAINT write_operations_kind_check;
ALTER TABLE write_operations ADD CONSTRAINT write_operations_kind_check CHECK (kind IN ('create', 'update'));
DROP FUNCTION status_label_rank(text);
ALTER TABLE repositories DROP COLUMN status_labels_changed_at;
ALTER TABLE repositories DROP COLUMN status_labels;
