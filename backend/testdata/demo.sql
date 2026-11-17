INSERT INTO repositories (id, name, description, is_demo) VALUES
 (1, 'orbit/example', 'A sample repository for exploring RepoOrbit.', true),
 (2, 'orbit/launchpad', 'A second sample repository.', true)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
 WHERE repositories.is_demo;
INSERT INTO issues (id, repository_id, title, state) VALUES
 (1, 1, 'Improve onboarding documentation', 'open'),
 (2, 1, 'Add keyboard shortcuts', 'open'),
 (3, 2, 'Document the release process', 'open')
ON CONFLICT (id) DO UPDATE SET repository_id = EXCLUDED.repository_id, title = EXCLUDED.title, state = EXCLUDED.state
 WHERE EXISTS (SELECT 1 FROM repositories r WHERE r.id = issues.repository_id AND r.is_demo);
