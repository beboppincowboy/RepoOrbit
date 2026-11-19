-- +goose Up
-- One row per user-initiated GitHub issue write: the audit record (actor, request hash, actual outcome) and the
-- preserved draft. Never holds tokens. A row is 'pending' before transmission and 'unknown' from the moment the
-- request may have reached GitHub, so a process death mid-send is never mistaken for "not sent".
CREATE TABLE write_operations (
 id bigserial PRIMARY KEY,
 user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 idempotency_key text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('create', 'update')),
 issue_number bigint, -- the issue an update targets; for a create, the created issue once confirmed
 request_hash bytea NOT NULL,
 content_hash bytea, -- create only: title and body, kept after the draft is wiped so a recheck can still verify
 draft jsonb, -- the requested change; NULL once wiped after the actor lost access
 state text NOT NULL CHECK (state IN ('pending', 'succeeded', 'failed', 'unknown')),
 outcome jsonb NOT NULL DEFAULT '{}',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (user_id, idempotency_key)
);
-- One GitHub issue can be the confirmed result of at most one create.
CREATE UNIQUE INDEX write_operations_created_issue ON write_operations (repository_id, issue_number) WHERE kind = 'create' AND issue_number IS NOT NULL;
CREATE INDEX write_operations_user_repository ON write_operations (user_id, repository_id, id);
-- +goose Down
DROP TABLE write_operations;
