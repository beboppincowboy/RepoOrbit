import { expect, test } from '@playwright/test';
import { repositoryBoardKey } from '../src/features/board/query-keys';
import { issueDraftStorageKey } from '../src/features/issues/drafts';
import { assignableUsersKey, issueCapabilitiesKey, statusLabelsQueryKey, workspaceIssueKey, workspaceIssuesKey, writeOperationsQueryKey } from '../src/features/issues/query-keys';
import { viewerKeyOf } from '../src/features/repositories/access-store';
import { crossRepositoryOverviewKey, ownedRepositoriesKey, repositoryOverviewKey, repositoryOverviewPrefix, repositorySnapshotKey, selectedRepositoriesKey, starredRepositoriesKey, workspacePullRequestsKey } from '../src/features/repositories/query-keys';
import type { ViewerSession } from '../src/shared/api/transport';

// Stored drafts and prefix invalidation depend on these exact shapes. Change a literal only with a migration.
const ada: ViewerSession = { mode: 'real', user: { login: 'ada', demo: false }, csrfToken: 'token' };
const viewer = viewerKeyOf(ada);

test('draft storage keys', () => {
  expect(issueDraftStorageKey(viewer, '42', 'create')).toBe('repoorbit:issue-draft:real:ada:42:create');
  expect(issueDraftStorageKey(viewer, '42', 7)).toBe('repoorbit:issue-draft:real:ada:42:update:7');
  expect(issueDraftStorageKey(viewer, '42', { board: 7 })).toBe('repoorbit:issue-draft:real:ada:42:board:7');
});

test('repository query keys keep their element order', () => {
  expect(repositoryOverviewKey(ada, '42', 30)).toEqual(['repositoryOverview', 'real', 'ada', '42', 30]);
  expect(repositoryOverviewPrefix(ada, '42')).toEqual(['repositoryOverview', 'real', 'ada', '42']);
  expect(selectedRepositoriesKey(ada)).toEqual(['selectedRepositories', 'real', 'ada']);
  expect(crossRepositoryOverviewKey(ada)).toEqual(['crossRepositoryOverview', 'real', 'ada']);
  expect(repositorySnapshotKey(ada, '42', 30)).toEqual(['repositorySnapshot', 'real', 'ada', '42', 30]);
  expect(workspacePullRequestsKey('42', viewer)).toEqual(['workspacePullRequests', '42', 'real:ada']);
  expect(ownedRepositoriesKey).toEqual(['owned']);
  expect(starredRepositoriesKey).toEqual(['starred']);
});

test('Board and issue query keys', () => {
  expect(repositoryBoardKey(viewer, '42')).toEqual(['repositoryBoard', 'real:ada', '42']);
  expect(workspaceIssuesKey('42', viewer)).toEqual(['workspaceIssues', '42', 'real:ada']);
  expect(workspaceIssueKey(viewer, '42', 7)).toEqual(['workspaceIssue', 'real:ada', '42', 7]);
  expect(writeOperationsQueryKey(viewer, '42')).toEqual(['writeOperations', 'real:ada', '42']);
  expect(statusLabelsQueryKey(viewer, '42')).toEqual(['statusLabels', 'real:ada', '42']);
  expect(issueCapabilitiesKey(viewer, '42')).toEqual(['issueCapabilities', 'real:ada', '42']);
  expect(assignableUsersKey(viewer, '42')).toEqual(['assignableUsers', 'real:ada', '42']);
});
