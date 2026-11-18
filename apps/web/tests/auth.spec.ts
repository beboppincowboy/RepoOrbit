import { test, expect } from '@playwright/test';

// Runs only under scripts/e2e-auth.sh, which starts a fake GitHub and the API in real-auth mode (E2E_AUTH_URL is set).
test.skip(!process.env.E2E_AUTH_URL, 'needs scripts/e2e-auth.sh');

test('signed-out visitors go to login, sign in with GitHub, and sign out', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByText('Demo mode')).toHaveCount(0);
  await page.getByRole('link', { name: 'Sign in with GitHub' }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByText('Signed in as')).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Account' }).getByText('octocat')).toBeVisible();
  await expect(page.getByText('No repositories connected yet.')).toBeVisible();
  await expect(page.getByText('orbit/example')).toHaveCount(0); // demo rows never appear in real mode
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto('/');
  await expect(page).toHaveURL(/\/login$/);
});
