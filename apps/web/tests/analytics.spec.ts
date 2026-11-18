import { expect, test, type Page } from '@playwright/test';
import { mockSelected, mockSession, selectedRepo, signedIn } from './mocks';

// Workspace (U5c) against a mocked GraphQL API: coverage display, review state, contributors, timeline gap, access and polling.
const covered = { covered: true, reason: null, through: '2026-11-17T08:00:00Z' };
const missing = (reason: string) => ({ covered: false, reason, through: null });
const stream = (over: object = {}) => ({ phase: 'current', boundary: '2026-08-01T00:00:00Z', catchupThrough: null, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, jobState: 'queued', nextAttemptAt: '2026-11-17T09:00:00Z', ...over });
const child = { complete: true, pendingChildren: 0, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, nextAttemptAt: null };
const activity = (over: object = {}) => ({ synchronized: true, message: '', archived: false, issues: stream(), pullRequests: stream(), releases: stream(), reviews: child, comments: child, issueEvents: { traversal: stream(), historicalGap: false }, futureResources: [], ...over });
// An uncovered daily column is null for every day (metrics-definitions 5.6).
const days = (n: number, covered = true) => Array.from({ length: n }, (_, i) => ({
  day: new Date(Date.UTC(2026, 10, 17 - (n - 1 - i))).toISOString(),
  issuesOpened: covered ? i % 3 : null,
  pullRequestsMerged: covered ? i % 2 : null,
}));
const fits = (page: Page) => page.getByRole('main').evaluate(el => el.scrollHeight <= el.clientHeight);

export function overview(over: Record<string, unknown> = {}) {
  return {
    fullName: 'ada/repo-1', syncState: 'awaiting-sync', activity: activity(),
    openIssues: { value: 12, coverage: covered }, staleOpenIssues: { value: 3, coverage: covered },
    pullRequestsMerged: { value: 7, coverage: covered },
    firstResponseTime: { seconds: null, sampleSize: null, coverage: missing('children_incomplete') },
    mergeTime: { seconds: 2 * 86400, sampleSize: 7, coverage: covered }, issuesWithoutResponse: { value: 2 },
    eventsCoveredFrom: '2026-11-03T00:00:00Z', windowStart: '2026-10-19T00:00:00Z',
    activeBotContributors: { value: 1, coverage: covered }, contributorsCoverage: covered,
    contributors: [
      { login: 'sam-k', isBot: false, pullRequestsOpened: 6, reviewsSubmitted: 11, commentsCreated: 9 },
      { login: 'alex', isBot: false, pullRequestsOpened: 9, reviewsSubmitted: 6, commentsCreated: 14 },
      { login: 'dependabot[bot]', isBot: true, pullRequestsOpened: 9, reviewsSubmitted: 0, commentsCreated: 0 },
    ],
    timeline: [
      { sourceId: 1, kind: 'pull_request_merged', occurredAt: '2026-11-16T09:14:00Z', title: 'Fix pagination cursor', number: 142, url: 'https://github.com/ada/repo-1/pull/142', actor: { login: 'alex', isBot: false } },
      { sourceId: 2, kind: 'issue_reopened', occurredAt: '2026-11-10T03:40:00Z', title: 'Login loop', number: 89, url: 'https://github.com/ada/repo-1/issues/89', actor: { login: 'sam-k', isBot: false } },
      { sourceId: 3, kind: 'release_published', occurredAt: '2026-10-28T12:00:00Z', title: 'v1.3', number: null, url: 'https://github.com/ada/repo-1/releases/tag/v1.3', actor: null },
    ],
    daily: days(30),
    ...over,
  };
}

const pulls = [
  { id: 'p1', number: 142, title: 'Fix pagination cursor', state: 'open', url: 'https://github.com/ada/repo-1/pull/142', createdAt: '2026-11-15T00:00:00Z', mergedAt: null, reviewState: 'changes_requested', author: { login: 'alex', type: 'User' } },
  { id: 'p2', number: 143, title: 'Add cache layer', state: 'open', url: 'https://github.com/ada/repo-1/pull/143', createdAt: '2026-11-16T00:00:00Z', mergedAt: null, reviewState: null, author: { login: 'priya', type: 'User' } },
];

// Serves the shell's selected list via mockSelected, then answers the workspace queries. Returns a call counter.
async function mockWorkspace(page: Page, data: () => unknown, errors?: unknown[]) {
  await mockSession(page, signedIn);
  await mockSelected(page, [selectedRepo('1')]);
  const calls = { overview: 0 };
  await page.route('**/graphql', async route => {
    const query = String(route.request().postDataJSON().query);
    if (query.startsWith('query RepositoryOverview')) {
      calls.overview++;
      return route.fulfill({ json: errors ? { data: { repositoryOverview: null }, errors } : { data: { repositoryOverview: data() } } });
    }
    if (query.startsWith('query WorkspacePullRequests')) return route.fulfill({ json: { data: { pullRequestEvidence: { nodes: pulls, withheld: 0, hasNextPage: false } } } });
    return route.fallback();
  });
  return calls;
}

test.beforeEach(async ({ page }) => { await page.setViewportSize({ width: 1440, height: 900 }); });

test('shows — with the coverage reason for an uncovered metric, and real figures otherwise', async ({ page }) => {
  await mockWorkspace(page, () => overview());
  await page.goto('/workspace/1');
  const metrics = page.getByRole('region', { name: 'Repository metrics' });
  const first = metrics.getByRole('group', { name: 'Median first response' });
  await expect(first).toContainText('—');
  await expect(first).toContainText('Partial: reviews and comments importing');
  await expect(metrics.getByRole('group', { name: 'Median merge time' })).toContainText('2.0d');
  await expect(metrics.getByRole('group', { name: 'Open issues' })).toContainText('3 stale');
  await expect(page.getByText('Partial coverage.')).toBeVisible();
  // Nothing is clipped below the fixed viewport.
  expect(await fits(page)).toBe(true);
});

test('chart reads a day on focus and hatches days with no imported history', async ({ page }) => {
  let covered = true;
  await mockWorkspace(page, () => overview({ daily: days(30, covered) }));
  await page.goto('/workspace/1');
  await expect(page.getByText('Hover or focus the chart to read a day.')).toBeVisible();
  await expect(page.getByTestId('chart-hatch')).toHaveCount(0);
  await page.getByRole('button', { name: 'Nov 17' }).focus();
  await expect(page.locator('[aria-live="polite"]').filter({ hasText: 'Nov 17' })).toContainText(/\d+ opened · \d+ merged/);
  covered = false;
  await page.reload();
  await expect(page.getByTestId('chart-hatch')).toBeVisible();
  await page.getByRole('button', { name: 'Oct 19' }).focus();
  await expect(page.getByText('not imported yet', { exact: true })).toBeVisible();
  await expect(page.getByText('PRs merged (not imported yet)')).toBeVisible();
});

test('pull requests table shows review state, and importing instead of a guess', async ({ page }) => {
  await mockWorkspace(page, () => overview());
  await page.goto('/workspace/1?tab=pulls');
  const table = page.getByRole('table');
  await expect(table.getByRole('columnheader', { name: 'Review' })).toBeVisible();
  await expect(table.getByRole('row', { name: /#142/ })).toContainText('Changes requested');
  await expect(table.getByRole('row', { name: /#143/ })).toContainText('Review history importing');
  await expect(table.getByRole('link', { name: '#142' })).toHaveAttribute('href', 'https://github.com/ada/repo-1/pull/142');
});

test('contributors are alphabetical, captioned, with bots counted separately', async ({ page }) => {
  await mockWorkspace(page, () => overview());
  await page.goto('/workspace/1?tab=contributors&days=7');
  await expect(page.getByRole('heading', { name: 'Participation · last 7 days' })).toBeVisible();
  await expect(page.getByText('Alphabetical. Participation, not a productivity ranking.')).toBeVisible();
  const people = page.getByRole('list', { name: 'Contributors' }).getByRole('listitem');
  await expect(people).toHaveCount(2);
  await expect(people.nth(0)).toContainText('alex');
  await expect(people.nth(0)).toContainText('9 PRs · 6 rev · 14 cmt');
  await expect(people.nth(1)).toContainText('sam-k');
  await expect(page.getByText('Bots counted separately: dependabot[bot] 9 PRs')).toBeVisible();
});

test('activity timeline marks the history gap where event coverage starts', async ({ page }) => {
  await mockWorkspace(page, () => overview());
  await page.goto('/workspace/1?tab=activity');
  const rows = page.getByRole('list', { name: 'Activity' }).getByRole('listitem');
  await expect(rows).toHaveCount(4);
  await expect(rows.nth(2)).toHaveText('History gap · issue events before Nov 3 were never recorded');
  await expect(rows.nth(0).getByRole('link')).toHaveAttribute('href', 'https://github.com/ada/repo-1/pull/142');
});

test('access unverified hides the repository name and offers re-verify', async ({ page }) => {
  await mockWorkspace(page, () => null, [{ message: 'access', extensions: { code: 'ACCESS_UNVERIFIED' } }]);
  await page.goto('/workspace/1');
  await expect(page.getByRole('heading', { name: 'Access could not be verified' })).toBeVisible();
  await expect(page.getByText('Protected content is hidden, not deleted.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Re-verify access' })).toBeVisible();
  // The shell's sidebar row comes from the selection list; the workspace itself shows no name.
  await expect(page.getByRole('main').getByText('repo-1')).toHaveCount(0);
});

test('rate limited and awaiting states show their banners', async ({ page }) => {
  let state = 'rate';
  const calls = await mockWorkspace(page, () => state === 'rate'
    ? overview({ activity: activity({ synchronized: false, issues: stream({ failureCode: 'rate_limited', nextAttemptAt: '2026-11-17T14:32:00Z' }) }) })
    : overview({
      activity: activity({ synchronized: false, issues: null, pullRequests: null, releases: null, reviews: null, comments: null, issueEvents: null }),
      ...Object.fromEntries(['openIssues', 'staleOpenIssues', 'pullRequestsMerged', 'activeBotContributors'].map(k => [k, { value: null, coverage: missing('not_synchronized') }])),
      ...Object.fromEntries(['firstResponseTime', 'mergeTime'].map(k => [k, { seconds: null, sampleSize: null, coverage: missing('not_synchronized') }])),
      contributorsCoverage: missing('not_synchronized'), contributors: [], timeline: [], daily: days(30, false), eventsCoveredFrom: null,
    }));
  await page.goto('/workspace/1');
  await expect(page.getByText(/Rate limited\. Showing the last good data from .+; next retry/)).toBeVisible();
  state = 'awaiting';
  await page.reload();
  const before = calls.overview;
  await expect(page.getByText('First sync has not started. Figures show — until data exists.')).toBeVisible();
  await expect(page.getByRole('region', { name: 'Repository metrics' }).getByText('—', { exact: true })).toHaveCount(4);
  await expect(page.getByRole('region', { name: 'Repository metrics' }).getByText('Awaiting data')).toHaveCount(4);
  // A first sync that has not started is queued work, so the page keeps checking.
  await expect.poll(() => calls.overview, { timeout: 15_000 }).toBeGreaterThan(before + 1);
});

test('polls while syncing and stops once settled', async ({ page }) => {
  let syncing = true;
  const calls = await mockWorkspace(page, () => overview({ activity: activity({ synchronized: !syncing, issues: stream({ jobState: syncing ? 'running' : 'queued', phase: syncing ? 'backfill' : 'current' }) }) }));
  await page.goto('/workspace/1');
  await expect(page.getByText(/^Syncing/).first()).toBeVisible();
  await expect.poll(() => calls.overview, { timeout: 15_000 }).toBeGreaterThanOrEqual(3);
  syncing = false;
  await expect(page.locator('[data-sync-state="synced"]')).toBeVisible({ timeout: 15_000 });
  const settled = calls.overview;
  await page.waitForTimeout(6_000);
  expect(calls.overview).toBe(settled);
});

test('the header pill opens the full sync status with Refresh activity', async ({ page }) => {
  await mockWorkspace(page, () => overview({ activity: activity({ synchronized: false, message: 'Activity import in progress; continuous reconciliation is not yet complete.', issues: stream({ phase: 'backfill' }) }) }));
  await page.goto('/workspace/1');
  const toggle = page.getByRole('button', { name: 'Sync details' });
  await expect(toggle.locator('[data-sync-state]')).toHaveText('Partial coverage');
  await expect(page.getByRole('list', { name: 'Activity status' })).toHaveCount(0);
  await toggle.click();
  const lines = page.getByRole('list', { name: 'Activity status' });
  await expect(lines.getByText('Activity import in progress; continuous reconciliation is not yet complete.')).toBeVisible();
  await expect(lines.getByText(/^Issues: importing \(backfill/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Refresh activity' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(lines).toHaveCount(0);
});

test('awaiting sync shows the metadata refresh lines from the selection list', async ({ page }) => {
  await mockSession(page, signedIn);
  await mockSelected(page, [selectedRepo('1', { metadataStatus: { state: 'queued', lastSuccessAt: null, nextAttemptAt: null, paused: false, failureCode: null, followSuspended: false } })]);
  await page.route('**/graphql', route => String(route.request().postDataJSON().query).startsWith('query RepositoryOverview')
    ? route.fulfill({ json: { data: { repositoryOverview: overview() } } }) : route.fallback());
  await page.goto('/workspace/1');
  await page.getByRole('button', { name: 'Sync details' }).click();
  await expect(page.getByRole('list', { name: 'Metadata status' })).toContainText('Metadata refresh queued.');
});

test('a repository the selection list withholds is unavailable, without its name', async ({ page }) => {
  await mockWorkspace(page, () => overview());
  await page.route('**/graphql', route => String(route.request().postDataJSON().query).startsWith('query RepositoryDetails')
    ? route.fulfill({ json: { data: { a0: { activity: null }, r0: { nodes: [], hasNextPage: false, withheld: 1, retryAt: null } } } }) : route.fallback());
  await page.goto('/workspace/1');
  await expect(page.getByRole('heading', { name: 'Repository unavailable' })).toBeVisible();
  await expect(page.locator('[data-sync-state="access-unverified"]')).toHaveText('Access unverified');
  await expect(page.getByRole('main').getByText('repo-1')).toHaveCount(0);
});

test('a failed overview load offers Retry', async ({ page }) => {
  let fail = true;
  await mockWorkspace(page, () => overview());
  await page.route('**/graphql', route => fail && String(route.request().postDataJSON().query).startsWith('query RepositoryOverview')
    ? route.fulfill({ status: 500, body: '' }) : route.fallback());
  await page.goto('/workspace/1');
  await expect(page.getByRole('alert')).toContainText('Could not load repository analytics.');
  fail = false;
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'repo-1', exact: true })).toBeVisible();
});

test('a paused metadata refresh stays on the workspace and explains itself in Sync details', async ({ page }) => {
  await mockSession(page, signedIn);
  await mockSelected(page, [selectedRepo('1', { metadataStatus: { state: 'paused', lastSuccessAt: null, nextAttemptAt: null, paused: true, failureCode: 'no_credential', followSuspended: false } })]);
  await page.route('**/graphql', route => String(route.request().postDataJSON().query).startsWith('query RepositoryOverview')
    ? route.fulfill({ json: { data: { repositoryOverview: overview() } } }) : route.fallback());
  await page.goto('/workspace/1');
  await expect(page.getByRole('heading', { name: 'repo-1', exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Repository unavailable' })).toHaveCount(0);
  const toggle = page.getByRole('button', { name: 'Sync details' });
  await expect(toggle.locator('[data-sync-state]')).toHaveText('Access unverified');
  await toggle.click();
  const lines = page.getByRole('list', { name: 'Metadata status' });
  await expect(lines).toContainText('Metadata refresh paused: no usable GitHub authorization.');
  await expect(lines).toContainText('Last refresh attempt failed (no credential).');
});
