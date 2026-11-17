import { test, expect } from '@playwright/test';
// Runs only under scripts/e2e-auth.sh (fake GitHub seeded with ada's public repo "notes" and private repo "secret").
const base = process.env.E2E_AUTH_URL;
test.skip(!base, 'needs scripts/e2e-auth.sh');

test('connect an owned repository, then install the app to reveal a private one', async ({ page }) => {
  // The fake GitHub signs in, and installs the app for, the account named by this cookie (cookies ignore ports).
  await page.context().addCookies([{ name: 'fake_as', value: 'ada', url: 'http://127.0.0.1:9090' }]);

  await page.goto(`${base}/login`);
  await page.getByRole('link', { name: 'Sign in with GitHub' }).click();
  await expect(page.getByText('Signed in as')).toBeVisible();
  await page.getByRole('link', { name: 'Repositories', exact: true }).click();

  await expect(page.getByRole('heading', { name: /ada\/notes/ })).toBeVisible();
  await expect(page.getByText('ada/secret')).toHaveCount(0); // private and not installed: not listed
  await expect(page.getByText('Some private repositories may need the GitHub App installed')).toBeVisible();

  await page.getByRole('link', { name: 'Install the app' }).click();
  await expect(page).toHaveURL(`${base}/repositories?install=ok`);
  await expect(page.getByRole('heading', { name: /ada\/secret/ })).toBeVisible();

  await page.getByRole('button', { name: 'Connect ada/notes', exact: true }).click();
  await expect(page.getByText('Selected, awaiting sync')).toBeVisible();

  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'ada/notes' })).toBeVisible();
  await expect(page.getByText('Selected, awaiting sync — no activity data yet.')).toBeVisible();
  await expect(page.getByText('ada/secret')).toHaveCount(0); // never selected

  await page.getByRole('link', { name: 'Repositories', exact: true }).click();
  await page.getByRole('button', { name: 'Disconnect ada/notes', exact: true }).click();
  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByText('No repositories connected yet.')).toBeVisible();
});

test('follow a public starred repository without installing the app', async ({ page }) => {
  await page.context().addCookies([{ name: 'fake_as', value: 'ada', url: 'http://127.0.0.1:9090' }]);
  await page.goto(`${base}/login`);
  await page.getByRole('link', { name: 'Sign in with GitHub' }).click();
  await expect(page.getByText('Signed in as')).toBeVisible();
  await page.getByRole('link', { name: 'Starred', exact: true }).click();

  await expect(page.getByRole('heading', { name: 'grace/compiler' })).toBeVisible();
  await expect(page.getByText('Not following')).toBeVisible();
  await page.getByRole('button', { name: 'Follow grace/compiler', exact: true }).click();
  await expect(page.getByText('Following, awaiting sync')).toBeVisible();

  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'grace/compiler' })).toBeVisible();
  await expect(page.getByText('Selected, awaiting sync — no activity data yet.')).toBeVisible();

  await page.getByRole('link', { name: 'Starred', exact: true }).click();
  await page.getByRole('button', { name: 'Unfollow grace/compiler', exact: true }).click();
  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByText('No repositories connected yet.')).toBeVisible();
});
