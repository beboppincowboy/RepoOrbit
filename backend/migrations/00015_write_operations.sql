-- +goose Up
-- One row per user-initiated GitHub issue write: the audit record (actor, request hash, actual outcome) and the
-- preserved draft. Never holds tokens. A row is 'pending' before transmission and 'unknown' from the moment the
-- request may have reached GitHub, so a process death mid-send is never mistaken for "not sent". After a 30-day
-- retention period (ingestion scheduler) succeeded and failed rows are deleted and unresolved ones redacted: an unknown
-- create keeps its content hash so a retry can never duplicate the issue.
CREATE TABLE write_operations (
 id bigserial PRIMARY KEY,
 user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 repository_id bigint NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
 idempotency_key text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('create', 'update')),
 issue_number bigint, -- the issue an update targets; for a create, the created issue once confirmed
 request_hash bytea NOT NULL,
 content_hash bytea, -- create only: title and body, kept after the draft is wiped so a recheck can still verify
 requested_metadata boolean NOT NULL DEFAULT false, -- create only: labels or assignees were requested (kept after a wipe)
 draft jsonb, -- the requested change; NULL once wiped after the actor lost access
 state text NOT NULL CHECK (state IN ('pending', 'succeeded', 'failed', 'unknown')),
 outcome jsonb NOT NULL DEFAULT '{}',
 sent_at timestamptz, -- when the row became 'unknown', just before the request was sent
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (user_id, idempotency_key)
);
-- One GitHub issue can be the confirmed result of at most one create.
CREATE UNIQUE INDEX write_operations_created_issue ON write_operations (repository_id, issue_number) WHERE kind = 'create' AND issue_number IS NOT NULL;
-- At most one unresolved create of the same content per actor and repository, whatever its idempotency key: a second
-- one could duplicate the issue.
CREATE UNIQUE INDEX write_operations_open_create ON write_operations (user_id, repository_id, content_hash) WHERE kind = 'create' AND state IN ('pending', 'unknown');
CREATE INDEX write_operations_user_repository ON write_operations (user_id, repository_id, id);
-- Actors whose private issue-write drafts a pending cleanup must wipe even if it cannot obtain a complete inventory:
-- the holders of a revoked installation, recorded when it was revoked (fail closed).
ALTER TABLE activity_cleanup ADD COLUMN wipe_users bigint[] NOT NULL DEFAULT '{}';
-- What an outcome keeps once the actor lost access: GitHub's issue number, link and state, the error summary (which
-- never quotes content) and how many requested changes were unapplied. Titles, labels and logins go. Idempotent.
-- +goose StatementBegin
CREATE FUNCTION redacted_write_outcome(o jsonb) RETURNS jsonb LANGUAGE sql IMMUTABLE AS $$
 SELECT jsonb_strip_nulls(jsonb_build_object(
  'issue', CASE WHEN o ? 'issue' THEN jsonb_build_object('number', o->'issue'->'number', 'url', o->'issue'->'url', 'state', o->'issue'->'state') END,
  'unappliedCount', COALESCE(to_jsonb(jsonb_array_length(CASE WHEN jsonb_typeof(o->'unapplied') = 'array' THEN o->'unapplied' END)), o->'unappliedCount'),
  'error', o->'error',
  'redacted', true))
$$;
-- +goose StatementEnd
-- +goose Down
ALTER TABLE activity_cleanup DROP COLUMN wipe_users;
DROP TABLE write_operations;
DROP FUNCTION redacted_write_outcome(jsonb);
