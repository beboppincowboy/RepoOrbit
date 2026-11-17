-- +goose Up
-- One row per accepted supported delivery; the unique id makes redelivery a no-op. Written in the same transaction as the work it triggers.
CREATE TABLE webhook_deliveries (
 delivery_id text PRIMARY KEY,
 event text NOT NULL,
 received_at timestamptz NOT NULL
);
-- +goose Down
DROP TABLE webhook_deliveries;
