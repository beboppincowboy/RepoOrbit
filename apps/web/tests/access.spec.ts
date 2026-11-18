import { test, expect } from '@playwright/test';

// Runs only under scripts/e2e-auth.sh (fake GitHub seeded with ada's public repo "notes" and private repo "secret").
test.skip(!process.env.E2E_AUTH_URL, 'needs scripts/e2e-auth.sh');

test('select an owned repository, then install the app to reveal a private one', async ({ page }) => {
  // The fake GitHub signs in, and installs the app for, the account named by this cookie (cookies ignore ports).
  await page.context().addCookies([{ name: 'fake_as', value: 'ada', url: 'http://127.0.0.1:9090' }]);

  await page.goto('/login');
  await page.getByRole('link', { name: 'Continue with GitHub' }).click();
  await page.getByRole('button', { name: 'Account menu' }).click();
  await expect(page.getByText('Signed in as')).toBeVisible();
  await page.getByRole('link', { name: 'Repositories', exact: true }).click();

  await expect(page.getByRole('checkbox', { name: 'ada/notes' })).toBeVisible();
  await expect(page.getByRole('checkbox', { name: 'ada/secret' })).toHaveCount(0); // private and not installed: not listed
  await expect(page.getByText('Some private repositories may need the GitHub App installed')).toBeVisible();

  await page.getByRole('link', { name: 'Install the app' }).click();
  await expect(page).toHaveURL(/\/repositories\?install=ok$/);
  await expect(page.getByRole('checkbox', { name: 'ada/secret' })).toBeVisible();

  // Nothing is pre-checked on first use; nothing syncs until the change is applied.
  await expect(page.getByRole('checkbox', { name: 'ada/notes' })).not.toBeChecked();
  await page.getByRole('checkbox', { name: 'ada/notes' }).check();
  await page.getByRole('button', { name: 'Start syncing 1 repository' }).click();
  await expect(page.getByText('Connected. Awaiting sync.')).toBeVisible();

  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'ada/notes' })).toBeVisible();
  await expect(page.getByText('Activity import in progress; continuous reconciliation is not yet complete.')).toBeVisible();
  await expect(page.getByText('ada/secret')).toHaveCount(0); // never selected

  await page.getByRole('link', { name: 'Repositories', exact: true }).click();
  await page.getByRole('checkbox', { name: 'ada/notes' }).uncheck();
  await page.getByRole('region', { name: 'Pending changes' }).getByRole('button', { name: 'Apply' }).click();
  await expect(page.getByText('Disconnected. RepoOrbit no longer syncs it for you.')).toBeVisible();
  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByText('No repositories connected yet.')).toBeVisible();

  // The installed private repository is visible to Ada after selection and stays hidden from another account.
  await page.getByRole('link', { name: 'Repositories', exact: true }).click();
  await page.getByRole('checkbox', { name: 'ada/secret' }).check();
  await page.getByRole('button', { name: 'Start syncing 1 repository' }).click();
  await expect(page.getByText('Connected. Awaiting sync.')).toBeVisible();
  await page.goto('/');
  const privateRepo = page.getByRole('heading', { name: 'ada/secret' });
  await expect(privateRepo).toBeVisible();
  await page.getByRole('button', { name: 'Refresh activity' }).click();
  await expect(page.getByText(/Activity refresh requested|A refresh is already queued/)).toBeVisible();

  await page.getByRole('button', { name: 'Account menu' }).click();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.context().addCookies([{ name: 'fake_as', value: 'grace', url: 'http://127.0.0.1:9090' }]);
  await page.getByRole('link', { name: 'Continue with GitHub' }).click();
  await page.getByRole('button', { name: 'Account menu' }).click();
  await expect(page.getByRole('navigation', { name: 'Account' }).getByText('grace')).toBeVisible();
  await expect(page.getByText('No repositories connected yet.')).toBeVisible();
  await expect(page.getByText('ada/secret')).toHaveCount(0);

  // The account menu is still open from the identity check above.
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.context().addCookies([{ name: 'fake_as', value: 'ada', url: 'http://127.0.0.1:9090' }]);
  await page.getByRole('link', { name: 'Continue with GitHub' }).click();
  await page.getByRole('link', { name: 'Repositories', exact: true }).click();
  await page.getByRole('checkbox', { name: 'ada/secret' }).uncheck();
  await page.getByRole('button', { name: 'Apply' }).click();
  await expect(page.getByText('Disconnected. RepoOrbit no longer syncs it for you.')).toBeVisible();
});

test('follow a public starred repository without installing the app', async ({ page }) => {
  await page.context().addCookies([{ name: 'fake_as', value: 'ada', url: 'http://127.0.0.1:9090' }]);
  await page.goto('/login');
  await page.getByRole('link', { name: 'Continue with GitHub' }).click();
  await page.getByRole('button', { name: 'Account menu' }).click();
  await expect(page.getByText('Signed in as')).toBeVisible();
  await page.getByRole('link', { name: 'Repositories', exact: true }).click();
  await page.getByRole('link', { name: 'Starred', exact: true }).click();

  await expect(page.getByRole('heading', { name: 'grace/compiler' })).toBeVisible();
  await expect(page.getByText('Not following')).toBeVisible();
  await page.getByRole('button', { name: 'Follow grace/compiler', exact: true }).click();
  await expect(page.getByText('Following, awaiting sync')).toBeVisible();

  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'grace/compiler' })).toBeVisible();
  await expect(page.getByText('Activity import in progress; continuous reconciliation is not yet complete.')).toBeVisible();

  await page.getByRole('link', { name: 'Repositories', exact: true }).click();
  await page.getByRole('link', { name: 'Starred', exact: true }).click();
  await page.getByRole('button', { name: 'Unfollow grace/compiler', exact: true }).click();
  await page.getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByText('No repositories connected yet.')).toBeVisible();
});
