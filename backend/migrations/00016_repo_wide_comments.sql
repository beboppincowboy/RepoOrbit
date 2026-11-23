-- +goose Up
-- Comments sync as one repository-wide stream. Per-issue comment progress and queued per-issue comment jobs are
-- dropped; stored comments stay. Each repository with an issue stream gets a comment stream from the same boundary,
-- which the next poll enqueues, so every repository rescans its comments once.
DELETE FROM jobs WHERE kind = 'activity_comments' AND key LIKE 'repo:%:parent:%';
DELETE FROM activity_children WHERE resource = 'comments';
INSERT INTO activity_resources (repository_id, resource, boundary)
SELECT repository_id, 'comments', boundary FROM activity_resources WHERE resource = 'issues'
ON CONFLICT DO NOTHING;

-- +goose Down
-- Per-issue progress is not restored: after a rollback, issue syncs register comment histories again and rescan.
DELETE FROM jobs WHERE kind = 'activity_comments' AND key NOT LIKE 'repo:%:parent:%';
DELETE FROM activity_resources WHERE resource = 'comments';
