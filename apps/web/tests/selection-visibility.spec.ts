import { expect, test } from '@playwright/test';
import { mockSession, quietActivity, selectedRepo, signedIn } from './mocks';

const covered = { covered: true, reason: null, through: '2026-11-17T08:00:00Z' };
const stream = { phase: 'current', boundary: '2026-08-01T00:00:00Z', catchupThrough: null, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, jobState: 'queued', nextAttemptAt: '2026-11-17T09:00:00Z' };
const child = { complete: true, pendingChildren: 0, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, nextAttemptAt: null };
const fullActivity = { synchronized: true, message: '', archived: false, issues: stream, pullRequests: stream, releases: stream, reviews: child, comments: child, issueEvents: { traversal: stream, historicalGap: false }, futureResources: [] };

const repoOverview = (id: string) => ({
  fullName: `ada/repo-${id}`,
  syncState: 'awaiting-sync',
  activity: fullActivity,
  openIssues: { value: 4, coverage: covered },
  staleOpenIssues: { value: 1, coverage: covered },
  pullRequestsMerged: { value: 2, coverage: covered },
  openPullRequests: { value: 1, coverage: covered },
  firstResponseTime: { seconds: 1800, sampleSize: 2, coverage: covered },
  mergeTime: { seconds: 3600, sampleSize: 2, coverage: covered },
  issuesWithoutResponse: { value: 0 },
  eventsCoveredFrom: '2026-11-03T00:00:00Z',
  windowStart: '2026-10-19T00:00:00Z',
  activeBotContributors: { value: 0, coverage: covered },
  contributorsCoverage: covered,
  contributors: [],
  timeline: [],
  daily: [],
});

const issuePage = (id: string) => ({
  nodes: [{ id, repositoryId: id, title: `Issue ${id}`, state: 'open' }],
  hasNextPage: false,
  withheld: 0,
  retryAt: null,
});

const sidebar = (page: import('@playwright/test').Page) => page.getByRole('navigation', { name: 'Primary' });

test('per-alias denial removes A snapshots and drafts while temporary uncertainty preserves B drafts and frozen snapshot', async ({ page }) => {
  await mockSession(page, signedIn);
  const retryAt = new Date(Date.now() + 600_000).toISOString();
  let detailsPhase: 'ok' | 'split' = 'ok';
  let overviewPhase: 'ok' | 'split' | 'outage' = 'ok';

  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      return route.fulfill({
        json: {
          data: {
            selectedRepositories: {
              nodes: [selectedRepo('1'), selectedRepo('2')],
              endCursor: '2',
              hasNextPage: false,
              withheld: 0,
              retryAt: null,
            },
          },
        },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      if (detailsPhase === 'ok') {
        return route.fulfill({
          json: { data: { a0: { activity: quietActivity }, r0: issuePage('1'), a1: { activity: quietActivity }, r1: issuePage('2') } },
        });
      }
      return route.fulfill({
        json: {
          data: { a0: null, r0: issuePage('1'), a1: null, r1: { nodes: [], hasNextPage: false, withheld: 1, retryAt } },
          errors: [{ message: 'Rate limited', path: ['a1'], extensions: { code: 'ACCESS_UNVERIFIED', retryAt } }],
        },
      });
    }
    if (String(query).startsWith('query RepositoryOverview')) {
      if (overviewPhase === 'outage') {
        return route.fulfill({ status: 503, body: 'Service Unavailable' });
      }
      if (overviewPhase === 'ok') {
        return route.fulfill({ json: { data: { repositoryOverview: repoOverview(variables.id) } } });
      }
      if (variables.id === '1') {
        return route.fulfill({ json: { data: { repositoryOverview: null } } });
      }
      return route.fulfill({
        json: {
          data: { repositoryOverview: null },
          errors: [{ message: 'Rate limited', path: ['repositoryOverview'], extensions: { code: 'ACCESS_UNVERIFIED', retryAt } }],
        },
      });
    }
    if (String(query).startsWith('query CrossRepositoryOverview')) {
      return route.fulfill({
        json: {
          data: {
            crossRepositoryOverview: {
              days: 30, windowStart: '2026-10-19T00:00:00Z', now: '2026-11-17T08:00:00Z',
              repositoriesCounted: 2, repositoriesSyncing: 0, withheld: 0, retryAt: null,
              openIssues: { value: 8, coverage: covered },
              reviewBacklog: { value: 1, coverage: covered },
              needsAttention: [], assignedToMe: [], daily: [],
            },
          },
        },
      });
    }
    return route.fallback();
  });

  await page.goto('/workspace/1?days=30');
  await expect(page.getByRole('heading', { name: 'repo-1', exact: true })).toBeVisible();
  await page.getByRole('link', { name: '7d', exact: true }).click();
  await expect(page.getByRole('link', { name: '7d', exact: true })).toHaveAttribute('aria-current', 'page');
  await sidebar(page).getByRole('link', { name: 'repo-2', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'repo-2', exact: true })).toBeVisible();

  await page.evaluate(() => {
    localStorage.setItem('repoorbit:issue-draft:real:ada:1:create', JSON.stringify({ version: 1, title: 'Secret A draft' }));
    localStorage.setItem('repoorbit:issue-draft:real:ada:2:create', JSON.stringify({ version: 1, title: 'Keep B draft' }));
  });

  detailsPhase = 'split';
  overviewPhase = 'split';
  await sidebar(page).getByRole('link', { name: 'Overview', exact: true }).click();

  await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter(k => k.startsWith('repoorbit:issue-draft:real:ada:')))).toEqual([
    'repoorbit:issue-draft:real:ada:2:create',
  ]);
  await expect(sidebar(page).getByRole('link', { name: 'repo-1', exact: true })).toHaveCount(0);
  await expect(sidebar(page).locator('a[href*="/workspace/1"]')).toHaveCount(0);
  await expect(sidebar(page).getByRole('link', { name: 'Repository', exact: true })).toHaveAttribute('href', /\/workspace\/2/);

  await sidebar(page).getByRole('link', { name: 'Repository', exact: true }).click();
  await expect(page.getByText(/Showing saved data from/)).toBeVisible();
  await expect(page.getByRole('heading', { name: 'repo-2', exact: true })).toBeVisible();

  overviewPhase = 'outage';
  await page.evaluate(() => {
    window.history.pushState({}, '', '/workspace/1?days=7');
    window.dispatchEvent(new PopStateEvent('popstate'));
  });
  await expect(page.getByRole('heading', { name: 'Repository', exact: true })).toBeVisible();
  await expect(page.getByText(/Showing saved data from/)).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'repo-1', exact: true })).toHaveCount(0);

  await page.evaluate(() => {
    window.history.pushState({}, '', '/workspace/1?days=30');
    window.dispatchEvent(new PopStateEvent('popstate'));
  });
  await expect(page.getByRole('heading', { name: 'Repository', exact: true })).toBeVisible();
  await expect(page.getByText(/Showing saved data from/)).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'repo-1', exact: true })).toHaveCount(0);
});

test('delayed manual detail retry completing after a workspace rejection cannot restore private data', async ({ page }) => {
  await mockSession(page, signedIn);
  const repositories = Array.from({ length: 10 }, (_, i) => selectedRepo(String(i + 1)));
  let failChunk2 = true;
  let denyRepo9Overview = false;
  let releaseDelayedRetry!: () => void;
  const delayedRetry = new Promise<void>(resolve => { releaseDelayedRetry = resolve; });
  let chunk2Calls = 0;
  let retryFulfilled = false;

  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      return route.fulfill({
        json: { data: { selectedRepositories: { nodes: repositories, endCursor: '10', hasNextPage: false, withheld: 0, retryAt: null } } },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      const ids = Object.values(variables as Record<string, string>);
      if (ids[0] === '9') {
        chunk2Calls++;
        if (failChunk2) {
          failChunk2 = false;
          return route.fulfill({ status: 502 });
        }
        await delayedRetry;
        retryFulfilled = true;
      }
      const data = Object.fromEntries(ids.flatMap((id, i) => [[`a${i}`, { activity: quietActivity }], [`r${i}`, issuePage(id)]]));
      return route.fulfill({ json: { data } });
    }
    if (String(query).startsWith('query RepositoryOverview')) {
      if (variables.id === '9' && denyRepo9Overview) {
        return route.fulfill({ json: { data: { repositoryOverview: null } } });
      }
      return route.fulfill({ json: { data: { repositoryOverview: repoOverview(variables.id) } } });
    }
    return route.fallback();
  });

  await page.goto('/workspace/9');
  await expect(page.getByRole('heading', { name: 'repo-9', exact: true })).toBeVisible();
  const retryBtn = sidebar(page).getByRole('button', { name: 'Retry details' });
  await expect(retryBtn).toBeVisible();

  await retryBtn.click();
  await expect.poll(() => chunk2Calls).toBe(2);

  denyRepo9Overview = true;
  await page.evaluate(() => {
    window.dispatchEvent(new Event('offline'));
    window.dispatchEvent(new Event('online'));
  });
  await expect(page.getByRole('heading', { name: 'Repository', exact: true })).toBeVisible();
  await expect(sidebar(page).getByRole('link', { name: 'repo-9', exact: true })).toHaveCount(0);

  releaseDelayedRetry();
  await expect.poll(() => retryFulfilled).toBe(true);
  await expect(sidebar(page).getByRole('link', { name: 'repo-10', exact: true })).toBeVisible();
  await expect(sidebar(page).getByRole('link', { name: 'repo-9', exact: true })).toHaveCount(0);
  await expect(page.locator('#repolist option[value="ada/repo-9"]')).toHaveCount(0);
});

test('aggregate overview hides rejected repository rows and totals immediately while preserving authorized and unloaded-page rows', async ({ page }) => {
  await mockSession(page, signedIn);
  let detailMode: 'ok' | 'reject-1' = 'ok';
  let releaseCrossRefresh!: () => void;
  const crossRefresh = new Promise<void>(resolve => { releaseCrossRefresh = resolve; });
  let crossCalls = 0;

  const aggregatePayload = {
    days: 30,
    windowStart: '2026-10-19T00:00:00Z',
    now: '2026-11-17T08:00:00Z',
    repositoriesCounted: 3,
    repositoriesSyncing: 0,
    withheld: 0,
    retryAt: null,
    openIssues: { value: 9, coverage: covered },
    reviewBacklog: { value: 4, coverage: covered },
    needsAttention: [
      { kind: 'issue_no_response', repositoryId: '1', fullName: 'ada/repo-1', number: 11, title: 'Secret issue 1', url: 'https://github.com/ada/repo-1/issues/11', since: '2026-11-15T00:00:00Z' },
      { kind: 'issue_no_response', repositoryId: '2', fullName: 'ada/repo-2', number: 22, title: 'Visible issue 2', url: 'https://github.com/ada/repo-2/issues/22', since: '2026-11-15T00:00:00Z' },
      { kind: 'stale_issue', repositoryId: '25', fullName: 'ada/repo-25', number: 250, title: 'Unloaded page issue 25', url: 'https://github.com/ada/repo-25/issues/250', since: '2026-10-01T00:00:00Z' },
    ],
    assignedToMe: [
      { repositoryId: '1', fullName: 'ada/repo-1', number: 11, title: 'Secret assigned 1', url: 'https://github.com/ada/repo-1/issues/11', updatedAt: '2026-11-15T00:00:00Z', status: 'todo' },
      { repositoryId: '25', fullName: 'ada/repo-25', number: 251, title: 'Unloaded assigned 25', url: 'https://github.com/ada/repo-25/issues/251', updatedAt: '2026-11-15T00:00:00Z', status: 'in_progress' },
    ],
    daily: [{ day: '2026-11-17T00:00:00Z', issuesOpened: 3, pullRequestsMerged: 2 }],
  };

  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      return route.fulfill({
        json: {
          data: {
            selectedRepositories: {
              nodes: [selectedRepo('1'), selectedRepo('2')],
              endCursor: '2',
              hasNextPage: true,
              withheld: 0,
              retryAt: null,
            },
          },
        },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      if (detailMode === 'reject-1') {
        return route.fulfill({
          json: {
            data: { a0: null, r0: issuePage('1'), a1: { activity: quietActivity }, r1: issuePage('2') },
          },
        });
      }
      return route.fulfill({
        json: { data: { a0: { activity: quietActivity }, r0: issuePage('1'), a1: { activity: quietActivity }, r1: issuePage('2') } },
      });
    }
    if (String(query).startsWith('query CrossRepositoryOverview')) {
      crossCalls++;
      if (crossCalls >= 2) {
        await crossRefresh;
      }
      return route.fulfill({ json: { data: { crossRepositoryOverview: aggregatePayload } } }).catch(() => {});
    }
    if (String(query).startsWith('query RepositoryOverview')) {
      return route.fulfill({ json: { data: { repositoryOverview: variables.id === '1' ? null : repoOverview(variables.id) } } });
    }
    return route.fallback();
  });

  try {
    await page.goto('/');
    const attn = page.getByRole('region', { name: 'Needs attention' });
    const mine = page.getByRole('region', { name: 'Assigned to me' });
    const totals = page.getByRole('region', { name: 'Totals' });

    await expect(attn).toContainText('Secret issue 1');
    await expect(attn).toContainText('Visible issue 2');
    await expect(attn).toContainText('Unloaded page issue 25');
    await expect(mine).toContainText('Secret assigned 1');
    await expect(mine).toContainText('Unloaded assigned 25');
    await expect(totals).toContainText('9');

    detailMode = 'reject-1';
    await page.evaluate(() => {
      window.dispatchEvent(new Event('offline'));
      window.dispatchEvent(new Event('online'));
    });

    await expect(attn).not.toContainText('Secret issue 1');
    await expect(mine).not.toContainText('Secret assigned 1');
    await expect(sidebar(page).getByRole('link', { name: 'repo-1', exact: true })).toHaveCount(0);
    await expect(attn).toContainText('Visible issue 2');
    await expect(attn).toContainText('Unloaded page issue 25');
    await expect(mine).toContainText('Unloaded assigned 25');
    await expect(totals).not.toContainText('9');
    await expect(totals).toContainText('—');
  } finally {
    releaseCrossRefresh();
  }
});

test('all-withheld metadata page still advances its cursor to load the next page', async ({ page }) => {
  await mockSession(page, signedIn);
  const cursors: (string | null)[] = [];
  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      cursors.push(variables.after ?? null);
      if (!variables.after) {
        return route.fulfill({
          json: {
            data: {
              selectedRepositories: { nodes: [], endCursor: '10', hasNextPage: true, withheld: 2, retryAt: null },
            },
          },
        });
      }
      return route.fulfill({
        json: {
          data: {
            selectedRepositories: { nodes: [selectedRepo('11')], endCursor: '11', hasNextPage: false, withheld: 0, retryAt: null },
          },
        },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      return route.fulfill({ json: { data: { a0: { activity: quietActivity }, r0: issuePage('11') } } });
    }
    if (String(query).startsWith('query CrossRepositoryOverview')) {
      return route.fulfill({
        json: {
          data: {
            crossRepositoryOverview: {
              days: 30, windowStart: '2026-10-19T00:00:00Z', now: '2026-11-17T08:00:00Z',
              repositoriesCounted: 0, repositoriesSyncing: 0, withheld: 2, retryAt: null,
              openIssues: { value: null, coverage: { covered: false, reason: 'not_synchronized', through: null } },
              reviewBacklog: { value: null, coverage: { covered: false, reason: 'not_synchronized', through: null } },
              needsAttention: [], assignedToMe: [], daily: [],
            },
          },
        },
      });
    }
    return route.fallback();
  });

  await page.goto('/');
  const loadMore = sidebar(page).getByRole('button', { name: 'Load more selected repositories' });
  await expect(loadMore).toBeVisible();
  await loadMore.click();
  await expect(sidebar(page).getByRole('link', { name: 'repo-11', exact: true })).toBeVisible();
  expect(cursors).toContain('10');
});

test('a rate-limited detail check that started before a workspace denial cannot list the denied repository again', async ({ page }) => {
  await mockSession(page, signedIn);
  const retryAt = new Date(Date.now() + 600_000).toISOString();
  let releaseDetails!: () => void;
  const details = new Promise<void>(resolve => { releaseDetails = resolve; });

  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      return route.fulfill({
        json: { data: { selectedRepositories: { nodes: [selectedRepo('1'), selectedRepo('2')], endCursor: '2', hasNextPage: false, withheld: 0, retryAt: null } } },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      await details;
      return route.fulfill({
        json: {
          data: { a0: null, r0: { nodes: [], hasNextPage: false, withheld: 1, retryAt }, a1: { activity: fullActivity }, r1: issuePage('2') },
          errors: [{ message: 'Rate limited', path: ['a0'], extensions: { code: 'ACCESS_UNVERIFIED', retryAt } }],
        },
      });
    }
    if (String(query).startsWith('query RepositoryOverview')) {
      return route.fulfill({ json: { data: { repositoryOverview: variables.id === '1' ? null : repoOverview(variables.id) } } });
    }
    return route.fallback();
  });

  await page.goto('/workspace/1');
  await expect(page.getByRole('heading', { name: 'Repository', exact: true })).toBeVisible();
  releaseDetails();
  await expect(sidebar(page).locator('a[href*="/workspace/2"]')).toHaveAttribute('title', 'repo-2 · Synced');
  await expect(sidebar(page).locator('a[href*="/workspace/1"]')).toHaveCount(0);
  await expect(sidebar(page).getByText('Waiting for GitHub')).toHaveCount(0);
});

test('a detail retry that denies a repository on Overview refetches the aggregate instead of leaving totals blank', async ({ page }) => {
  await mockSession(page, signedIn);
  let detailCalls = 0;
  let crossCalls = 0;
  const aggregate = (openIssues: number) => ({
    days: 30, windowStart: '2026-10-19T00:00:00Z', now: '2026-11-17T08:00:00Z',
    repositoriesCounted: 2, repositoriesSyncing: 0, withheld: 0, retryAt: null,
    openIssues: { value: openIssues, coverage: covered }, reviewBacklog: { value: 1, coverage: covered },
    needsAttention: [], assignedToMe: [], daily: [{ day: '2026-11-17T00:00:00Z', issuesOpened: 1, pullRequestsMerged: 1 }],
  });

  await page.route('**/graphql', async route => {
    const { query } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      return route.fulfill({
        json: { data: { selectedRepositories: { nodes: [selectedRepo('1'), selectedRepo('2')], endCursor: '2', hasNextPage: false, withheld: 0, retryAt: null } } },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      if (++detailCalls === 1) return route.fulfill({ status: 502 });
      return route.fulfill({ json: { data: { a0: null, r0: issuePage('1'), a1: { activity: quietActivity }, r1: issuePage('2') } } });
    }
    if (String(query).startsWith('query CrossRepositoryOverview')) {
      return route.fulfill({ json: { data: { crossRepositoryOverview: aggregate(++crossCalls === 1 ? 41 : 37) } } });
    }
    return route.fallback();
  });

  await page.goto('/');
  const totals = page.getByRole('region', { name: 'Totals' });
  await expect(totals).toContainText('41');
  await sidebar(page).getByRole('button', { name: 'Retry details' }).click();
  await expect(sidebar(page).getByRole('link', { name: 'repo-1', exact: true })).toHaveCount(0);
  await expect(totals).toContainText('37');
});

test('a rate-limited repository missing from a complete selection refresh loses its saved data link and stops the wait', async ({ page }) => {
  await mockSession(page, signedIn);
  const retryAt = new Date(Date.now() + 600_000).toISOString();
  let phase: 'ok' | 'limited' | 'dropped' = 'ok';

  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      const nodes = phase === 'dropped' ? [selectedRepo('1')] : [selectedRepo('1'), selectedRepo('2')];
      return route.fulfill({ json: { data: { selectedRepositories: { nodes, endCursor: String(nodes.length), hasNextPage: false, withheld: 0, retryAt: null } } } });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      const ids = Object.values(variables as Record<string, string>);
      const data: Record<string, unknown> = {};
      const errors: unknown[] = [];
      ids.forEach((id, i) => {
        if (id === '2' && phase === 'limited') {
          data[`a${i}`] = null;
          data[`r${i}`] = { nodes: [], hasNextPage: false, withheld: 1, retryAt };
          errors.push({ message: 'Rate limited', path: [`a${i}`], extensions: { code: 'ACCESS_UNVERIFIED', retryAt } });
        } else {
          data[`a${i}`] = { activity: quietActivity };
          data[`r${i}`] = issuePage(id);
        }
      });
      return route.fulfill({ json: errors.length > 0 ? { data, errors } : { data } });
    }
    if (String(query).startsWith('query RepositoryOverview')) {
      return route.fulfill({ json: { data: { repositoryOverview: repoOverview(variables.id) } } });
    }
    if (String(query).startsWith('query CrossRepositoryOverview')) {
      return route.fulfill({
        json: {
          data: {
            crossRepositoryOverview: {
              days: 30, windowStart: '2026-10-19T00:00:00Z', now: '2026-11-17T08:00:00Z',
              repositoriesCounted: 1, repositoriesSyncing: 0, withheld: 0, retryAt: null,
              openIssues: { value: 3, coverage: covered }, reviewBacklog: { value: 1, coverage: covered },
              needsAttention: [], assignedToMe: [], daily: [],
            },
          },
        },
      });
    }
    return route.fallback();
  });
  const reconnect = () => page.evaluate(() => {
    window.dispatchEvent(new Event('offline'));
    window.dispatchEvent(new Event('online'));
  });

  await page.goto('/workspace/2?days=30');
  await expect(page.getByRole('heading', { name: 'repo-2', exact: true })).toBeVisible();
  await sidebar(page).getByRole('link', { name: 'Overview', exact: true }).click();

  phase = 'limited';
  await reconnect();
  await expect(sidebar(page).getByText('Waiting for GitHub')).toBeVisible();
  await expect(sidebar(page).getByRole('link', { name: 'Repository', exact: true })).toHaveAttribute('href', /\/workspace\/2/);

  phase = 'dropped';
  await reconnect();
  await expect(sidebar(page).getByRole('link', { name: 'Repository', exact: true })).toHaveCount(0);
  await expect(sidebar(page).locator('a[href*="/workspace/2"]')).toHaveCount(0);
  await expect(sidebar(page).getByText('Waiting for GitHub')).toHaveCount(0);
});

test('navigating away during a manual detail retry cancels it without reporting a failed check', async ({ page }) => {
  await mockSession(page, signedIn);
  let detailCalls = 0;
  let releaseRetry!: () => void;
  const retry = new Promise<void>(resolve => { releaseRetry = resolve; });

  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      return route.fulfill({
        json: { data: { selectedRepositories: { nodes: [selectedRepo('1'), selectedRepo('2')], endCursor: '2', hasNextPage: false, withheld: 0, retryAt: null } } },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      if (++detailCalls === 1) return route.fulfill({ status: 502 });
      await retry;
      return route.fulfill({ json: { data: { a0: { activity: quietActivity }, r0: issuePage('1'), a1: { activity: quietActivity }, r1: issuePage('2') } } }).catch(() => {});
    }
    if (String(query).startsWith('query RepositoryOverview')) {
      return route.fulfill({ json: { data: { repositoryOverview: repoOverview(variables.id) } } });
    }
    return route.fallback();
  });

  try {
    await page.goto('/workspace/1');
    const retryBtn = sidebar(page).getByRole('button', { name: 'Retry details' });
    await retryBtn.click();
    await expect.poll(() => detailCalls).toBe(2);
    await sidebar(page).getByRole('link', { name: 'repo-2', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'repo-2', exact: true })).toBeVisible();
    await expect(retryBtn).toBeEnabled();
    await expect(sidebar(page).getByRole('alert')).toHaveCount(0);
  } finally {
    releaseRetry();
  }
});

test('an error-free null repository alias is a denial even when its issues alias is rate-limited', async ({ page }) => {
  await mockSession(page, signedIn);
  const retryAt = new Date(Date.now() + 600_000).toISOString();
  let mixed = false;

  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      return route.fulfill({
        json: { data: { selectedRepositories: { nodes: [selectedRepo('1'), selectedRepo('2')], endCursor: '2', hasNextPage: false, withheld: 0, retryAt: null } } },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      if (!mixed) {
        return route.fulfill({ json: { data: { a0: { activity: quietActivity }, r0: issuePage('1'), a1: { activity: quietActivity }, r1: issuePage('2') } } });
      }
      return route.fulfill({
        json: {
          data: { a0: null, r0: { nodes: [], hasNextPage: false, withheld: 1, retryAt }, a1: { activity: quietActivity }, r1: issuePage('2') },
          errors: [{ message: 'Rate limited', path: ['r0'], extensions: { code: 'ACCESS_UNVERIFIED', retryAt } }],
        },
      });
    }
    if (String(query).startsWith('query RepositoryOverview')) {
      return route.fulfill({ json: { data: { repositoryOverview: repoOverview(variables.id) } } });
    }
    return route.fallback();
  });

  await page.goto('/workspace/2');
  await expect(page.getByRole('heading', { name: 'repo-2', exact: true })).toBeVisible();
  await page.evaluate(() => localStorage.setItem('repoorbit:issue-draft:real:ada:1:create', JSON.stringify({ version: 1, title: 'Private draft' })));

  mixed = true;
  await page.evaluate(() => {
    window.dispatchEvent(new Event('offline'));
    window.dispatchEvent(new Event('online'));
  });
  await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter(k => k.startsWith('repoorbit:issue-draft:real:ada:1:')))).toEqual([]);
  await expect(sidebar(page).locator('a[href*="/workspace/1"]')).toHaveCount(0);
});

test('an open workspace stops showing a verified repository once a complete selection refresh leaves it out', async ({ page }) => {
  await mockSession(page, signedIn);
  let dropped = false;
  let releaseOverview!: () => void;
  const heldOverview = new Promise<void>(resolve => { releaseOverview = resolve; });

  await page.route('**/graphql', async route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      const nodes = dropped ? [selectedRepo('1')] : [selectedRepo('1'), selectedRepo('2')];
      return route.fulfill({ json: { data: { selectedRepositories: { nodes, endCursor: String(nodes.length), hasNextPage: false, withheld: 0, retryAt: null } } } });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      const ids = Object.values(variables as Record<string, string>);
      return route.fulfill({ json: { data: Object.fromEntries(ids.flatMap((id, i) => [[`a${i}`, { activity: quietActivity }], [`r${i}`, issuePage(id)]])) } });
    }
    if (String(query).startsWith('query RepositoryOverview')) {
      if (dropped) await heldOverview; // an overview read that has not answered yet
      return route.fulfill({ json: { data: { repositoryOverview: repoOverview(variables.id) } } }).catch(() => {});
    }
    return route.fallback();
  });

  try {
    await page.goto('/workspace/2');
    await expect(page.getByRole('heading', { name: 'repo-2', exact: true })).toBeVisible();
    dropped = true;
    await page.evaluate(() => {
      window.dispatchEvent(new Event('offline'));
      window.dispatchEvent(new Event('online'));
    });
    await expect(sidebar(page).locator('a[href*="/workspace/2"]')).toHaveCount(0);
    await expect(page.getByRole('heading', { name: 'repo-2', exact: true })).toHaveCount(0);
    releaseOverview();
    await expect(page.getByRole('heading', { name: 'repo-2', exact: true })).toHaveCount(0);
  } finally {
    releaseOverview();
  }
});

test('a denial recorded while the first Overview aggregate is still loading starts a fresh aggregate read', async ({ page }) => {
  await mockSession(page, signedIn);
  let crossCalls = 0;
  let markStarted!: () => void;
  const firstStarted = new Promise<void>(resolve => { markStarted = resolve; });
  let releaseFirst!: () => void;
  const firstAggregate = new Promise<void>(resolve => { releaseFirst = resolve; });
  const aggregate = (openIssues: number) => ({
    days: 30, windowStart: '2026-10-19T00:00:00Z', now: '2026-11-17T08:00:00Z',
    repositoriesCounted: 1, repositoriesSyncing: 0, withheld: 0, retryAt: null,
    openIssues: { value: openIssues, coverage: covered }, reviewBacklog: { value: 1, coverage: covered },
    needsAttention: [], assignedToMe: [], daily: [],
  });

  await page.route('**/graphql', async route => {
    const { query } = route.request().postDataJSON();
    if (String(query).startsWith('query SelectedRepositories')) {
      return route.fulfill({
        json: { data: { selectedRepositories: { nodes: [selectedRepo('1'), selectedRepo('2')], endCursor: '2', hasNextPage: false, withheld: 0, retryAt: null } } },
      });
    }
    if (String(query).startsWith('query RepositoryDetails')) {
      await firstStarted;
      return route.fulfill({ json: { data: { a0: null, r0: issuePage('1'), a1: { activity: fullActivity }, r1: issuePage('2') } } });
    }
    if (String(query).startsWith('query CrossRepositoryOverview')) {
      const call = ++crossCalls;
      if (call === 1) {
        markStarted();
        await firstAggregate;
      }
      return route.fulfill({ json: { data: { crossRepositoryOverview: aggregate(call === 1 ? 41 : 37) } } }).catch(() => {});
    }
    return route.fallback();
  });

  try {
    await page.goto('/');
    await expect(sidebar(page).locator('a[href*="/workspace/2"]')).toHaveAttribute('title', 'repo-2 · Synced');
    await expect(sidebar(page).locator('a[href*="/workspace/1"]')).toHaveCount(0);
    await expect(page.getByRole('region', { name: 'Totals' })).toContainText('37');
  } finally {
    releaseFirst();
  }
});
