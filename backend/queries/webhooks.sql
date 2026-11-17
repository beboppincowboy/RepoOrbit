-- name: InsertWebhookDelivery :execrows
INSERT INTO webhook_deliveries (delivery_id, event, received_at) VALUES ($1, $2, $3)
ON CONFLICT (delivery_id) DO NOTHING;

-- name: GetUserIDByGithubID :one
SELECT id FROM users WHERE github_id = $1;
