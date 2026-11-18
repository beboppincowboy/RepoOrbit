-- Repository metrics (docs/metrics-definitions.md). Every query reads exactly one repository_id; callers check access first.
-- A window is [window_start, now]; stale_before = today - 29 days.

-- name: GetMetricCoverage :one
-- Stream watermarks and boundaries, the issue-event coverage start, and per-parent-set child coverage
-- (missing = qualifying parents whose child history row is absent or has never converged).
WITH pr_children AS (
  SELECT
    count(*) FILTER (WHERE p.state = 'open' AND c.watermark IS NULL) AS open_missing,
    min(c.watermark) FILTER (WHERE p.state = 'open') AS open_through,
    count(*) FILTER (WHERE p.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz AND c.watermark IS NULL) AS created_missing,
    min(c.watermark) FILTER (WHERE p.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz) AS created_through,
    count(*) FILTER (WHERE p.updated_at >= sqlc.arg(window_start)::timestamptz AND c.watermark IS NULL) AS updated_missing,
    min(c.watermark) FILTER (WHERE p.updated_at >= sqlc.arg(window_start)::timestamptz) AS updated_through
  FROM activity_pull_requests p
  LEFT JOIN activity_children c ON c.repository_id = p.repository_id AND c.resource = 'reviews' AND c.parent_id = p.source_id
  WHERE p.repository_id = sqlc.arg(repository_id)
), issue_children AS (
  SELECT
    count(*) FILTER (WHERE i.state = 'open' AND c.watermark IS NULL) AS open_missing,
    min(c.watermark) FILTER (WHERE i.state = 'open') AS open_through,
    count(*) FILTER (WHERE i.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz AND c.watermark IS NULL) AS created_missing,
    min(c.watermark) FILTER (WHERE i.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz) AS created_through
  FROM activity_issues i
  LEFT JOIN activity_children c ON c.repository_id = i.repository_id AND c.resource = 'comments' AND c.parent_id = i.number
  WHERE i.repository_id = sqlc.arg(repository_id)
)
SELECT
  si.boundary AS issues_boundary, si.watermark AS issues_watermark,
  sp.boundary AS pull_requests_boundary, sp.watermark AS pull_requests_watermark,
  sr.boundary AS releases_boundary, sr.watermark AS releases_watermark,
  se.boundary AS issue_events_boundary, se.watermark AS issue_events_watermark,
  (SELECT min(e.observed_at) FROM activity_issue_events e WHERE e.repository_id = sqlc.arg(repository_id))::timestamptz AS events_covered_from,
  pc.open_missing AS open_pr_reviews_missing, pc.open_through::timestamptz AS open_pr_reviews_through,
  pc.created_missing AS created_pr_reviews_missing, pc.created_through::timestamptz AS created_pr_reviews_through,
  pc.updated_missing AS updated_pr_reviews_missing, pc.updated_through::timestamptz AS updated_pr_reviews_through,
  ic.open_missing AS open_issue_comments_missing, ic.open_through::timestamptz AS open_issue_comments_through,
  ic.created_missing AS created_issue_comments_missing, ic.created_through::timestamptz AS created_issue_comments_through
FROM pr_children pc CROSS JOIN issue_children ic
LEFT JOIN activity_resources si ON si.repository_id = sqlc.arg(repository_id) AND si.resource = 'issues'
LEFT JOIN activity_resources sp ON sp.repository_id = sqlc.arg(repository_id) AND sp.resource = 'pull_requests'
LEFT JOIN activity_resources sr ON sr.repository_id = sqlc.arg(repository_id) AND sr.resource = 'releases'
LEFT JOIN activity_resources se ON se.repository_id = sqlc.arg(repository_id) AND se.resource = 'issue_events';

-- name: GetMetricCounts :one
-- Raw counts and medians. Coverage is applied by the caller; a median is 0 when its sample is empty.
WITH first_comments AS (
  SELECT i.source_id, min(c.created_at) AS responded_at
  FROM activity_comments c
  JOIN activity_issues i ON i.repository_id = c.repository_id AND i.number = c.issue_number
  WHERE c.repository_id = sqlc.arg(repository_id)
    AND c.author_id IS NOT NULL AND c.author_type <> 'Bot' AND c.author_id IS DISTINCT FROM i.author_id
    AND c.created_at <= sqlc.arg(now)::timestamptz
  GROUP BY i.source_id
), first_reviews AS (
  SELECT p.source_id, min(r.submitted_at) AS reviewed_at
  FROM activity_reviews r
  JOIN activity_pull_requests p ON p.repository_id = r.repository_id AND p.source_id = r.pull_request_id
  WHERE r.repository_id = sqlc.arg(repository_id) AND upper(r.state) <> 'PENDING'
    AND r.author_id IS NOT NULL AND r.author_type <> 'Bot' AND r.author_id IS DISTINCT FROM p.author_id
    AND r.submitted_at <= sqlc.arg(now)::timestamptz
  GROUP BY p.source_id
), issues AS (
  SELECT i.*, c.responded_at,
    EXISTS (SELECT 1 FROM activity_issue_assignees a WHERE a.issue_id = i.source_id) AS assigned,
    i.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz AS in_window
  FROM activity_issues i LEFT JOIN first_comments c ON c.source_id = i.source_id
  WHERE i.repository_id = sqlc.arg(repository_id)
), pulls AS (
  SELECT p.*, r.reviewed_at,
    p.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz AS in_window,
    p.merged_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz AS merged_in_window
  FROM activity_pull_requests p LEFT JOIN first_reviews r ON r.source_id = p.source_id
  WHERE p.repository_id = sqlc.arg(repository_id)
), issue_stats AS (
  SELECT
    count(*) FILTER (WHERE state = 'open') AS open_issues,
    count(*) FILTER (WHERE state = 'open' AND assigned) AS open_issues_assigned,
    count(*) FILTER (WHERE state = 'open' AND updated_at < sqlc.arg(stale_before)::timestamptz) AS stale_open_issues,
    count(*) FILTER (WHERE state = 'open' AND responded_at IS NULL) AS unanswered_open_issues,
    count(*) FILTER (WHERE in_window) AS issues_opened,
    count(*) FILTER (WHERE state = 'closed' AND closed_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz) AS issues_closed,
    count(*) FILTER (WHERE in_window AND responded_at IS NOT NULL) AS first_response_sample,
    count(*) FILTER (WHERE in_window AND responded_at IS NULL) AS issues_without_response,
    COALESCE(floor(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM responded_at - created_at)::float8)
      FILTER (WHERE in_window AND responded_at IS NOT NULL)), 0)::bigint AS first_response_seconds
  FROM issues
), pull_stats AS (
  SELECT
    count(*) FILTER (WHERE state = 'open') AS open_pull_requests,
    count(*) FILTER (WHERE state = 'open' AND updated_at < sqlc.arg(stale_before)::timestamptz) AS stale_open_pull_requests,
    count(*) FILTER (WHERE state = 'open' AND reviewed_at IS NULL) AS unreviewed_open_pull_requests,
    count(*) FILTER (WHERE in_window) AS pull_requests_opened,
    count(*) FILTER (WHERE merged_in_window) AS pull_requests_merged,
    count(*) FILTER (WHERE in_window AND reviewed_at IS NOT NULL) AS first_review_sample,
    count(*) FILTER (WHERE in_window AND reviewed_at IS NULL) AS pull_requests_never_reviewed,
    COALESCE(floor(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM reviewed_at - created_at)::float8)
      FILTER (WHERE in_window AND reviewed_at IS NOT NULL)), 0)::bigint AS first_review_seconds,
    COALESCE(floor(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM merged_at - created_at)::float8)
      FILTER (WHERE merged_in_window)), 0)::bigint AS merge_seconds
  FROM pulls
), participants AS (
  SELECT ap.author_id, ap.author_type FROM activity_pull_requests ap
  WHERE ap.repository_id = sqlc.arg(repository_id) AND ap.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
  UNION
  SELECT ar.author_id, ar.author_type FROM activity_reviews ar
  WHERE ar.repository_id = sqlc.arg(repository_id) AND upper(ar.state) <> 'PENDING'
    AND ar.submitted_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
)
SELECT
  s.open_issues, s.open_issues_assigned, s.stale_open_issues, s.unanswered_open_issues, s.issues_opened, s.issues_closed,
  s.first_response_sample, s.issues_without_response, s.first_response_seconds,
  p.open_pull_requests, p.stale_open_pull_requests, p.unreviewed_open_pull_requests, p.pull_requests_opened, p.pull_requests_merged,
  p.first_review_sample, p.pull_requests_never_reviewed, p.first_review_seconds, p.merge_seconds,
  (SELECT count(*) FROM activity_issue_events e JOIN activity_issues i ON i.source_id = e.issue_id AND i.repository_id = e.repository_id
   WHERE e.repository_id = sqlc.arg(repository_id) AND e.event = 'reopened'
     AND e.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz) AS issues_reopened,
  (SELECT count(*) FROM activity_releases r WHERE r.repository_id = sqlc.arg(repository_id)
     AND r.published_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz) AS releases_published,
  (SELECT count(DISTINCT author_id) FROM participants WHERE author_id IS NOT NULL AND author_type <> 'Bot') AS active_contributors,
  (SELECT count(DISTINCT author_id) FROM participants WHERE author_id IS NOT NULL AND author_type = 'Bot') AS active_bot_contributors
FROM issue_stats s CROSS JOIN pull_stats p;

-- name: ListMetricContributors :many
-- Up to 20 active humans; login is taken from the author's most recent contributing row.
WITH contributions AS (
  SELECT p.author_id, p.author_login, p.created_at AS at, p.source_id, 1 AS opened, 0 AS reviewed FROM activity_pull_requests p
  WHERE p.repository_id = sqlc.arg(repository_id) AND p.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
    AND p.author_id IS NOT NULL AND p.author_type <> 'Bot'
  UNION ALL
  SELECT r.author_id, r.author_login, r.submitted_at, r.source_id, 0, 1 FROM activity_reviews r
  WHERE r.repository_id = sqlc.arg(repository_id) AND upper(r.state) <> 'PENDING'
    AND r.submitted_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
    AND r.author_id IS NOT NULL AND r.author_type <> 'Bot'
), people AS (
  SELECT c.author_id,
    COALESCE((array_agg(c.author_login ORDER BY c.at DESC, c.source_id DESC))[1], '')::text AS login,
    sum(c.opened)::bigint AS pull_requests_opened, sum(c.reviewed)::bigint AS reviews_submitted
  FROM contributions c GROUP BY c.author_id
)
SELECT author_id, login, pull_requests_opened, reviews_submitted FROM people
ORDER BY pull_requests_opened + reviews_submitted DESC, login ASC, author_id ASC
LIMIT 20;

-- name: ListMetricTimeline :many
-- Up to 50 entries from the groups whose coverage holds (include_* flags). Issue events start at events_from.
SELECT kind, occurred_at, source_id, actor_id, actor_login, actor_type, title, label, number, url FROM (
  SELECT 'release_published'::text AS kind, r.published_at AS occurred_at, r.source_id,
    r.author_id AS actor_id, r.author_login AS actor_login, r.author_type AS actor_type,
    r.name AS title, NULL::text AS label, NULL::bigint AS number, r.html_url AS url
  FROM activity_releases r
  WHERE sqlc.arg(include_releases)::bool AND r.repository_id = sqlc.arg(repository_id)
    AND r.published_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
  UNION ALL
  SELECT 'pull_request_merged', p.merged_at, p.source_id, p.author_id, p.author_login, p.author_type, p.title, NULL, p.number, p.html_url
  FROM activity_pull_requests p
  WHERE sqlc.arg(include_merged)::bool AND p.repository_id = sqlc.arg(repository_id)
    AND p.merged_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
  UNION ALL
  SELECT 'issue_opened', i.created_at, i.source_id, i.author_id, i.author_login, i.author_type, i.title, NULL, i.number, i.html_url
  FROM activity_issues i
  WHERE sqlc.arg(include_issues)::bool AND i.repository_id = sqlc.arg(repository_id)
    AND i.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
  UNION ALL
  SELECT 'issue_' || e.event, e.created_at, e.source_id, e.actor_id, e.actor_login, e.actor_type, i.title, e.label, e.issue_number, e.url
  FROM activity_issue_events e JOIN activity_issues i ON i.source_id = e.issue_id AND i.repository_id = e.repository_id
  WHERE sqlc.arg(include_events)::bool AND e.repository_id = sqlc.arg(repository_id)
    AND e.event IN ('closed', 'reopened', 'assigned', 'unassigned', 'labeled', 'unlabeled')
    AND e.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz
    AND e.created_at >= sqlc.arg(events_from)::timestamptz
) t
ORDER BY occurred_at DESC, kind ASC, source_id DESC
LIMIT 50;

-- name: ListMetricDaily :many
-- One row per UTC day of the window (zero-filled), using the same rows as the windowed counts.
WITH days AS (
  SELECT ((sqlc.arg(window_start)::timestamptz AT TIME ZONE 'UTC')::date + g)::date AS day
  FROM generate_series(0, sqlc.arg(days)::int - 1) AS g
), io AS (
  SELECT (xio.created_at AT TIME ZONE 'UTC')::date AS day, count(*) AS n FROM activity_issues xio
  WHERE xio.repository_id = sqlc.arg(repository_id) AND xio.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz GROUP BY 1
), ic AS (
  SELECT (xic.closed_at AT TIME ZONE 'UTC')::date AS day, count(*) AS n FROM activity_issues xic
  WHERE xic.repository_id = sqlc.arg(repository_id) AND xic.state = 'closed' AND xic.closed_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz GROUP BY 1
), po AS (
  SELECT (xpo.created_at AT TIME ZONE 'UTC')::date AS day, count(*) AS n FROM activity_pull_requests xpo
  WHERE xpo.repository_id = sqlc.arg(repository_id) AND xpo.created_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz GROUP BY 1
), pm AS (
  SELECT (xpm.merged_at AT TIME ZONE 'UTC')::date AS day, count(*) AS n FROM activity_pull_requests xpm
  WHERE xpm.repository_id = sqlc.arg(repository_id) AND xpm.merged_at BETWEEN sqlc.arg(window_start)::timestamptz AND sqlc.arg(now)::timestamptz GROUP BY 1
)
SELECT d.day, COALESCE(io.n, 0)::bigint AS issues_opened, COALESCE(ic.n, 0)::bigint AS issues_closed,
  COALESCE(po.n, 0)::bigint AS pull_requests_opened, COALESCE(pm.n, 0)::bigint AS pull_requests_merged
FROM days d
LEFT JOIN io ON io.day = d.day LEFT JOIN ic ON ic.day = d.day LEFT JOIN po ON po.day = d.day LEFT JOIN pm ON pm.day = d.day
ORDER BY d.day;
