-- name: ListMetadataStatuses :many
-- Shared metadata status for repositories the user selected. No credential identity and no raw error text leave the database:
-- last_error_code is a safe code written by the refresh handler.
SELECT s.repository_id, s.follow_suspended_at, j.state AS job_state, j.run_at AS job_run_at, rs.last_success_at, rs.last_error_code
FROM selections s
LEFT JOIN jobs j ON j.kind = 'repo_metadata_refresh' AND j.key = 'repo:' || s.repository_id::text
LEFT JOIN repository_sync rs ON rs.repository_id = s.repository_id
WHERE s.user_id = $1 AND s.deselected_at IS NULL AND s.repository_id = ANY(sqlc.arg(repository_ids)::bigint[]);

-- name: ListFollowedRepositoryIDs :many
-- Repositories some user actively selects and is still follow-eligible for: the poll set.
SELECT DISTINCT s.repository_id FROM selections s JOIN repositories r ON r.id = s.repository_id
WHERE s.deselected_at IS NULL AND s.follow_suspended_at IS NULL AND r.is_demo = false ORDER BY s.repository_id;

-- name: ListEligibleRepositoryIDsForUser :many
SELECT s.repository_id FROM selections s JOIN repositories r ON r.id = s.repository_id
WHERE s.user_id = $1 AND s.deselected_at IS NULL AND s.follow_suspended_at IS NULL AND r.is_demo = false ORDER BY s.repository_id;

-- name: ListStarFollowerIDs :many
-- Users whose followed repositories depend on a star snapshot (suspended ones included: they may star again).
SELECT DISTINCT user_id FROM selections WHERE source = 'starred' AND deselected_at IS NULL ORDER BY user_id;

-- name: SuspendUnstarredFollows :exec
-- Run only after a COMPLETE star replacement. Owned selections never depend on stars.
UPDATE selections s SET follow_suspended_at = sqlc.arg(now)::timestamptz
WHERE s.user_id = $1 AND s.source = 'starred' AND s.deselected_at IS NULL AND s.follow_suspended_at IS NULL
AND NOT EXISTS (SELECT 1 FROM stars st WHERE st.user_id = s.user_id AND st.repository_id = s.repository_id);

-- name: RestoreStarredFollows :exec
UPDATE selections s SET follow_suspended_at = NULL
WHERE s.user_id = $1 AND s.follow_suspended_at IS NOT NULL
AND (s.source = 'owned' OR EXISTS (SELECT 1 FROM stars st WHERE st.user_id = s.user_id AND st.repository_id = s.repository_id));
