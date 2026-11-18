import { test, expect, type Page } from '@playwright/test';
import { demo, metadata, mockSelected, mockSession, quietActivity, selectedRepo, signedIn } from './mocks';

// Fully mocked API: runs against whatever web server the config starts.
const res = (over: object = {}) => ({ phase: 'current', boundary: '2026-11-01T00:00:00Z', catchupThrough: null, lastSuccessAt: '2026-11-16T08:00:00Z', failureCode: null, jobState: 'idle', nextAttemptAt: null, ...over });

async function mockRepository(page: Page, meta: object | null, activity: object = quietActivity, extra: object = {}, issues: object = {}) {
  await mockSession(page, signedIn);
  await mockSelected(page, [selectedRepo('10', { metadataStatus: meta, activity, ...extra })], issues);
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'ada/repo-10' })).toBeVisible();
}

test('metadata success still says activity is not synchronized', async ({ page }) => {
  await mockRepository(page, metadata({ state: 'queued', lastSuccessAt: '2026-11-16T08:00:00Z', nextAttemptAt: '2026-11-16T08:15:00Z' }), quietActivity, { syncState: 'awaiting-sync' });
  const lines = page.getByRole('list', { name: 'Metadata status' });
  await expect(lines.getByText('Metadata last refreshed')).toBeVisible();
  await expect(lines.getByText('Metadata refresh queued for')).toBeVisible();
  await expect(page.getByRole('list', { name: 'Activity status' }).getByText('Activity import in progress; continuous reconciliation is not yet complete.')).toBeVisible();
});

test('paused refresh is explained and a failure shows a safe code only', async ({ page }) => {
  await mockRepository(page, metadata({ state: 'paused', paused: true, failureCode: 'no_credential' }), quietActivity, { syncState: 'awaiting-sync' });
  await expect(page.getByText('Metadata refresh paused: no usable GitHub authorization.', { exact: false })).toBeVisible();
  await expect(page.getByText('Last refresh attempt failed (no credential).')).toBeVisible();
  await expect(page.getByText('Metadata not refreshed yet.')).toBeVisible();
});

test('dead and unstarred states are reported without credentials or raw errors', async ({ page }) => {
  await mockRepository(page, metadata({ state: 'dead', failureCode: 'unknown', followSuspended: true }));
  await expect(page.getByText('stopped after repeated failures', { exact: false })).toBeVisible();
  await expect(page.getByText('You no longer star this repository', { exact: false })).toBeVisible();
  const text = await page.locator('main').innerText();
  expect(text).not.toMatch(/token|ghu_|ghp_|Bearer|https?:\/\/api\.github/i);
});

test('each activity stream is described separately, with the permanent gap and only safe codes', async ({ page }) => {
  await mockRepository(page, metadata({}), {
    synchronized: false, message: 'Activity import in progress; continuous reconciliation is not yet complete.',
    issues: res(), pullRequests: res({ phase: 'backfill', catchupThrough: '2026-10-01T00:00:00Z' }),
    releases: res({ jobState: 'dead', failureCode: 'github_500: ghp_secret' }),
    reviews: { complete: false, pendingChildren: 3, lastSuccessAt: null, failureCode: null, nextAttemptAt: null }, comments: null,
    issueEvents: { traversal: res({ jobState: 'queued', nextAttemptAt: '2026-11-17T09:00:00Z' }), historicalGap: true }, futureResources: [],
  });
  const lines = page.getByRole('list', { name: 'Activity status' });
  await expect(lines.getByText('Issues: synchronized as of')).toBeVisible();
  await expect(lines.getByText('Pull requests: importing (backfill')).toBeVisible();
  await expect(lines.getByText('Releases: stopped after repeated failures; an operator must requeue it.')).toBeVisible();
  await expect(lines.getByText('Reviews: incomplete, 3 pull requests pending.')).toBeVisible();
  await expect(lines.getByText('Issue events: synchronized')).toBeVisible();
  await expect(lines.getByText('permanently unavailable', { exact: false })).toBeVisible();
  await expect(lines.getByText('All activity streams are synchronized.')).toHaveCount(0);
  expect(await lines.innerText()).not.toMatch(/ghp_|secret/);
});

// One fixture per SyncStatus state (plan U5a). The badge carries data-sync-state, so each fixture checks the state it means.
const stateCases: { state: string; label: string; setup: (page: Page) => Promise<void> }[] = [
  { state: 'synced', label: 'Synced', setup: p => mockRepository(p, null, { ...quietActivity, synchronized: true, message: '', issues: res(), pullRequests: res(), releases: res(), reviews: { complete: true, pendingChildren: 0, lastSuccessAt: '2026-11-16T08:00:00Z', failureCode: null, nextAttemptAt: null }, comments: { complete: true, pendingChildren: 0, lastSuccessAt: '2026-11-16T08:00:00Z', failureCode: null, nextAttemptAt: null }, issueEvents: { traversal: res(), historicalGap: false } }) },
  { state: 'syncing', label: 'Syncing', setup: p => mockRepository(p, metadata({ state: 'running' }), quietActivity, { syncState: 'awaiting-sync' }) },
  { state: 'partial', label: 'Partial coverage', setup: p => mockRepository(p, null, { ...quietActivity, issues: res({ phase: 'backfill', catchupThrough: '2026-10-01T00:00:00Z' }) }) },
  { state: 'awaiting', label: 'Awaiting sync', setup: p => mockRepository(p, null, quietActivity, { syncState: 'awaiting-sync' }) },
  { state: 'rate-limited', label: 'Rate limited', setup: p => mockRepository(p, null, quietActivity, {}, { retryAt: '2026-11-17T09:00:00Z' }) },
  { state: 'access-unverified', label: 'Access unverified', setup: p => mockRepository(p, null, quietActivity, {}, { withheld: 1 }) },
  { state: 'stopped', label: 'Sync stopped', setup: p => mockRepository(p, metadata({ state: 'dead' })) },
];

for (const c of stateCases) {
  test(`SyncStatus renders the ${c.label} state from a fixture`, async ({ page }) => {
    await c.setup(page);
    const badge = page.locator(`[data-sync-state="${c.state}"]`);
    await expect(badge).toHaveText(c.label);
  });
}

test('demo data is always labeled, and the demo fixture never shows a live-data state', async ({ page }) => {
  await mockSession(page, demo);
  await mockSelected(page, [selectedRepo('10', { syncState: 'demo', metadataStatus: null, activity: null, fullName: 'orbit/example' })]);
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'orbit/example' })).toBeVisible();
  await expect(page.locator('[data-sync-state="demo"]')).toHaveText('Demo data');
  await expect(page.getByText('Demo mode', { exact: true })).toBeVisible();
});

const refreshCases = [
  ['accepted', 202, { state: 'accepted' }, {}, 'Activity refresh requested.', 'status'],
  ['coalesced', 202, { state: 'coalesced' }, {}, 'A refresh is already queued or running; it will include your request.', 'status'],
  ['dead', 409, { state: 'dead' }, {}, 'Activity sync stopped after repeated failures; an operator must requeue it.', 'alert'],
  ['not eligible', 404, {}, {}, 'This repository is no longer eligible for activity sync.', 'alert'],
  ['unverifiable', 503, {}, { 'Retry-After': '30' }, 'GitHub could not verify access right now. Try again shortly.', 'alert'],
] as const;

for (const [name, status_, body, headers, expected, role] of refreshCases) {
  test(`refresh activity feedback: ${name}`, async ({ page }) => {
    let csrf: string | undefined;
    await page.route('**/api/repositories/10/activity/refresh', route => { csrf = route.request().headers()['x-csrf-token']; return route.fulfill({ status: status_, headers, json: body }); });
    await mockRepository(page, metadata({}));
    await page.getByRole('button', { name: 'Refresh activity' }).click();
    await expect(page.getByRole(role).filter({ hasText: expected })).toBeVisible();
    expect(csrf).toBe('t');
  });
}
