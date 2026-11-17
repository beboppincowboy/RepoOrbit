-- name: ListActivityStatuses :many
SELECT a.repository_id, a.boundary, a.phase, a.watermark, a.last_success_at, a.last_error_code,
       j.state AS job_state, j.run_at AS job_run_at
FROM activity_resources a
LEFT JOIN jobs j ON j.kind = 'activity_issues' AND j.key = 'repo:' || a.repository_id::text
WHERE a.resource = 'issues' AND a.repository_id = ANY(sqlc.arg(repository_ids)::bigint[]);

