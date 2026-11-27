import { test, expect, type Page } from '@playwright/test';
import { demo, mockSelected, mockSession, selectedRepo, signedIn } from './mocks';

// Fully mocked API (plan U5d). mockSelected answers the selection; serveOverview answers only CrossRepositoryOverview.
const covered = { covered: true, reason: null, through: null };
const overview = (over: object = {}) => ({
  days: 30, windowStart: '2026-10-18T00:00:00Z', now: '2026-11-17T00:00:00Z',
  repositoriesCounted: 2, repositoriesSyncing: 1, withheld: 0, retryAt: null,
  openIssues: { value: 37, coverage: covered }, reviewBacklog: { value: 2, coverage: covered }, failingWorkflows: { value: 1, coverage: covered }, buildSuccessRate: { rate: 0.75, numerator: 3, denominator: 4, coverage: covered },
  needsAttention: [
    { kind: 'pr_awaiting_review', repositoryId: '1', fullName: 'ada/repo-1', number: 130, title: 'Add rotor tests', url: 'https://github.com/ada/repo-1/pull/130', since: '2026-11-10T00:00:00Z' },
    { kind: 'issue_no_response', repositoryId: '1', fullName: 'ada/repo-1', number: 37, title: 'Crash on start', url: 'https://github.com/ada/repo-1/issues/37', since: '2026-11-01T00:00:00Z' },
    { kind: 'stale_issue', repositoryId: '2', fullName: 'ada/repo-2', number: 4, title: 'Old docs', url: 'https://github.com/ada/repo-2/issues/4', since: '2026-09-01T00:00:00Z' },
  ],
  assignedToMe: [{ repositoryId: '2', fullName: 'ada/repo-2', number: 9, title: 'Fix the login flow', url: 'https://github.com/ada/repo-2/issues/9', updatedAt: '2026-11-15T00:00:00Z', status: null }],
  daily: Array.from({ length: 30 }, (_, i) => ({ day: new Date(Date.UTC(2026, 9, 19 + i)).toISOString(), issuesOpened: i % 4, pullRequestsMerged: i % 3 })),
  ...over,
});

type Answer = { data?: unknown; errors?: unknown[] };
// Serves CrossRepositoryOverview from `answers` in order (the last one repeats). Returns the requested windows.
async function serveOverview(page: Page, ...answers: Answer[]) {
  const calls: number[] = [];
  await page.route('**/graphql', route => {
    const body = route.request().postDataJSON();
    if (!String(body.query).startsWith('query CrossRepositoryOverview')) return route.fallback();
    calls.push(body.variables.days);
    return route.fulfill({ json: answers[Math.min(calls.length - 1, answers.length - 1)] });
  });
  return calls;
}
const ok = (over: object = {}): Answer => ({ data: { crossRepositoryOverview: overview(over) } });
const unverified: Answer = { data: { crossRepositoryOverview: null }, errors: [{ message: 'access', extensions: { code: 'ACCESS_UNVERIFIED' } }] };

async function open(page: Page, url = '/', ...answers: Answer[]) {
  await mockSession(page, signedIn);
  await mockSelected(page, ['1', '2', '3'].map(id => selectedRepo(id)));
  const calls = await serveOverview(page, ...(answers.length ? answers : [ok()]));
  await page.goto(url);
  return calls;
}

test('shows the engineering overview with totals, attention rows and assigned issues', async ({ page }) => {
  await open(page);
  await expect(page.getByRole('heading', { name: 'Engineering overview', level: 1 })).toBeVisible();
  await expect(page.getByText('Activity and delivery across 3 repositories · 1 still syncing (left out)')).toBeVisible();
  const stats = page.getByRole('region', { name: 'Totals' });
  await expect(stats.getByText('37', { exact: true })).toBeVisible();
  await expect(stats.getByText('Review backlog')).toBeVisible();
  const attention = page.getByRole('region', { name: 'Needs attention' });
  await expect(attention.getByRole('link')).toHaveCount(3);
  await expect(attention.getByRole('link', { name: /repo-1 · PR #130/ })).toContainText('Awaiting first review');
  await expect(attention.getByRole('link', { name: /repo-2 · Issue #4/ })).toContainText('Open, 30+ days quiet');
  const mine = page.getByRole('region', { name: 'Assigned to me' });
  await expect(mine.getByRole('link', { name: /Fix the login flow/ })).toHaveAttribute('href', /\/workspace\/2/);
  await expect(page.getByRole('region', { name: 'Engineering activity' })).toBeVisible();
});

test('the window is kept in the URL and survives a reload', async ({ page }) => {
  const calls = await open(page);
  await page.getByRole('link', { name: '90d', exact: true }).click();
  await expect(page).toHaveURL(/days=90/);
  await expect(page.getByRole('link', { name: '90d', exact: true })).toHaveAttribute('aria-current', 'page');
  await page.reload();
  await expect(page.getByRole('link', { name: '90d', exact: true })).toHaveAttribute('aria-current', 'page');
  expect(calls).toContain(90);
});

test('an unknown metric shows a dash and its reason, never zero', async ({ page }) => {
  await open(page, '/', ok({ reviewBacklog: { value: null, coverage: { covered: false, reason: 'children_incomplete', through: null } } }));
  const stats = page.getByRole('region', { name: 'Totals' });
  await expect(stats.getByText('—', { exact: true })).toBeVisible();
  await expect(stats.getByText('Related activity is still importing')).toBeVisible();
  await expect(stats.getByText('0', { exact: true })).toHaveCount(0);
});

test('withheld repositories are counted without names', async ({ page }) => {
  await open(page, '/', ok({ withheld: 2 }));
  await expect(page.getByText('2 repositories hidden (access could not be verified)')).toBeVisible();
});

test('with every selected repository withheld, the overview says so instead of showing the empty state', async ({ page }) => {
  await mockSession(page, signedIn);
  await page.route('**/graphql', route => route.fulfill({ json: { data: { selectedRepositories: { nodes: [], endCursor: null, hasNextPage: false, withheld: 2, retryAt: null } } } }));
  await page.goto('/');
  await expect(page.getByText('2 repositories hidden (access could not be verified)')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'No repositories yet' })).toHaveCount(0);
});

test('an empty selection page does not suppress the cross-repository overview', async ({ page }) => {
  await mockSession(page, signedIn);
  await page.route('**/graphql', route => route.fulfill({ json: { data: { selectedRepositories: {
    nodes: [], endCursor: '20', hasNextPage: true, withheld: 0, retryAt: null,
  } } } }));
  await serveOverview(page, ok());
  await page.goto('/');
  await expect(page.getByRole('region', { name: 'Totals' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'No repositories yet' })).toHaveCount(0);
});

test('the overview selection limit explains how to recover', async ({ page }) => {
  await open(page, '/', { data: { crossRepositoryOverview: null }, errors: [{
    message: 'The overview supports up to 100 selected repositories. Disconnect or unfollow repositories to view your overview.',
    extensions: { code: 'OVERVIEW_SELECTION_LIMIT' },
  }] });
  await expect(page.getByRole('alert')).toContainText('Disconnect or unfollow repositories');
});

test('one access-unverified read is retried once and then shows the data', async ({ page }) => {
  const calls = await open(page, '/', unverified, ok());
  await expect(page.getByRole('region', { name: 'Needs attention' }).getByRole('link')).toHaveCount(3);
  expect(calls).toHaveLength(2);
});

test('a second access-unverified read shows the access error', async ({ page }) => {
  const calls = await open(page, '/', unverified, unverified);
  await expect(page.getByRole('alert')).toContainText('Access could not be verified');
  expect(calls).toHaveLength(2);
});

test('assigned issues are grouped by Board status and open on the Board with the drawer', async ({ page }) => {
  const issue = (number: number, title: string, status: string | null) => ({ repositoryId: '2', fullName: 'ada/repo-2', number, title, url: `https://github.com/ada/repo-2/issues/${number}`, updatedAt: '2026-11-15T00:00:00Z', status });
  await open(page, '/', ok({ assignedToMe: [issue(5, 'No labels here', null), issue(3, 'Review me', 'in_review'), issue(9, 'Fix the login flow', 'todo')] }));
  const mine = page.getByRole('region', { name: 'Assigned to me' });
  await expect(mine.getByRole('heading', { level: 3 })).toHaveText(['Todo 1', 'In Review 1', 'No status source 1']);
  await expect(mine.getByRole('group', { name: /Todo/ }).getByRole('link')).toHaveText([/Fix the login flow/]);
  await expect(mine.getByRole('group', { name: /No status source/ }).getByRole('link')).toHaveText([/No labels here/]);
  await mine.getByRole('link', { name: /Review me/ }).click();
  await expect(page).toHaveURL(/\/workspace\/2\?.*tab=board/);
  await expect(page).toHaveURL(/issue=3/);
});

test('empty lists use the prototype wording', async ({ page }) => {
  await open(page, '/', ok({ needsAttention: [], assignedToMe: [] }));
  await expect(page.getByText('Nothing needs attention right now.')).toBeVisible();
  await expect(page.getByText('No open issues are assigned to you.')).toBeVisible();
});

test('a folded panel keeps its header and count, and stays folded after a reload', async ({ page }) => {
  await open(page);
  const fold = page.getByRole('button', { name: /Needs attention/ });
  await expect(fold).toHaveAttribute('aria-expanded', 'true');
  await fold.click();
  await expect(fold).toHaveAttribute('aria-expanded', 'false');
  await expect(fold).toContainText('3');
  await expect(page.getByRole('link', { name: /repo-1 · PR #130/ })).toHaveCount(0);
  await page.reload();
  await expect(page.getByRole('button', { name: /Needs attention/ })).toHaveAttribute('aria-expanded', 'false');
  await page.getByRole('button', { name: /Needs attention/ }).click();
  await expect(page.getByRole('link', { name: /repo-1 · PR #130/ })).toBeVisible();
});

test('with nothing selected, the empty state links to Repositories', async ({ page }) => {
  await mockSession(page, demo);
  await mockSelected(page, []);
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'No repositories yet' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Choose repositories' })).toHaveAttribute('href', '/repositories');
});

test('the page does not scroll at 1440x900, even with long lists', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  const many = Array.from({ length: 25 }, (_, i) => ({ kind: 'stale_issue', repositoryId: '1', fullName: 'ada/repo-1', number: i + 1, title: `Issue ${i + 1}`, url: '', since: '2026-09-01T00:00:00Z' }));
  await open(page, '/', ok({ needsAttention: many }));
  await expect(page.getByRole('region', { name: 'Needs attention' }).getByRole('link')).toHaveCount(25);
  await page.evaluate(() => window.scrollBy(0, 400));
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
  expect(await page.evaluate(() => document.documentElement.scrollHeight)).toBeLessThanOrEqual(900);
  const shell = await page.locator('[data-app-shell]').boundingBox();
  expect(shell!.y + shell!.height).toBeLessThanOrEqual(900);
});
