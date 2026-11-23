INSERT INTO repositories (id, name, description, is_demo) VALUES
 (1, 'orbit/example', 'A sample repository for exploring RepoOrbit.', true),
 (2, 'orbit/launchpad', 'A second sample repository.', true)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
 WHERE repositories.is_demo;
-- Demo activity evidence for the analytics screens. Timestamps are relative to seeding time, and every Demo start
-- reseeds, replacing these rows. Assignee source_id 1 stands in for the Demo viewer (graphql demoAssigneeID).
DELETE FROM issues WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_issues WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_pull_requests WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_comments WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_children WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
DELETE FROM activity_resources WHERE repository_id IN (SELECT id FROM repositories WHERE is_demo);
INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark, last_success_at)
SELECT r.id, res, now() - interval '90 days', 'current', now(), now()
FROM repositories r CROSS JOIN unnest(ARRAY['issues', 'pull_requests', 'releases', 'issue_events', 'comments']) AS res WHERE r.is_demo;
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
