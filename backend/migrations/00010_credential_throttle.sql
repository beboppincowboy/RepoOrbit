-- +goose Up
-- One durable retry-after per stored credential (one per user), shared by every job kind and process, so a rate limit
-- seen by any of them paces all of them. Stale rows are harmless: a past retry_after is ignored.
CREATE TABLE credential_throttle (
 user_id bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 retry_after timestamptz NOT NULL
);
-- +goose Down
DROP TABLE credential_throttle;
