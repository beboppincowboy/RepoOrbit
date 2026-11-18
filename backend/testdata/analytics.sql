INSERT INTO repositories (id, name, description, is_private) VALUES
  (10, 'test/primary', '', false), (20, 'other/private', '', true);

INSERT INTO activity_resources (repository_id, resource, boundary, phase, watermark)
SELECT 10, resource, '2026-08-18 00:00Z', 'catchup', '2026-11-17 11:00Z'
FROM unnest(ARRAY['issues', 'pull_requests', 'releases', 'issue_events']) AS resource;

INSERT INTO activity_issues (source_id, repository_id, number, title, state, html_url, created_at, updated_at, closed_at, author_id, author_type)
VALUES
  (1, 10, 1, 'Reopened', 'open', 'https://github.com/test/primary/issues/1', '2026-11-11 00:00Z', '2026-11-16 00:00Z', '2026-11-15 00:00Z', 10, 'User'),
  (2, 10, 2, 'Closed', 'closed', '', '2026-11-16 00:00Z', '2026-11-17 00:00Z', '2026-11-17 00:00Z', 10, 'User'),
  (3, 10, 3, 'Before window', 'open', '', '2026-11-10 23:59:59Z', '2026-11-10 23:59:59Z', null, null, null),
  (4, 20, 1, 'Private issue', 'open', '', '2026-11-15 00:00Z', '2026-11-16 00:00Z', null, null, null);

INSERT INTO activity_pull_requests (source_id, repository_id, number, title, state, html_url, created_at, updated_at, merged_at, author_id, author_login, author_type)
VALUES
  (100, 10, 5, 'Human PR', 'closed', '', '2026-11-11 00:00Z', '2026-11-11 01:00Z', '2026-11-11 01:00Z', 10, 'author', 'User'),
  (101, 10, 6, 'Bot PR', 'open', '', '2026-11-17 12:00Z', '2026-11-17 12:00Z', null, 11, 'bot', 'Bot'),
  (102, 10, 7, 'Future PR', 'closed', '', '2026-11-17 12:00:01Z', '2026-11-17 12:00:01Z', null, 12, 'future', 'User'),
  (103, 20, 5, 'Private PR', 'closed', '', '2026-11-17 11:00Z', '2026-11-17 11:30Z', '2026-11-17 11:30Z', 13, 'private', 'User');

INSERT INTO activity_comments (source_id, repository_id, issue_number, body, html_url, created_at, updated_at, author_id, author_type)
SELECT source_id, repo, issue, '', '', ts, ts, actor, kind FROM (VALUES
  (201, 10, 1, '2026-11-11 00:00:01Z'::timestamptz, 10, 'User'),
  (202, 10, 1, '2026-11-11 00:00:02Z', 11, 'Bot'),
  (203, 10, 1, '2026-11-11 00:00:03Z', null, null),
  (204, 10, 1, '2026-11-11 00:00:10Z', 12, 'User'),
  (205, 10, 2, '2026-11-16 00:00:31Z', 12, 'User'),
  (206, 20, 1, '2026-11-11 00:00:04Z', 13, 'User')
) AS comments(source_id, repo, issue, ts, actor, kind);

INSERT INTO activity_reviews (source_id, repository_id, pull_request_id, state, body, html_url, submitted_at, author_id, author_login, author_type)
VALUES
  (301, 10, 100, 'PENDING', '', '', '2026-11-11 00:00:01Z', 14, 'pending', 'User'),
  (302, 10, 100, 'APPROVED', '', '', '2026-11-11 00:00:02Z', 11, 'bot', 'Bot'),
  (303, 10, 100, 'APPROVED', '', '', '2026-11-11 00:00:03Z', 12, 'reviewer', 'User');

INSERT INTO activity_children (repository_id, resource, parent_id, parent_number, watermark)
VALUES
  (10, 'comments', 1, 1, '2026-11-17 10:00Z'),
  (10, 'comments', 2, 2, '2026-11-17 09:00Z'),
  (10, 'comments', 3, 3, '2026-11-17 10:00Z'),
  (10, 'reviews', 100, 5, '2026-11-17 10:00Z'),
  (10, 'reviews', 101, 6, '2026-11-17 10:00Z'),
  (10, 'reviews', 102, 7, '2026-11-17 10:00Z');

INSERT INTO activity_issue_events (source_id, repository_id, issue_id, issue_number, event, url, created_at, observed_at)
VALUES (401, 10, 1, 1, 'reopened', '', '2026-11-16 00:00Z', '2026-11-14 00:00Z');
