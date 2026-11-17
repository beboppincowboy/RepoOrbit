-- +goose Up
-- Durable work queue: one row per (kind, key). A finished job with no pending intent deletes its row.
-- generation is bumped on every claim and fences stale workers; attempts counts spent failure attempts only
-- (rate-limit and no-credential waits never increment it). A dead row is a barrier: only the operator Requeue revives it.
-- last_error is for operators only and must not be exposed through shared responses.
CREATE TABLE jobs (
 id bigserial PRIMARY KEY,
 kind text NOT NULL,
 key text NOT NULL,
 state text NOT NULL CHECK (state IN ('queued', 'running', 'paused', 'dead')),
 run_at timestamptz NOT NULL,
 pending boolean NOT NULL DEFAULT false,
 attempts int NOT NULL DEFAULT 0,
 generation bigint NOT NULL DEFAULT 0,
 lease_until timestamptz,
 started_at timestamptz,
 last_error text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (kind, key)
);
CREATE INDEX jobs_claim ON jobs (state, run_at);
-- +goose Down
DROP TABLE jobs;
