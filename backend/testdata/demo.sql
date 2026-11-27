-- orbit/example has status labels on, so its Board has columns; orbit/launchpad shows "No status source".
INSERT INTO repositories (id, name, description, is_demo, status_labels) VALUES
 (1, 'orbit/example', 'A sample repository for exploring RepoOrbit.', true, true),
 (2, 'orbit/launchpad', 'A second sample repository.', true, false)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description, status_labels = EXCLUDED.status_labels
 WHERE repositories.is_demo;
-- Demo activity evidence for the analytics screens. Timestamps are relative to seeding time, and every Demo start
-- reseeds, replacing these rows. Assignee source_id 1 stands in for the Demo viewer (graphql demoAssigneeID).
DELETE FROM workflow_jobs WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM workflow_runs WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM workflows WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM deployments WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM issue_pull_request_links WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM issues WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_issues WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_pull_requests WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_comments WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_children WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_resources WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark, last_success_at)
SELECT r.id, res, now() - interval '90 days', 'current', now(), now()
FROM repositories r CROSS JOIN unnest(ARRAY['issues', 'pull_requests', 'releases', 'issue_events', 'comments', 'actions', 'deployments']) AS res WHERE r.is_demo;
WITH people(k, login) AS (VALUES (0, 'maya'), (1, 'sam'), (2, 'jordan'), (3, 'priya'), (4, 'alex')),
issues AS (
  SELECT r.id AS repo, r.name, g, 900000000 + r.id * 1000 + g AS source_id, now() - g * interval '53 hours' AS created
  FROM repositories r CROSS JOIN LATERAL generate_series(1, CASE WHEN r.id = 1 THEN 40 ELSE 15 END) AS g WHERE r.is_demo
)
INSERT INTO activity_issues (source_id, repository_id, number, title, state, html_url, created_at, updated_at, closed_at, author_id, author_login, author_type)
SELECT i.source_id, i.repo, i.g, 'Sample issue ' || i.g, CASE WHEN i.g % 3 = 0 THEN 'closed' ELSE 'open' END,
  'https://github.com/' || i.name || '/issues/' || i.g, i.created,
  CASE WHEN i.g % 7 = 0 THEN i.created ELSE i.created + interval '1 hour' END,
  CASE WHEN i.g % 3 = 0 THEN i.created + interval '1 day' END,
  9000 + i.g % 5, p.login, 'User'
FROM issues i JOIN people p ON p.k = i.g % 5;
INSERT INTO activity_comments (source_id, repository_id, issue_number, body, html_url, created_at, updated_at, author_id, author_login, author_type)
SELECT i.source_id + 500, i.repository_id, i.number, 'Sample reply', i.html_url, i.created_at + (i.number % 5 + 1) * interval '1 hour',
  i.created_at + (i.number % 5 + 1) * interval '1 hour', 9000 + (i.number + 1) % 5, p.login, 'User'
FROM activity_issues i JOIN (VALUES (0, 'maya'), (1, 'sam'), (2, 'jordan'), (3, 'priya'), (4, 'alex')) AS p(k, login) ON p.k = (i.number + 1) % 5
WHERE i.repository_id IN (SELECT id FROM repositories WHERE is_demo) AND i.number % 4 <> 1;
-- Status labels on a few open orbit/example issues; #7 carries two (flagged, shown in In Review).
INSERT INTO activity_issue_labels (issue_id, source_id, name)
SELECT i.source_id, l.id, l.name
FROM activity_issues i JOIN repositories r ON r.id = i.repository_id AND r.is_demo AND r.id = 1
JOIN (VALUES (2, 2, 'status:in-progress'), (4, 1, 'status:todo'), (5, 3, 'status:review'), (7, 1, 'status:todo'), (7, 3, 'status:review'),
  (8, 2, 'status:in-progress'), (10, 1, 'status:todo')) AS l(number, id, name) ON l.number = i.number;
INSERT INTO activity_issue_assignees (issue_id, source_id, login, type)
SELECT source_id, 1, 'demo-user', 'User' FROM activity_issues
WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo) AND state = 'open' AND number % 6 = 2;
WITH pulls AS (
  SELECT r.id AS repo, r.name, g, 910000000 + r.id * 1000 + 31 - g AS source_id, now() - g * interval '61 hours' AS created
  FROM repositories r CROSS JOIN LATERAL generate_series(1, CASE WHEN r.id = 1 THEN 30 ELSE 10 END) AS g WHERE r.is_demo
)
INSERT INTO activity_pull_requests (source_id, repository_id, number, title, state, html_url, created_at, updated_at, closed_at, merged_at, author_id, author_login, author_type)
SELECT source_id, repo, 100 + g, CASE WHEN g % 8 = 0 THEN 'Bump dependencies' ELSE 'Sample change ' || g END,
  CASE WHEN g % 3 = 0 THEN 'open' ELSE 'closed' END, 'https://github.com/' || name || '/pull/' || (100 + g), created,
  created + (g % 4 + 1) * interval '3 hours',
  CASE WHEN g % 3 <> 0 THEN created + (g % 4 + 1) * interval '3 hours' END,
  CASE WHEN g % 3 <> 0 THEN created + (g % 4 + 1) * interval '3 hours' END,
  CASE WHEN g % 8 = 0 THEN 49699333 ELSE 9000 + g % 5 END,
  CASE WHEN g % 8 = 0 THEN 'dependabot[bot]' ELSE (ARRAY['maya', 'sam', 'jordan', 'priya', 'alex'])[g % 5 + 1] END,
  CASE WHEN g % 8 = 0 THEN 'Bot' ELSE 'User' END
FROM pulls;
INSERT INTO activity_reviews (source_id, repository_id, pull_request_id, state, body, html_url, submitted_at, author_id, author_login, author_type)
SELECT p.source_id + 500, p.repository_id, p.source_id, CASE WHEN p.number % 6 = 0 THEN 'CHANGES_REQUESTED' ELSE 'APPROVED' END, '', p.html_url,
  p.created_at + interval '2 hours', 9000 + (p.number + 2) % 5, (ARRAY['maya', 'sam', 'jordan', 'priya', 'alex'])[(p.number + 2) % 5 + 1], 'User'
FROM activity_pull_requests p
WHERE p.repository_id IN (SELECT id FROM repositories WHERE is_demo) AND p.number % 5 <> 0;
INSERT INTO activity_children (repository_id, resource, parent_id, parent_number, complete, watermark)
SELECT repository_id, 'reviews', source_id, number, true, now() FROM activity_pull_requests WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);

UPDATE activity_pull_requests SET head_sha = lpad(to_hex(repository_id * 1000 + number), 40, '0')
WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
INSERT INTO workflows (repository_id, source_id, name, path, state, html_url, updated_at)
SELECT r.id, w.id, w.name, '.github/workflows/' || w.path, 'active',
  'https://github.com/' || r.name || '/actions/workflows/' || w.path, now()
FROM repositories r JOIN (VALUES (1, 11, 'CI', 'ci.yml'), (1, 12, 'Integration Tests', 'integration.yml'),
  (1, 13, 'Release', 'release.yml'), (2, 21, 'Deploy', 'deploy.yml'), (2, 22, 'Integration Tests', 'integration.yml'))
  AS w(repo, id, name, path) ON w.repo = r.id AND r.is_demo;
INSERT INTO workflow_runs (repository_id, source_id, workflow_id, workflow_name, run_number, event, status, conclusion,
  head_branch, head_sha, html_url, created_at, run_started_at, updated_at, pull_request_numbers)
SELECT r.id, v.id, v.workflow_id, w.name, v.id, 'pull_request', v.status, v.conclusion, 'main',
  p.head_sha, 'https://github.com/' || r.name || '/actions/runs/' || v.id,
  now() - v.age * interval '1 day' - interval '10 minutes', now() - v.age * interval '1 day' - interval '8 minutes',
  CASE WHEN v.status = 'completed' THEN now() - v.age * interval '1 day' + interval '12 minutes' ELSE now() END,
  ARRAY[p.number]::bigint[]
FROM (VALUES (1, 1001, 11, 101, 1, 'completed', 'success'), (1, 1002, 12, 103, 2, 'completed', 'failure'),
  (1, 1003, 11, 104, 4, 'completed', 'success'), (1, 1004, 13, 105, 10, 'completed', 'success'),
  (1, 1005, 12, 106, 15, 'completed', 'failure'), (1, 1006, 11, 103, 0, 'in_progress', ''),
  (2, 2001, 21, 101, 3, 'completed', 'success'), (2, 2002, 22, 103, 2, 'completed', 'failure'))
  AS v(repo, id, workflow_id, pr, age, status, conclusion)
JOIN repositories r ON r.id = v.repo AND r.is_demo
JOIN workflows w ON w.repository_id = r.id AND w.source_id = v.workflow_id
JOIN activity_pull_requests p ON p.repository_id = r.id AND p.number = v.pr;
INSERT INTO workflow_jobs (repository_id, source_id, run_id, name, status, conclusion, html_url, created_at, started_at, completed_at)
SELECT run.repository_id, run.source_id * 10, run.source_id,
  CASE WHEN run.workflow_name = 'Integration Tests' THEN 'integration' ELSE 'build' END,
  run.status, run.conclusion, run.html_url || '/job/' || (run.source_id * 10), run.created_at,
  run.run_started_at, CASE WHEN run.status = 'completed' THEN run.updated_at END
FROM workflow_runs run JOIN repositories r ON r.id = run.repository_id AND r.is_demo;
INSERT INTO issue_pull_request_links (repository_id, issue_number, pull_request_number, source, created_at)
SELECT id, 2, 103, 'timeline', now() - interval '3 days' FROM repositories WHERE is_demo AND id IN (1, 2);
INSERT INTO deployments (repository_id, source_id, environment, sha, ref, state, description, html_url, created_at, updated_at)
SELECT p.repository_id, 3000 + p.number, v.environment, p.head_sha, 'main', 'success', 'Sample deployment',
  'https://github.com/orbit/example/deployments', now() - v.age * interval '1 day', now() - v.age * interval '1 day' + interval '5 minutes'
FROM (VALUES (101, 'production', 2), (104, 'staging', 5)) AS v(pr, environment, age)
JOIN activity_pull_requests p ON p.repository_id = 1 AND p.number = v.pr
JOIN repositories r ON r.id = p.repository_id AND r.is_demo;
