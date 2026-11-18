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
  // Scrolling the window does nothing: the document is no taller than the viewport.
  await page.evaluate(() => window.scrollBy(0, 400));
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
  expect(await page.evaluate(() => document.documentElement.scrollHeight)).toBeLessThanOrEqual(height);
  // The long list scrolls inside its own region.
  const inner = page.locator('main .overflow-y-auto').first();
  expect(await inner.evaluate(el => el.scrollHeight > el.clientHeight)).toBe(true);
}

for (const size of sizes) {
  const name = `${size.width}x${size.height}`;
  test(`overview fits ${name} and scrolls only inside its list`, async ({ page }) => {
    await page.setViewportSize(size);
    await mockSession(page, demo);
    await mockSelected(page, forty().map(id => selectedRepo(id, { fullName: `orbit/repo-${id}`, syncState: 'demo', activity: null })));
    await page.goto('/');
    await expect(page.getByRole('heading', { name: 'orbit/repo-40' })).toBeAttached();
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
