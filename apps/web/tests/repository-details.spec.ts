import { test, expect, type Page, type Route } from '@playwright/test';
import { mockOwned, mockSession, quietActivity, selectedRepo, signedIn } from './mocks';

// Per-repository details load in chunks through the shell's shared selection fetch (sidebar and workspace). Since U5d the
// detailed status lives in the workspace header (/workspace/<id>), not on /.
const issuePage = (id: string) => ({
  nodes: [{ id, repositoryId: id, title: `Issue from repository ${id}`, state: 'open' }],
  hasNextPage: false, withheld: 0, retryAt: null,
});
const unknown = { value: null, coverage: { covered: false, reason: 'not_synchronized', through: null } };
const repositoryOverview = (id: string) => ({
  fullName: `ada/repo-${id}`, syncState: 'awaiting-sync', activity: { ...quietActivity, archived: false },
  openIssues: unknown, staleOpenIssues: unknown, pullRequestsMerged: unknown,
  firstResponseTime: { seconds: null, sampleSize: null, coverage: unknown.coverage }, mergeTime: { seconds: null, sampleSize: null, coverage: unknown.coverage },
  issuesWithoutResponse: { value: null }, eventsCoveredFrom: null, windowStart: '2026-10-19T00:00:00Z',
  activeBotContributors: unknown, contributorsCoverage: unknown.coverage, contributors: [], timeline: [], daily: [],
});
// Answers the workspace (RepositoryOverview) and overview (CrossRepositoryOverview) reads; returns false for anything else.
function answerScreens(route: Route, query: string, variables: Record<string, string>) {
  if (query.startsWith('query RepositoryOverview')) return route.fulfill({ json: { data: { repositoryOverview: repositoryOverview(variables.id) } } }).then(() => true);
  if (query.startsWith('query CrossRepositoryOverview')) return route.fulfill({ json: { data: null, errors: [{ message: 'not under test' }] } }).then(() => true);
  return Promise.resolve(false);
}
const sidebar = (page: Page) => page.getByRole('navigation', { name: 'Primary' });

test('loads ordered chunks with two requests in flight, and a failed chunk does not block the others', async ({ page }) => {
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
    if (await answerScreens(route, query, variables)) return;
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

  await page.goto('/workspace/1');
  try {
    await expect.poll(() => inFlight).toBe(2);
  } finally {
    releaseInitial();
  }
  await expect(sidebar(page).getByRole('link', { name: /^repo-\d+$/ })).toHaveText(repositories.filter((_, i) => i < 8 || i >= 16).map(repo => repo.fullName.split('/')[1]));
  await expect(sidebar(page).getByRole('link', { name: 'repo-9', exact: true })).toHaveCount(0);
  await expect(page.locator('#repolist option[value="ada/repo-9"]')).toHaveCount(0);
  await expect.poll(() => calls.length).toBe(3);
  // Repository 9's chunk failed; its workspace still opens from its own analytics read.
  await expect(page.getByRole('heading', { name: 'repo-1', exact: true })).toBeVisible();
  expect(peak).toBe(2);
  expect(listCalls).toBe(1);
  expect(calls.map(ids => ids.length)).toEqual([8, 8, 4]);
  await sidebar(page).getByRole('button', { name: 'Retry details' }).click();
  await expect(sidebar(page).getByRole('link', { name: 'repo-9', exact: true })).toBeVisible();
  expect(calls.map(ids => ids[0])).toEqual(['1', '9', '17', '9']);
  expect(listCalls).toBe(1);
});

test('a failed access check clears previously loaded details and keeps the other repository', async ({ page }) => {
  await mockSession(page, signedIn);
  let revalidation: 'initial' | 'unverified' | 'list-error' = 'initial';
  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (await answerScreens(route, query, variables)) return;
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
  await expect(sidebar(page).getByRole('link', { name: 'repo-1', exact: true })).toBeVisible();
  // Opening another screen revalidates the selection (and its details).
  revalidation = 'unverified';
  await sidebar(page).getByRole('link', { name: 'repo-1', exact: true }).click();
  await expect(page.getByRole('main').getByText('Access unverified', { exact: true })).toBeVisible();
  await expect(page.getByRole('main').getByText('repo-1')).toHaveCount(0);
  await expect(sidebar(page).getByRole('link', { name: 'repo-1', exact: true })).toHaveCount(0);
  await expect(sidebar(page).getByRole('link', { name: 'repo-2', exact: true })).toBeVisible();

  revalidation = 'list-error';
  await sidebar(page).getByRole('link', { name: 'Overview', exact: true }).click();
  await expect(page.getByRole('alert')).toContainText('Could not load repositories.');
  await expect(sidebar(page).getByRole('link', { name: 'repo-2', exact: true })).toHaveCount(0);
});

test('switching repositories clears the previous workspace while its access check runs', async ({ page }) => {
  await mockSession(page, signedIn);
  let release!: () => void;
  const pending = new Promise<void>(resolve => { release = resolve; });
  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (query.startsWith('query SelectedRepositories')) {
      return route.fulfill({ json: { data: { selectedRepositories: {
        nodes: [selectedRepo('1'), selectedRepo('2')], withheld: 0, hasNextPage: false,
      } } } });
    }
    if (query.startsWith('query RepositoryOverview') && variables.id === '2') await pending;
    if (await answerScreens(route, query, variables)) return;
    return route.fulfill({ json: { data: { a0: { activity: quietActivity }, r0: issuePage('1'), a1: { activity: quietActivity }, r1: issuePage('2') } } });
  });
  await page.goto('/workspace/1');
  const heading = page.getByRole('main').getByRole('heading', { level: 1 });
  await expect(heading).toHaveText('repo-1');
  try {
    await sidebar(page).getByRole('link', { name: 'repo-2', exact: true }).click();
    await expect(page.getByText('Loading repository workspace…')).toBeVisible();
    await expect(heading).toHaveCount(0);
  } finally {
    release();
  }
  await expect(heading).toHaveText('repo-2');
});

for (const action of ['navigating away', 'signing out'] as const) {
  test(`${action} cancels a delayed read without restoring private details`, async ({ page }) => {
    let session = signedIn;
    await page.route('**/auth/me', route => route.fulfill({ json: session }));
    await page.route('**/auth/logout', route => {
      session = { mode: 'real', user: null, csrfToken: null };
      return route.fulfill({ status: 204 });
    });
    await mockOwned(page, []);
    let reads = 0;
    let releaseRead!: () => void;
    const heldRead = new Promise<void>(resolve => { releaseRead = resolve; });
    await page.route('**/graphql', async route => {
      const { query, variables } = route.request().postDataJSON();
      if (query.startsWith('query SelectedRepositories')) {
        return route.fulfill({ json: { data: { selectedRepositories: {
          nodes: [selectedRepo('1')], withheld: 0, hasNextPage: false,
        } } } });
      }
      if (query.startsWith('query RepositoryOverview')) {
        // The first workspace read is held; later reads report that access could not be verified.
        if (++reads === 1) {
          await heldRead;
          return route.fulfill({ json: { data: { repositoryOverview: repositoryOverview(variables.id) } } }).catch(() => {});
        }
        return route.fulfill({ json: { data: { repositoryOverview: null }, errors: [{ path: ['repositoryOverview'], extensions: { code: 'ACCESS_UNVERIFIED' } }] } });
      }
      if (await answerScreens(route, query, variables)) return;
      return route.fulfill({ json: { data: { a0: { activity: quietActivity }, r0: issuePage('1') } } });
    });

    await page.goto('/workspace/1');
    await expect.poll(() => reads).toBe(1);
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
        await page.goto('/workspace/1');
        await expect(page.getByRole('link', { name: 'Continue with GitHub' })).toBeVisible();
      } else {
        await page.getByRole('link', { name: 'Repositories', exact: true }).click();
        await canceled;
        await sidebar(page).getByRole('link', { name: 'repo-1', exact: true }).click();
        await expect(page.getByRole('main').getByText('Access unverified', { exact: true })).toBeVisible();
      }
    } finally {
      releaseRead();
    }
    await expect(page.getByRole('main').getByText('ada/repo-1')).toHaveCount(0);
    await expect(page.getByRole('heading', { name: 'repo-1', exact: true })).toHaveCount(0);
  });
}
