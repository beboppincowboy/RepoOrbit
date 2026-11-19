import { test, expect } from '@playwright/test';

// Runs only under scripts/e2e-auth.sh, which starts a fake GitHub and the API in real-auth mode (E2E_AUTH_URL is set).
test.skip(!process.env.E2E_AUTH_URL, 'needs scripts/e2e-auth.sh');

test('signed-out visitors go to login, sign in with GitHub, and sign out', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByText('Demo mode')).toHaveCount(0);
  await page.getByRole('link', { name: 'Continue with GitHub' }).click();
  await expect(page).toHaveURL(/\/$/);
  await page.getByRole('button', { name: 'Account menu' }).click();
  await expect(page.getByText('Signed in as')).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Account' }).getByText('octocat')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'No repositories yet' })).toBeVisible();
  await expect(page.getByText('orbit/example')).toHaveCount(0); // demo rows never appear in real mode
  // The account menu is still open from the identity checks above.
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto('/');
  await expect(page).toHaveURL(/\/login$/);
});
