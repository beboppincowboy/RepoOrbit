-- name: UpsertJob :exec
INSERT INTO jobs (kind, key, state, run_at) VALUES ($1, $2, 'queued', sqlc.arg(run_at))
ON CONFLICT (kind, key) DO UPDATE SET
 run_at = CASE WHEN jobs.state = 'queued' THEN LEAST(jobs.run_at, EXCLUDED.run_at) ELSE jobs.run_at END,
 pending = CASE WHEN jobs.state = 'queued' THEN jobs.pending ELSE true END,
 updated_at = now();

-- name: ResumeJob :exec
UPDATE jobs SET state = 'queued', run_at = sqlc.arg(run_at), pending = false, lease_until = NULL, updated_at = now()
WHERE kind = $1 AND key = $2 AND state = 'paused';

-- name: RequeueJob :execrows
UPDATE jobs SET state = 'queued', run_at = sqlc.arg(run_at), pending = false, attempts = 0, lease_until = NULL, last_error = '', updated_at = now()
WHERE kind = $1 AND key = $2 AND state = 'dead';

-- name: KillExhaustedJobs :exec
UPDATE jobs SET state = 'dead', lease_until = NULL, last_error = 'lease expired after final attempt', updated_at = now()
WHERE state = 'running' AND lease_until <= sqlc.arg(now) AND attempts + 1 >= sqlc.arg(max_attempts)::int;

-- name: ClaimJob :one
UPDATE jobs SET state = 'running', generation = generation + 1, lease_until = sqlc.arg(lease_until), started_at = sqlc.arg(now),
 attempts = CASE WHEN state = 'running' THEN attempts + 1 ELSE attempts END, updated_at = now()
WHERE id = (
 SELECT j.id FROM jobs j
 WHERE j.kind = ANY(sqlc.arg(kinds)::text[])
 AND ((j.state = 'queued' AND j.run_at <= sqlc.arg(now)) OR (j.state = 'running' AND j.lease_until <= sqlc.arg(now)))
 ORDER BY j.run_at, j.id LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING id, kind, key, generation, attempts;

-- name: HeartbeatJob :execrows
UPDATE jobs SET lease_until = sqlc.arg(lease_until), updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'running' AND lease_until > sqlc.arg(now);

-- name: CheckClaim :one
SELECT id FROM jobs WHERE id = $1 AND generation = $2 AND state = 'running' AND lease_until > sqlc.arg(now) FOR SHARE;

-- name: DeleteFinishedJob :execrows
DELETE FROM jobs WHERE id = $1 AND generation = $2 AND state = 'running' AND lease_until > sqlc.arg(now) AND NOT pending;

-- name: FollowUpJob :execrows
UPDATE jobs SET state = 'queued', run_at = sqlc.arg(now), pending = false, attempts = 0, lease_until = NULL, last_error = '', updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'running' AND lease_until > sqlc.arg(now) AND pending;

-- name: DeferJob :execrows
UPDATE jobs SET state = 'queued', run_at = sqlc.arg(run_at), pending = false, lease_until = NULL, last_error = sqlc.arg(reason), updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'running' AND lease_until > sqlc.arg(now);

-- name: PauseJob :execrows
-- A request that arrived while the job ran (pending) is not erased by the pause: the job goes back to queued, no attempt
-- spent, and re-evaluates with the new request. With nothing pending it parks until Resume.
UPDATE jobs SET state = CASE WHEN pending THEN 'queued' ELSE 'paused' END,
 run_at = CASE WHEN pending THEN sqlc.arg(now) ELSE run_at END,
 pending = false, lease_until = NULL, last_error = sqlc.arg(reason), updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'running' AND lease_until > sqlc.arg(now);

-- name: ReleaseJob :execrows
UPDATE jobs SET state = 'queued', run_at = sqlc.arg(now), lease_until = NULL, updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'running';

-- name: RetryJob :execrows
UPDATE jobs SET attempts = attempts + 1,
 state = CASE WHEN attempts + 1 >= sqlc.arg(max_attempts)::int THEN 'dead' ELSE 'queued' END,
 run_at = sqlc.arg(run_at), pending = CASE WHEN attempts + 1 >= sqlc.arg(max_attempts)::int THEN pending ELSE false END,
 lease_until = NULL, last_error = sqlc.arg(reason), updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'running' AND lease_until > sqlc.arg(now);

-- name: KillJob :execrows
UPDATE jobs SET state = 'dead', lease_until = NULL, last_error = sqlc.arg(reason), updated_at = now()
WHERE id = $1 AND generation = $2 AND state = 'running' AND lease_until > sqlc.arg(now);
