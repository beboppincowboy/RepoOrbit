import { test, expect } from '@playwright/test';
import { listed, mockOwned, mockSession, mockStarred, signedIn, signedOut } from './mocks';

// Direct links and reloads work through the production server (the shell is served for every path).
test('a repositories deep link restores its filters and survives a reload', async ({ page }) => {
  await mockSession(page, signedIn);
  await mockOwned(page, [listed('1', { selected: true }), listed('2')]);
  await page.goto('/repositories?q=repo-1&view=selected');
  await expect(page.getByRole('searchbox', { name: 'Search repositories' })).toHaveValue('repo-1');
  await expect(page.getByRole('button', { name: 'Connected' })).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByRole('checkbox', { name: 'ada/repo-2' })).toHaveCount(0);

  await page.reload();
  await expect(page).toHaveURL(/\/repositories\?q=repo-1&view=selected$/);
  await expect(page.getByRole('searchbox', { name: 'Search repositories' })).toHaveValue('repo-1');
  await expect(page.getByRole('checkbox', { name: 'ada/repo-1' })).toBeChecked();
});

test('a starred deep link restores its search after a reload', async ({ page }) => {
  await mockSession(page, signedIn);
  await mockStarred(page, [listed('1', { fullName: 'grace/compiler' }), listed('2', { fullName: 'ada/other' })]);
  await page.goto('/starred?q=grace');
  await expect(page.getByRole('heading', { name: 'grace/compiler' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'ada/other' })).toHaveCount(0);
  await page.reload();
  await expect(page.getByRole('searchbox', { name: 'Search starred repositories' })).toHaveValue('grace');
  await expect(page.getByRole('heading', { name: 'grace/compiler' })).toBeVisible();
});

test('a signed-out deep link goes to sign-in', async ({ page }) => {
  await mockSession(page, signedOut);
  await page.goto('/repositories');
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole('link', { name: 'Sign in with GitHub' })).toBeVisible();
});

test('a failed sign-in is reported on the login page', async ({ page }) => {
  await mockSession(page, signedOut);
  await page.goto('/login?error=access_denied');
  await expect(page.getByRole('alert').filter({ hasText: 'Sign-in did not complete.' })).toBeVisible();
});

test('a failed session load can be retried on login', async ({ page }) => {
  await page.route('**/auth/me', route => route.fulfill({ status: 503 }));
  await page.goto('/login');
  await expect(page.getByRole('alert')).toContainText('Could not load the session.');
  await mockSession(page, signedOut);
  await page.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByRole('link', { name: 'Sign in with GitHub' })).toBeVisible();
});
