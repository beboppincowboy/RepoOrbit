import { test, expect, type Page } from '@playwright/test';
import { demo, listed, mockOwned, mockSelected, mockSession, mockStarred, selectedRepo, signedIn } from './mocks';

// The page itself never scrolls: the shell fits the viewport, and only inner lists scroll.
const sizes = [{ width: 1280, height: 720 }, { width: 1440, height: 900 }, { width: 1920, height: 1080 }];
const forty = () => Array.from({ length: 40 }, (_, n) => String(n + 1));

async function expectNoPageScroll(page: Page) {
  const { width, height } = page.viewportSize()!;
  const shell = await page.locator('[data-app-shell]').boundingBox();
  expect(shell).not.toBeNull();
  expect(shell!.y + shell!.height).toBeLessThanOrEqual(height);
  expect(shell!.width).toBeLessThanOrEqual(width);
  expect(shell!.width).toBeGreaterThanOrEqual(width - 32);
  const sidebar = await page.getByRole("complementary", { name: "Main" }).boundingBox();
  expect(sidebar?.width).toBe(216);
  // Scrolling the window does nothing: the document is no taller than the viewport.
  await page.evaluate(() => window.scrollBy(0, 400));
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
  expect(await page.evaluate(() => document.documentElement.scrollHeight)).toBeLessThanOrEqual(height);
  // The long list scrolls inside its own region.
  const inner = page.locator('main .overflow-y-auto').first();
  expect(await inner.evaluate(el => el.scrollHeight > el.clientHeight)).toBe(true);
}

// Answers only the Engineering overview query, with 25 Needs attention rows (the API's cap) so that list must scroll.
async function mockLongOverview(page: Page) {
  const coverage = { covered: true, reason: null, through: null };
  const needsAttention = forty().slice(0, 25).map(id => ({ kind: 'stale_issue', repositoryId: id, fullName: `orbit/repo-${id}`, number: Number(id), title: `Quiet issue ${id}`, url: '', since: '2026-09-01T00:00:00Z' }));
  await page.route('**/graphql', route => {
    if (!String(route.request().postDataJSON().query).startsWith('query CrossRepositoryOverview')) return route.fallback();
    return route.fulfill({ json: { data: { crossRepositoryOverview: {
      days: 30, windowStart: '2026-10-18T00:00:00Z', now: '2026-11-17T00:00:00Z', repositoriesCounted: 40, repositoriesSyncing: 0, withheld: 0, retryAt: null,
      openIssues: { value: 40, coverage }, reviewBacklog: { value: 0, coverage }, failingWorkflows: { value: 0, coverage }, buildSuccessRate: { rate: null, numerator: null, denominator: null, coverage }, needsAttention, assignedToMe: [], daily: [],
    } } } });
  });
}

for (const size of sizes) {
  const name = `${size.width}x${size.height}`;
  test(`overview fits ${name} and scrolls only inside its list`, async ({ page }) => {
    await page.setViewportSize(size);
    await mockSession(page, demo);
    await mockSelected(page, forty().map(id => selectedRepo(id, { fullName: `orbit/repo-${id}`, syncState: 'demo', activity: null })));
    await mockLongOverview(page);
    await page.goto('/');
    await expect(page.getByRole('link', { name: /repo-25 · Issue #25/ })).toBeAttached();
    await expectNoPageScroll(page);
  });

  test(`repositories fit ${name} and scroll only inside the list`, async ({ page }) => {
    await page.setViewportSize(size);
    await mockSession(page, signedIn);
    await mockOwned(page, forty().map(id => listed(id)));
    await page.goto('/repositories');
    await expect(page.getByRole('checkbox', { name: 'ada/repo-40' })).toBeAttached();
    await expectNoPageScroll(page);
  });

  test(`starred fits ${name} and scrolls only inside the list`, async ({ page }) => {
    await page.setViewportSize(size);
    await mockSession(page, signedIn);
    await mockStarred(page, forty().map(id => listed(id)));
    await page.goto('/starred');
    await expect(page.getByRole('heading', { name: 'ada/repo-40' })).toBeAttached();
    await expectNoPageScroll(page);
  });
}
