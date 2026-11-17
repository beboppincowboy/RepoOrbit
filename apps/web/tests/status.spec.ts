import { test, expect, type Page } from '@playwright/test';
// Fully mocked API: runs against any running web server (default the compose one; set STATUS_E2E_URL to use another).
const base = process.env.STATUS_E2E_URL ?? 'http://127.0.0.1:3000';
const activity = { synchronized: false, message: 'Selected, awaiting sync — no activity data yet.' };
const status = (over: object) => ({ state: 'queued', lastSuccessAt: null, nextAttemptAt: null, paused: false, failureCode: null, followSuspended: false, ...over });

async function mockRepository(page: Page, metadataStatus: object | null, act: object = activity) {
  await page.route('**/auth/me', route => route.fulfill({ json: { mode: 'real', user: { login: 'ada', demo: false }, csrfToken: 't' } }));
  await page.route('**/graphql', async route => {
    const body = route.request().postDataJSON();
    if (String(body.query).startsWith('query Issues')) return route.fulfill({ json: { data: { r0: { nodes: [], hasNextPage: false } } } });
    await route.fulfill({ json: { data: { selectedRepositories: { nodes: [{ id: '10', fullName: 'ada/notes', description: 'd', syncState: 'awaiting-sync', metadataStatus, activity: act }], endCursor: '10', hasNextPage: false, withheld: 0, retryAt: null } } } });
  });
  await page.goto(base);
  await expect(page.getByRole('heading', { name: 'ada/notes' })).toBeVisible();
}

test('metadata success still says activity is not synchronized', async ({ page }) => {
  await mockRepository(page, status({ state: 'queued', lastSuccessAt: '2026-11-16T08:00:00Z', nextAttemptAt: '2026-11-16T08:15:00Z' }));
  const lines = page.getByRole('list', { name: 'Metadata status' });
  await expect(lines.getByText('Metadata last refreshed')).toBeVisible();
  await expect(lines.getByText('Metadata refresh queued for')).toBeVisible();
  await expect(page.getByText('Selected, awaiting sync — no activity data yet.')).toBeVisible();
});

test('paused refresh is explained and a failure shows a safe code only', async ({ page }) => {
  await mockRepository(page, status({ state: 'paused', paused: true, failureCode: 'no_credential' }));
  await expect(page.getByText('Metadata refresh paused: no usable GitHub authorization.', { exact: false })).toBeVisible();
  await expect(page.getByText('Last refresh attempt failed (no credential).')).toBeVisible();
  await expect(page.getByText('Metadata not refreshed yet.')).toBeVisible();
  await expect(page.getByText('Selected, awaiting sync — no activity data yet.')).toBeVisible();
});

test('dead and unstarred states are reported without credentials or raw errors', async ({ page }) => {
  await mockRepository(page, status({ state: 'dead', failureCode: 'unknown', followSuspended: true }));
  await expect(page.getByText('stopped after repeated failures', { exact: false })).toBeVisible();
  await expect(page.getByText('You no longer star this repository', { exact: false })).toBeVisible();
  const text = await page.locator('main').innerText();
  expect(text).not.toMatch(/token|ghu_|ghp_|Bearer|https?:\/\/api\.github/i);
});

const res = (over: object) => ({ phase: 'current', boundary: '2026-11-01T00:00:00Z', catchupThrough: null, lastSuccessAt: '2026-11-16T08:00:00Z', failureCode: null, jobState: 'idle', nextAttemptAt: null, ...over });

test('each activity stream is described separately, with the permanent gap and only safe codes', async ({ page }) => {
  await mockRepository(page, status({}), {
    synchronized: false, message: 'Selected, awaiting sync — no activity data yet.',
    issues: res({}), pullRequests: res({ phase: 'backfill', catchupThrough: '2026-10-01T00:00:00Z' }),
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

for (const [name, status_, body, headers, expected, role] of [
  ['accepted', 202, { state: 'accepted' }, {}, 'Activity refresh requested.', 'status'],
  ['coalesced', 202, { state: 'coalesced' }, {}, 'A refresh is already queued or running; it will include your request.', 'status'],
  ['dead', 409, { state: 'dead' }, {}, 'Activity sync stopped after repeated failures; an operator must requeue it.', 'alert'],
  ['not eligible', 404, {}, {}, 'This repository is no longer eligible for activity sync.', 'alert'],
  ['unverifiable', 503, {}, { 'Retry-After': '30' }, 'GitHub could not verify access right now. Try again shortly.', 'alert'],
] as const) {
  test(`refresh activity feedback: ${name}`, async ({ page }) => {
    let csrf: string | undefined;
    await page.route('**/api/repositories/10/activity/refresh', route => { csrf = route.request().headers()['x-csrf-token']; return route.fulfill({ status: status_, headers, json: body }); });
    await mockRepository(page, status({}));
    await page.getByRole('button', { name: 'Refresh activity' }).click();
    await expect(page.getByRole(role).filter({ hasText: expected })).toBeVisible();
    expect(csrf).toBe('t');
  });
}
