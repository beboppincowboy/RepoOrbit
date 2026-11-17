import { test, expect, type Page } from '@playwright/test';
// Fully mocked API: runs against any running web server (default the compose one; set STATUS_E2E_URL to use another).
const base = process.env.STATUS_E2E_URL ?? 'http://127.0.0.1:3000';
const activity = { synchronized: false, message: 'Selected, awaiting sync — no activity data yet.' };
const status = (over: object) => ({ state: 'queued', lastSuccessAt: null, nextAttemptAt: null, paused: false, failureCode: null, followSuspended: false, ...over });

async function mockRepository(page: Page, metadataStatus: object | null) {
  await page.route('**/auth/me', route => route.fulfill({ json: { mode: 'real', user: { login: 'ada', demo: false }, csrfToken: 't' } }));
  await page.route('**/graphql', async route => {
    const body = route.request().postDataJSON();
    if (String(body.query).startsWith('query Issues')) return route.fulfill({ json: { data: { r0: { nodes: [], hasNextPage: false } } } });
    await route.fulfill({ json: { data: { selectedRepositories: { nodes: [{ id: '10', fullName: 'ada/notes', description: 'd', syncState: 'awaiting-sync', metadataStatus, activity }], endCursor: '10', hasNextPage: false, withheld: 0, retryAt: null } } } });
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
