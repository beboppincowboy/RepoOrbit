import { test, expect } from '@playwright/test';
import { mockOwned, mockSession, quietActivity, selectedRepo, signedIn } from './mocks';

const issuePage = (id: string) => ({
  nodes: [{ id, repositoryId: id, title: `Issue from repository ${id}`, state: 'open' }],
  hasNextPage: false, withheld: 0, retryAt: null,
});

test('loads ordered chunks with two requests in flight and retries only a failed chunk', async ({ page }) => {
  await mockSession(page, signedIn);
  const repositories = Array.from({ length: 20 }, (_, i) => selectedRepo(String(i + 1)));
  const calls: string[][] = [];
  let listCalls = 0;
  let inFlight = 0;
  let peak = 0;
  let releaseInitial!: () => void;
  const initial = new Promise<void>(resolve => { releaseInitial = resolve; });
  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (query.startsWith('query SelectedRepositories')) {
      listCalls++;
      return route.fulfill({ json: { data: { selectedRepositories: { nodes: repositories, withheld: 0, hasNextPage: false } } } });
    }
    const ids = Object.values(variables as Record<string, string>);
    calls.push(ids);
    peak = Math.max(peak, ++inFlight);
    try {
      if (ids[0] !== '17') await initial;
      if (ids[0] === '9' && calls.filter(chunk => chunk[0] === '9').length === 1) {
        return await route.fulfill({ status: 502 });
      }
      const data = Object.fromEntries(ids.flatMap((id, i) => [[`a${i}`, { activity: quietActivity }], [`r${i}`, issuePage(id)]]));
      await route.fulfill({ json: { data } });
    } finally {
      inFlight--;
    }
  });

  await page.goto('/');
  try {
    await expect.poll(() => inFlight).toBe(2);
  } finally {
    releaseInitial();
  }
  await expect(page.getByRole('heading', { name: /^ada\/repo-/ })).toHaveText(repositories.map(repo => repo.fullName));
  await expect(page.getByRole('listitem').filter({ hasText: /^Issue from repository 20 \(open\)$/ })).toBeVisible();
  await expect(page.getByRole('listitem').filter({ hasText: /^Issue from repository 9 \(open\)$/ })).toHaveCount(0);
  await expect(page.getByRole('alert')).toContainText('Could not load repository details for this batch.');
  await page.getByRole('button', { name: 'Retry details' }).click();
  await expect(page.getByRole('listitem').filter({ hasText: /^Issue from repository 9 \(open\)$/ })).toBeVisible();
  await expect(page.getByRole('alert')).toHaveCount(0);
  expect(peak).toBe(2);
  expect(listCalls).toBe(1);
  expect(calls.map(ids => ids.length)).toEqual([8, 8, 4, 8]);
  expect(calls[3]).toEqual(calls.find(ids => ids[0] === '9'));
});

test('a failed access check clears previously loaded details and keeps the other repository', async ({ page }) => {
  await mockSession(page, signedIn);
  let revalidation: 'initial' | 'unverified' | 'list-error' = 'initial';
  await page.route('**/graphql', async route => {
    const { query } = route.request().postDataJSON();
    if (query.startsWith('query SelectedRepositories')) {
      if (revalidation === 'list-error') return route.fulfill({ status: 503 });
      const nodes = [selectedRepo('1'), selectedRepo('2')];
      return route.fulfill({ json: { data: { selectedRepositories: { nodes, withheld: 0, hasNextPage: false } } } });
    }
    const data = { a0: revalidation === 'initial' ? { activity: quietActivity } : null, r0: issuePage('1'), a1: { activity: quietActivity }, r1: issuePage('2') };
    const errors = revalidation === 'unverified' ? [{ path: ['a0'], extensions: { code: 'ACCESS_UNVERIFIED' } }] : undefined;
    return route.fulfill({ json: { data, errors } });
  });
  await page.goto('/');
  await expect(page.getByRole('listitem').filter({ hasText: /^Issue from repository 1 \(open\)$/ })).toBeVisible();
  revalidation = 'unverified';
  await page.getByRole('button', { name: 'Refresh repositories' }).click();
  await expect(page.getByText('Access unverified', { exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'ada/repo-1' })).toHaveCount(0);
  await expect(page.getByRole('listitem').filter({ hasText: /^Issue from repository 1 \(open\)$/ })).toHaveCount(0);
  await expect(page.getByRole('listitem').filter({ hasText: /^Issue from repository 2 \(open\)$/ })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Retry details' })).toHaveCount(0);

  revalidation = 'list-error';
  await page.getByRole('button', { name: 'Refresh repositories' }).click();
  await expect(page.getByRole('alert')).toContainText('Could not load repositories.');
  await expect(page.getByRole('listitem').filter({ hasText: /^Issue from repository 2 \(open\)$/ })).toHaveCount(0);
});

for (const action of ['navigating away', 'signing out'] as const) {
  test(`${action} cancels a delayed retry without restoring private details`, async ({ page }) => {
    let session = signedIn;
    await page.route('**/auth/me', route => route.fulfill({ json: session }));
    await page.route('**/auth/logout', route => {
      session = { mode: 'real', user: null, csrfToken: null };
      return route.fulfill({ status: 204 });
    });
    await mockOwned(page, []);
    let details = 0;
    let releaseRetry!: () => void;
    const heldRetry = new Promise<void>(resolve => { releaseRetry = resolve; });
    await page.route('**/graphql', async route => {
      const { query } = route.request().postDataJSON();
      if (query.startsWith('query SelectedRepositories')) {
        return route.fulfill({ json: { data: { selectedRepositories: {
          nodes: [selectedRepo('1')], withheld: 0, hasNextPage: false,
        } } } });
      }
      if (++details === 1) return route.fulfill({ status: 502 });
      if (details === 2) {
        await heldRetry;
        return route.fulfill({ json: { data: { a0: { activity: quietActivity }, r0: issuePage('1') } } }).catch(() => {});
      }
      return route.fulfill({ json: {
        data: { a0: null, r0: issuePage('1') },
        errors: [{ path: ['a0'], extensions: { code: 'ACCESS_UNVERIFIED' } }],
      } });
    });

    await page.goto('/');
    const retry = page.getByRole('button', { name: 'Retry details' });
    await retry.click();
    await expect.poll(() => details).toBe(2);
    const canceled = page.waitForEvent('requestfailed', {
      predicate: request => request.url().endsWith('/graphql') && request.failure()?.errorText === 'net::ERR_ABORTED',
      timeout: 4000,
    });
    try {
      if (action === 'signing out') {
        await page.getByRole('button', { name: 'Account menu' }).click();
        await page.getByRole('button', { name: 'Sign out' }).click();
        await canceled;
        await expect(page.getByRole('link', { name: 'Continue with GitHub' })).toBeVisible();
        await page.goto('/');
        await expect(page.getByRole('link', { name: 'Continue with GitHub' })).toBeVisible();
      } else {
        await page.getByRole('link', { name: 'Repositories', exact: true }).click();
        await canceled;
        await page.getByRole('link', { name: 'Overview', exact: true }).click();
        await expect(page.getByText('Access unverified', { exact: true })).toBeVisible();
      }
    } finally {
      releaseRetry();
    }
    await expect(page.getByText('Issue from repository 1', { exact: false })).toHaveCount(0);
  });
}
