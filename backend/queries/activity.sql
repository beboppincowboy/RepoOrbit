-- name: ListActivityStatuses :many
SELECT a.repository_id, a.resource, a.boundary, a.phase, a.watermark, a.last_success_at, a.last_error_code,
       j.state AS job_state, (CASE WHEN j.state = 'queued' THEN j.run_at END)::timestamptz AS next_attempt_at
FROM activity_resources a
LEFT JOIN jobs j ON j.kind = 'activity_' || a.resource AND j.key = 'repo:' || a.repository_id::text
WHERE a.repository_id = ANY(sqlc.arg(repository_ids)::bigint[])
  AND a.resource IN ('issues', 'pull_requests', 'releases', 'issue_events', 'comments');

-- name: ListReviewStatuses :many
SELECT c.repository_id, count(*) FILTER (WHERE NOT c.complete) AS pending,
       (CASE WHEN count(*) > 0 AND count(c.last_success_at) = count(*) THEN min(c.last_success_at) END)::timestamptz AS last_success_at,
       ((array_agg(c.last_error_code ORDER BY c.parent_id) FILTER (WHERE c.last_error_code IS NOT NULL))[1])::text AS last_error_code,
       (min(j.run_at) FILTER (WHERE j.state = 'queued'))::timestamptz AS next_attempt_at
FROM activity_children c
LEFT JOIN jobs j ON j.kind = 'activity_reviews' AND j.key = 'repo:' || c.repository_id::text || ':parent:' || c.parent_id::text
WHERE c.repository_id = ANY(sqlc.arg(repository_ids)::bigint[]) AND c.resource = 'reviews'
GROUP BY c.repository_id;

-- name: ListRepositoryArchived :many
SELECT id, archived FROM repositories WHERE id = ANY(sqlc.arg(ids)::bigint[]);
