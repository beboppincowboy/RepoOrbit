import { test, expect } from '@playwright/test';
import { listed, mockOwned, mockSession, mockStarred, signedIn } from './mocks';

// R2 checkbox selection, against a mocked owned list. Nothing reaches the server until Apply.
test('first use: nothing is pre-checked and nothing syncs until confirm', async ({ page }) => {
  await mockSession(page, signedIn);
  const writes = await mockOwned(page, [listed('1'), listed('2'), listed('3')]);
  await page.goto('/repositories');
  await expect(page.getByRole('heading', { name: 'Choose what RepoOrbit keeps in sync' })).toBeVisible();
  await expect(page.getByRole('checkbox', { name: /^ada\/repo-/ })).toHaveCount(3);
  await expect(page.getByRole('checkbox', { checked: true })).toHaveCount(0);
  // First use always shows the bar, with nothing to start until something is checked.
  const bar = page.getByRole('region', { name: 'Pending changes' });
  await expect(bar).toContainText('Pick at least one repository');
  await expect(bar.getByRole('button', { name: 'Start syncing repositories' })).toBeDisabled();

  // Checking and then unchecking the same repository leaves no pending change and sends nothing.
  await page.getByRole('checkbox', { name: 'ada/repo-1' }).check();
  await expect(bar).toContainText('1 selected');
  await page.getByRole('checkbox', { name: 'ada/repo-1' }).uncheck();
  await expect(bar).toContainText('Pick at least one repository');
  await expect(bar.getByRole('button', { name: 'Start syncing repositories' })).toBeDisabled();
  expect(writes).toEqual([]);
});

test('confirm applies each repository, and a failed one stays unchecked with its reason', async ({ page }) => {
  await mockSession(page, signedIn);
  const writes = await mockOwned(page, [listed('1'), listed('2'), listed('3')], { '2': 503 });
  await page.goto('/repositories');
  await page.getByRole('checkbox', { name: 'ada/repo-1' }).check();
  await page.getByRole('checkbox', { name: 'ada/repo-2' }).check();
  await page.getByRole('button', { name: 'Connected' }).click();
  const bar = page.getByRole('region', { name: 'Pending changes' });
  await expect(bar).toContainText('2 selected');
  await bar.getByRole('button', { name: 'Start syncing 2 repositories' }).click();

  await expect(page.getByRole('status').filter({ hasText: '1 applied, 1 failed.' })).toBeVisible();
  expect(writes).toEqual(['PUT 1', 'PUT 2']);
  await expect(page.getByRole('checkbox', { name: 'ada/repo-1' })).toBeChecked();
  await expect(page.getByRole('checkbox', { name: 'ada/repo-2' })).not.toBeChecked();
  await expect(page.getByText('Connected. Awaiting sync.')).toBeVisible();
  await expect(page.getByRole('alert').filter({ hasText: 'GitHub could not verify access right now.' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Pending changes' })).toHaveCount(0);
});

test('removing a connected repository explains what happens, and Apply sends the removal', async ({ page }) => {
  await mockSession(page, signedIn);
  const writes = await mockOwned(page, [listed('1', { selected: true })]);
  await page.goto('/repositories');
  await expect(page.getByRole('checkbox', { name: 'ada/repo-1' })).toBeChecked();
  await page.getByRole('checkbox', { name: 'ada/repo-1' }).uncheck();

  const bar = page.getByRole('region', { name: 'Pending changes' });
  await expect(bar).toContainText('1 to remove');
  await expect(bar).toContainText('stops RepoOrbit syncing it for you.');
  await expect(bar).toContainText('Other people who selected it keep syncing.');
  await expect(bar).toContainText('Nothing changes on GitHub.');
  await expect(bar).toContainText('Saved issues remain available while you have access.');
  await bar.getByRole('button', { name: 'Apply' }).click();
  await expect(page.getByText('Disconnected. RepoOrbit no longer syncs it for you.')).toBeVisible();
  expect(writes).toEqual(['DELETE 1']);
  await expect(page.getByRole('checkbox', { name: 'ada/repo-1' })).not.toBeChecked();
});

test('discard drops the pending changes without sending anything', async ({ page }) => {
  await mockSession(page, signedIn);
  const writes = await mockOwned(page, [listed('1')]);
  await page.goto('/repositories');
  await page.getByRole('checkbox', { name: 'ada/repo-1' }).check();
  const bar = page.getByRole('region', { name: 'Pending changes' });
  await bar.getByRole('button', { name: 'Discard' }).click();
  await expect(bar).toContainText('Pick at least one repository');
  await expect(page.getByRole('checkbox', { name: 'ada/repo-1' })).not.toBeChecked();
  expect(writes).toEqual([]);
});

test('starred repositories keep controls disabled while saving a follow', async ({ page }) => {
  await mockSession(page, signedIn);
  await mockStarred(page, [listed('1'), listed('2')]);
  let finishFollow!: () => void;
  const follow = new Promise<void>(resolve => { finishFollow = resolve; });
  await page.route('**/api/repositories/1/follow', async route => {
    await follow;
    await route.fulfill({ status: 204 });
  });
  await page.goto('/starred');
  await page.getByRole('button', { name: 'Follow ada/repo-1' }).click();
  try {
    await expect(page.getByRole('button', { name: 'Follow ada/repo-2' })).toBeDisabled();
  } finally {
    finishFollow();
  }
  await expect(page.getByRole('button', { name: 'Unfollow ada/repo-1' })).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Follow ada/repo-2' })).toBeEnabled();
});
