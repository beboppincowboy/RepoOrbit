import { expect, test, type Page, type Route } from '@playwright/test';
import { demo, mockSelected, mockSession, selectedRepo, signedIn } from './mocks';


const covered = { covered: true, reason: null, through: '2026-11-17T08:00:00Z' };
const stream = { phase: 'current', boundary: '2026-08-01T00:00:00Z', catchupThrough: null, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, jobState: 'queued', nextAttemptAt: '2026-11-17T09:00:00Z' };
const child = { complete: true, pendingChildren: 0, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, nextAttemptAt: null };
const overview = {
  fullName: 'ada/repo-1', syncState: 'awaiting-sync',
  activity: { synchronized: true, message: '', archived: false, issues: stream, pullRequests: stream, releases: stream, reviews: child, comments: child, issueEvents: { traversal: stream, historicalGap: false }, futureResources: [] },
  openIssues: { value: 5, coverage: covered }, staleOpenIssues: { value: 0, coverage: covered }, pullRequestsMerged: { value: 1, coverage: covered }, openPullRequests: { value: 1, coverage: covered },
  firstResponseTime: { seconds: 3600, sampleSize: 2, coverage: covered }, mergeTime: { seconds: 7200, sampleSize: 1, coverage: covered }, issuesWithoutResponse: { value: 0 },
  eventsCoveredFrom: '2026-11-03T00:00:00Z', windowStart: '2026-10-19T00:00:00Z', activeBotContributors: { value: 0, coverage: covered }, contributorsCoverage: covered,
  contributors: [{ login: 'alex', isBot: false, pullRequestsOpened: 3, reviewsSubmitted: 2, commentsCreated: 4 }],
  timeline: [], daily: [],
};

const issue = (number: number, title: string, over: Record<string, unknown> = {}) => ({
  number, title, url: `https://github.com/ada/repo-1/issues/${number}`, state: 'open', updatedAt: '2026-11-20T10:00:00Z',
  authorLogin: 'alex', labels: [] as string[], assignees: [] as string[], status: 'backlog', multipleStatus: false, ...over,
});
const boardIssues = [
  issue(1, 'Plan the roadmap'),
  issue(3, 'Write the importer', { labels: ['status:todo'], assignees: ['alex'], status: 'todo' }),
  issue(5, 'Ship the board', { labels: ['status:in-progress'], assignees: ['ada'], status: 'in_progress' }),
  issue(7, 'Review the API', { labels: ['status:todo', 'status:review'], assignees: ['alex'], status: 'in_review', multipleStatus: true }),
  issue(9, 'Old closed task', { state: 'closed', labels: ['status:todo', 'bug'], status: 'done' }),
  issue(12, 'Fix pagination edge case', { labels: ['bug'] }),
];

async function setup(page: Page, opts: { session?: typeof signedIn | typeof demo; statusLabels?: boolean; canManageIssues?: boolean } = {}) {
  const session = opts.session ?? signedIn;
  const labelsOn = opts.statusLabels ?? true;
  const issues = boardIssues.map(i => labelsOn ? { ...i } : { ...i, status: null, multipleStatus: false });
  const boardIncludes: number[][] = [];
  const posts: string[] = [];
  page.on('request', request => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.startsWith('/api/')) posts.push(request.url());
  });
  await mockSession(page, session);
  await mockSelected(page, [selectedRepo('1', { syncState: session.mode === 'demo' ? 'demo' : 'awaiting-sync' })]);
  await page.route('**/graphql', route => {
    const { query, variables } = route.request().postDataJSON();
    if (String(query).startsWith('query RepositoryOverview')) return route.fulfill({ json: { data: { repositoryOverview: overview } } });
    if (String(query).startsWith('query RepositoryBoard')) {
      boardIncludes.push(variables.include);
      return route.fulfill({ json: { data: { repositoryBoard: { statusLabels: labelsOn, truncated: false, issues } } } });
    }
    return route.fallback();
  });
  if (session.mode === 'real') {
    const canManageIssues = opts.canManageIssues ?? true;
    await page.route('**/api/repositories/1/issue-capabilities', route => route.fulfill({ json: { canCreateIssue: true, canCreateIssueWithMetadata: canManageIssues, canManageIssues } }));
    await page.route('**/api/repositories/1/assignees', route => route.fulfill({ json: { assignees: ['ada', 'alex', 'sam'] } }));
    await page.route('**/api/repositories/1/status-labels', route => route.fulfill({ json: { enabled: labelsOn, canChange: false } }));
  }
  return { boardIncludes, issues, posts };
}

const column = (page: Page, name: string) => page.getByRole('list', { name, exact: true });
const card = (page: Page, n: number) => page.locator(`[data-issue-card="${n}"]`);
const boardKeys = (page: Page) => page.evaluate(() => Object.keys(localStorage).filter(key => key.startsWith('repoorbit:issue-draft:') && key.includes(':board:')));
const storedCard = (page: Page, n: number) => page.evaluate(key => JSON.parse(localStorage.getItem(key) ?? 'null'), `repoorbit:issue-draft:real:ada:1:board:${n}`);

async function openDrawer(page: Page, n: number) {
  await card(page, n).getByRole('button').click();
  const drawer = page.getByRole('dialog', { name: `Issue #${n}` });
  await expect(drawer).toBeVisible();
  return drawer;
}

const saveButton = (page: Page) => page.getByRole('button', { name: 'Save…' });
const review = (page: Page) => page.getByRole('dialog', { name: 'Review staged changes' });

// Stages a status move through the drawer, as a user does, and closes the drawer again.
async function stageStatus(page: Page, n: number, status: string) {
  const drawer = await openDrawer(page, n);
  await drawer.getByRole('radio', { name: status, exact: true }).check();
  await expect(card(page, n)).toContainText('Staged');
  await page.keyboard.press('Escape');
  await expect(drawer).toHaveCount(0);
}

async function confirmSave(page: Page) {
  await saveButton(page).click();
  await review(page).getByRole('button', { name: 'Confirm' }).click();
}

const operation = (n: number, state: 'succeeded' | 'failed' | 'unknown', id = 600 + n) => ({
  id, repositoryId: 1, kind: 'update', state, issueNumber: n, draft: {},
  outcome: state === 'succeeded' ? { allStepsAnswered: true } : { error: 'GitHub did not apply every requested change.' },
  actor: 'ada', createdAt: '2026-11-25T12:00:00Z', updatedAt: '2026-11-25T12:00:01Z',
});

type SentUpdate = { number: number; key: string; body: Record<string, unknown> };

// Mocks the Board's update route, recording each request before answer replies to it.
async function mockUpdates(page: Page, answer: (route: Route, sent: SentUpdate) => Promise<void> = (route, sent) => route.fulfill({ status: 201, json: operation(sent.number, 'succeeded') })) {
  const sent: SentUpdate[] = [];
  await page.route('**/api/repositories/1/issues/*/updates', async route => {
    const request = route.request();
    const update = {
      number: Number(new URL(request.url()).pathname.split('/').at(-2)),
      key: request.headers()['idempotency-key'] ?? '',
      body: request.postDataJSON() as Record<string, unknown>,
    };
    sent.push(update);
    await answer(route, update);
  });
  return sent;
}

test.beforeEach(async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
});

test('places issues in the five status columns, keeps closed issues in Done and flags multiple status labels', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=board');
  const placement: [string, number[]][] = [['Backlog', [1, 12]], ['Todo', [3]], ['In Progress', [5]], ['In Review', [7]], ['Done', [9]]];
  for (const [name, numbers] of placement) {
    await expect(column(page, name).getByRole('listitem')).toHaveCount(numbers.length);
    for (const n of numbers) await expect(column(page, name).locator(`[data-issue-card="${n}"]`)).toBeVisible();
  }
  await expect(card(page, 7)).toContainText('Multiple status labels');
  await expect(card(page, 3)).not.toContainText('Multiple status labels');
  await expect(card(page, 3)).toContainText('alex');
  expect(await page.getByRole('main').evaluate(el => el.scrollHeight <= el.clientHeight)).toBe(true);
});

test('without status labels shows No status source, the setup panel and a read-only list without status', async ({ page }) => {
  await setup(page, { statusLabels: false });
  await page.goto('/workspace/1?tab=board');
  await expect(page.getByRole('heading', { name: 'No status source' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Status labels' })).toBeVisible();
  await expect(page.getByRole('table').locator('tbody tr')).toHaveCount(boardIssues.length);
  await expect(page.getByRole('columnheader', { name: 'Status' })).toHaveCount(0);
  await expect(column(page, 'Backlog')).toHaveCount(0);
  await expect(page.locator('[draggable="true"]')).toHaveCount(0);
});

test('assignee, label and text filters narrow the Board and List views identically', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=board');
  const shown = () => page.locator('main [data-issue-card]').evaluateAll(els => els.map(el => Number(el.getAttribute('data-issue-card'))).sort((a, b) => a - b));
  const both = async (expected: number[]) => {
    await page.getByRole('button', { name: 'Board', exact: true }).click();
    await expect.poll(shown).toEqual(expected);
    await page.getByRole('button', { name: 'List', exact: true }).click();
    await expect(page.getByRole('columnheader', { name: 'Status' })).toBeVisible();
    await expect.poll(shown).toEqual(expected);
  };
  await expect(card(page, 1)).toBeVisible();
  await page.getByLabel('Filter by assignee').selectOption('alex');
  await both([3, 7]);
  await page.getByLabel('Filter by assignee').selectOption('');
  await page.getByLabel('Filter by label').selectOption('bug');
  await both([9, 12]);
  await page.getByLabel('Filter by label').selectOption('');
  await page.getByLabel('Filter by text').fill('pagination');
  await both([12]);
});

test('dragging a card stages the move on the card and sends nothing', async ({ page }) => {
  const { posts } = await setup(page);
  await page.goto('/workspace/1?tab=board');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'true');
  await card(page, 3).dragTo(column(page, 'In Progress'), { steps: 15 });
  await expect(column(page, 'In Progress').locator('[data-issue-card="3"]')).toContainText('Staged · from Todo');
  await expect(column(page, 'Todo').locator('[data-issue-card="3"]')).toHaveCount(0);
  expect(await storedCard(page, 3)).toMatchObject({ phase: 'staged', target: { status: 'in_progress' }, snapshot: { status: 'todo', labels: ['status:todo'] }, request: null, operationId: null });
  await expect(page.getByText('1 staged change')).toBeVisible();
  expect(posts).toEqual([]);
});

test('the drawer status menu stages the same move from the keyboard alone and returns focus to the card', async ({ page }) => {
  const { posts } = await setup(page);
  await page.goto('/workspace/1?tab=board');
  await card(page, 3).getByRole('button').focus();
  await page.keyboard.press('Enter');
  const drawer = page.getByRole('dialog', { name: 'Issue #3' });
  await expect(drawer).toBeFocused();
  await expect(page).toHaveURL(/issue=3/);
  const todo = drawer.getByRole('radio', { name: 'Todo' });
  await expect(todo).toBeChecked();
  for (let i = 0; i < 10 && !(await todo.evaluate(el => el === document.activeElement)); i++) await page.keyboard.press('Tab');
  await page.keyboard.press('ArrowRight');
  await expect(drawer.getByRole('radio', { name: 'In Progress' })).toBeChecked();
  await expect(column(page, 'In Progress').locator('[data-issue-card="3"]')).toContainText('Staged · from Todo');
  await page.keyboard.press('Escape');
  await expect(drawer).toHaveCount(0);
  await expect(page).not.toHaveURL(/issue=/);
  await expect(card(page, 3).getByRole('button')).toBeFocused();
  expect(posts).toEqual([]);
});

test('a Read user cannot drag, sees disabled drawer controls with the reason, and stores nothing (AE6)', async ({ page }) => {
  const { posts } = await setup(page, { canManageIssues: false });
  await page.goto('/workspace/1?tab=board');
  const drawer = await openDrawer(page, 3);
  await expect(drawer).toContainText('requires Triage or higher permission on GitHub');
  await expect(drawer.getByRole('radio', { name: 'In Progress' })).toBeDisabled();
  await expect(drawer.getByRole('checkbox', { name: 'sam' })).toBeDisabled();
  await expect(drawer.getByRole('button', { name: 'Close issue' })).toBeDisabled();
  await page.keyboard.press('Escape');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'false');
  await card(page, 3).dragTo(column(page, 'In Progress'), { steps: 15 });
  await expect(column(page, 'Todo').locator('[data-issue-card="3"]')).toBeVisible();
  expect(await boardKeys(page)).toEqual([]);
  expect(posts).toEqual([]);
});

test('demo mode shows the Board with staging disabled and labeled', async ({ page }) => {
  await setup(page, { session: demo });
  await page.goto('/workspace/1?tab=board');
  await expect(column(page, 'In Review').locator('[data-issue-card="7"]')).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'Demo mode' })).toContainText('Board changes cannot be staged');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'false');
  const drawer = await openDrawer(page, 3);
  await expect(drawer).toContainText('Demo mode');
  await expect(drawer.getByRole('radio', { name: 'Done' })).toBeDisabled();
  await expect(drawer.getByRole('button', { name: 'Close issue' })).toBeDisabled();
});

test('assigning someone to a Backlog card moves it to Todo and names the side effect', async ({ page }) => {
  const { posts } = await setup(page);
  await page.goto('/workspace/1?tab=board');
  const drawer = await openDrawer(page, 12);
  await drawer.getByRole('checkbox', { name: 'alex' }).check();
  await expect(column(page, 'Todo').locator('[data-issue-card="12"]')).toContainText('Staged · from Backlog');
  await expect(drawer.getByRole('radio', { name: 'Todo' })).toBeChecked();
  await expect(drawer).toContainText('Moves to Todo because an assignee was added');
  expect(await storedCard(page, 12)).toMatchObject({ target: { status: 'todo', assigneesAdd: ['alex'], assigneesRemove: [], assignedFromBacklog: true } });
  expect(posts).toEqual([]);
});

test('Reopen on a Done card stages it to Backlog', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=board');
  const drawer = await openDrawer(page, 9);
  await drawer.getByRole('button', { name: 'Reopen issue' }).click();
  await expect(column(page, 'Backlog').locator('[data-issue-card="9"]')).toContainText('Staged · from Done');
  await expect(drawer).toContainText('Reopens the issue');
  expect(await storedCard(page, 9)).toMatchObject({ target: { status: 'backlog' }, snapshot: { state: 'closed', status: 'done' } });
});

test('a failed card stays in its confirmed column naming its target, and an unknown card is locked', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=board');
  await page.evaluate(() => {
    const snapshot = (status: string, labels: string[], assignees: string[]) => ({ state: 'open', labels, assignees, status, updatedAt: '2026-11-20T10:00:00Z' });
    const save = (n: number, card: object) => localStorage.setItem(`repoorbit:issue-draft:real:ada:1:board:${n}`, JSON.stringify({ version: 1, number: n, operationId: null, ...card }));
    save(3, { snapshot: snapshot('todo', ['status:todo'], ['alex']), target: { status: 'in_progress', assigneesAdd: [], assigneesRemove: [] }, phase: 'failed', request: null });
    save(5, { snapshot: snapshot('in_progress', ['status:in-progress'], ['ada']), target: { status: 'in_review', assigneesAdd: [], assigneesRemove: [] }, phase: 'unknown', request: { draft: {}, idempotencyKey: 'k1' } });
  });
  await page.reload();
  await expect(column(page, 'Todo').locator('[data-issue-card="3"]')).toContainText('Failed · keeps In Progress');
  await expect(column(page, 'In Review').locator('[data-issue-card="5"]')).toContainText('Unknown · from In Progress');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'true');
  await expect(card(page, 5)).toHaveAttribute('draggable', 'false');
  const drawer = await openDrawer(page, 5);
  await expect(drawer).toContainText('last save is not confirmed yet');
  await expect(drawer.getByRole('radio', { name: 'Done' })).toBeDisabled();
  await expect(drawer.getByRole('button', { name: 'Discard' })).toBeDisabled();
});

test('staging respects an unknown card written before the Board refreshes', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=board');
  const drawer = await openDrawer(page, 3);
  await drawer.getByRole('radio', { name: 'In Progress' }).check();
  const unknown = await page.evaluate(() => {
    const key = 'repoorbit:issue-draft:real:ada:1:board:3';
    const updated = { ...JSON.parse(localStorage.getItem(key)!), phase: 'unknown', request: { draft: {}, idempotencyKey: 'other-tab' } };
    localStorage.setItem(key, JSON.stringify(updated));
    return updated;
  });

  const done = drawer.getByRole('radio', { name: 'Done' });
  await done.click();
  expect(await storedCard(page, 3)).toEqual(unknown);
  await expect(done).toBeDisabled();
});

test('Discard removes an expired unknown card with no operation id without sending a write', async ({ page }) => {
  const { posts } = await setup(page);
  await page.goto('/workspace/1?tab=board');
  await page.evaluate(() => {
    localStorage.setItem('repoorbit:issue-draft:real:ada:1:board:3', JSON.stringify({
      version: 1, number: 3, phase: 'unknown', operationId: null,
      attemptedAt: new Date(Date.now() - 30 * 24 * 60 * 60 * 1000).toISOString(),
      snapshot: { state: 'open', labels: ['status:todo'], assignees: ['alex'], status: 'todo', updatedAt: '2026-11-20T10:00:00Z' },
      target: { status: 'in_progress', assigneesAdd: [], assigneesRemove: [] },
      request: { draft: { labels: { add: ['status:in-progress'], remove: ['status:todo'] } }, idempotencyKey: 'expired-key' },
    }));
  });
  await page.reload();

  await expect(card(page, 3)).toContainText('Unknown · from Todo');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'false');
  const drawer = await openDrawer(page, 3);
  await expect(drawer).toContainText('This change may already be on GitHub. Check GitHub first, then discard.');
  await expect(drawer.getByRole('link', { name: 'Write history' })).toBeVisible();
  await expect(drawer.getByRole('radio', { name: 'Done' })).toBeDisabled();
  await expect(drawer.getByRole('button', { name: 'Stage again' })).toHaveCount(0);
  await expect(saveButton(page)).toBeDisabled();
  const discard = drawer.getByRole('button', { name: 'Discard' });
  await expect(discard).toBeEnabled();
  await discard.click();

  await expect(drawer.getByRole('region', { name: 'Staged change' })).toHaveCount(0);
  await expect(column(page, 'Todo').locator('[data-issue-card="3"]')).toHaveAttribute('draggable', 'true');
  expect(await storedCard(page, 3)).toBeNull();
  expect(posts).toEqual([]);
});

test('issue=12 in the URL opens the drawer for #12 and includes it in the Board read', async ({ page }) => {
  const { boardIncludes } = await setup(page);
  await page.goto('/workspace/1?tab=board&issue=12');
  const drawer = page.getByRole('dialog', { name: 'Issue #12' });
  await expect(drawer).toContainText('Fix pagination edge case');
  await expect(drawer.getByRole('link', { name: /GitHub/ })).toHaveAttribute('href', 'https://github.com/ada/repo-1/issues/12');
  expect(boardIncludes[0]).toContain(12);
});

test('a staged card survives a reload and confirmed access loss clears it with the editor drafts', async ({ page }) => {
  const { boardIncludes } = await setup(page);
  await page.goto('/workspace/1?tab=board');
  const drawer = await openDrawer(page, 3);
  await drawer.getByRole('radio', { name: 'In Progress' }).check();
  await expect(column(page, 'In Progress').locator('[data-issue-card="3"]')).toContainText('Staged');
  await page.evaluate(() => localStorage.setItem('repoorbit:issue-draft:real:ada:1:create', JSON.stringify({ version: 1, title: 'Draft' })));

  await page.goto('/workspace/1?tab=board');
  await expect(column(page, 'In Progress').locator('[data-issue-card="3"]')).toContainText('Staged · from Todo');
  expect(boardIncludes.at(-1)).toContain(3);

  await page.route('**/graphql', route => String(route.request().postDataJSON().query).startsWith('query RepositoryOverview')
    ? route.fulfill({ json: { data: { repositoryOverview: null } } })
    : route.fallback());
  await page.reload();
  await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter(key => key.startsWith('repoorbit:issue-draft:')))).toEqual([]);
});

test('failing an independent Board refetch hides rows, filters, staging controls, and an open drawer; authoritative null clears drafts', async ({ page }) => {
  await setup(page);
  let boardMode: 'ok' | 'transient' | 'unverified' | 'denied' = 'ok';
  await page.route('**/graphql', route => {
    const { query } = route.request().postDataJSON();
    if (!String(query).startsWith('query RepositoryBoard')) return route.fallback();
    if (boardMode === 'transient') {
      return route.fulfill({ status: 502, body: 'Bad Gateway' });
    }
    if (boardMode === 'unverified') {
      return route.fulfill({
        json: {
          data: { repositoryBoard: null },
          errors: [{ message: 'Access unverified', path: ['repositoryBoard'], extensions: { code: 'ACCESS_UNVERIFIED' } }],
        },
      });
    }
    if (boardMode === 'denied') {
      return route.fulfill({ json: { data: { repositoryBoard: null } } });
    }
    return route.fallback();
  });

  await page.goto('/workspace/1?tab=board&issue=3');
  const drawer = page.getByRole('dialog', { name: 'Issue #3' });
  await expect(drawer).toBeVisible();
  await drawer.getByRole('radio', { name: 'In Progress' }).check();
  await expect(page.getByText('1 staged change')).toBeVisible();
  await page.evaluate(() => localStorage.setItem('repoorbit:issue-draft:real:ada:1:create', JSON.stringify({ version: 1, title: 'Private draft' })));

  boardMode = 'transient';
  await page.evaluate(() => {
    window.dispatchEvent(new Event('offline'));
    window.dispatchEvent(new Event('online'));
  });

  await expect(drawer).toHaveCount(0);
  await expect(page.locator('main [data-issue-card]')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Save…' })).toHaveCount(0);
  await expect(page.getByLabel('Filter by assignee').locator('option')).toHaveText(['All assignees']);
  await expect(page.getByLabel('Filter by label').locator('option')).toHaveText(['All labels']);
  expect(await page.evaluate(() => Object.keys(localStorage).filter(k => k.startsWith('repoorbit:issue-draft:real:ada:1:')))).toHaveLength(2);

  boardMode = 'unverified';
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Repository', exact: true })).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: 'repo-1', exact: true })).toHaveCount(0);
  expect(await page.evaluate(() => Object.keys(localStorage).filter(k => k.startsWith('repoorbit:issue-draft:real:ada:1:')))).toHaveLength(2);

  boardMode = 'denied';
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter(k => k.startsWith('repoorbit:issue-draft:real:ada:1:')))).toEqual([]);
  await expect(page.getByRole('navigation', { name: 'Primary' }).getByRole('link', { name: /^(repo-1|Repository)$/ })).toHaveCount(0);
});

test('Save is disabled with no staged cards and enabled with one or more (R1)', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=board');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'true');
  await expect(saveButton(page)).toBeDisabled();
  await expect(saveButton(page)).toHaveAttribute('title', 'No staged changes to save.');

  await stageStatus(page, 3, 'In Progress');
  await expect(saveButton(page)).toBeEnabled();
  await stageStatus(page, 1, 'Todo');
  await expect(page.getByText('2 staged changes')).toBeVisible();
  await expect(saveButton(page)).toBeEnabled();
});

test('demo mode disables Save and its title gives the reason (R1)', async ({ page }) => {
  await setup(page, { session: demo });
  await page.goto('/workspace/1?tab=board');
  await expect(card(page, 3)).toBeVisible();
  await expect(saveButton(page)).toBeDisabled();
  await expect(saveButton(page)).toHaveAttribute('title', 'Demo mode — remote GitHub writes are disabled, so Board changes cannot be staged.');
});

test('the review lists each staged card with its title, from-to change and side effects, and sends nothing (R2)', async ({ page }) => {
  const { posts } = await setup(page);
  await page.goto('/workspace/1?tab=board');
  await stageStatus(page, 3, 'Done');
  await stageStatus(page, 1, 'In Progress');
  await saveButton(page).click();

  const items = review(page).locator('[data-review-card]');
  await expect(items).toHaveCount(2);
  await expect(items.nth(0)).toHaveAttribute('data-review-card', '1');
  await expect(items.nth(0)).toContainText('#1 Plan the roadmap');
  await expect(items.nth(0)).toContainText('Backlog → In Progress');
  await expect(items.nth(0).getByRole('listitem')).toHaveText(['Assigns you (ada)']);
  await expect(items.nth(1)).toHaveAttribute('data-review-card', '3');
  await expect(items.nth(1)).toContainText('#3 Write the importer');
  await expect(items.nth(1)).toContainText('Todo → Done');
  await expect(items.nth(1).getByRole('listitem')).toHaveText(['Closes the issue as completed', 'Removes label status:todo']);

  await review(page).getByRole('button', { name: 'Cancel' }).click();
  await expect(review(page)).toHaveCount(0);
  await expect(saveButton(page)).toBeFocused();
  await expect(page.getByText('2 staged changes')).toBeVisible();
  expect(posts).toEqual([]);
});

test('Save is clickable with the task drawer open, and the review contains focus', async ({ page }) => {
  await page.setViewportSize({ width: 900, height: 900 });
  await setup(page);
  await page.goto('/workspace/1?tab=board');
  const drawer = await openDrawer(page, 3);
  await drawer.getByRole('radio', { name: 'In Progress' }).check();
  await saveButton(page).click();
  const dialog = review(page);
  await expect(dialog).toBeVisible();
  expect(await dialog.evaluate(el => el.matches('dialog:modal'))).toBe(true);
  await page.keyboard.press('Tab');
  expect(await dialog.evaluate(el => el.contains(document.activeElement))).toBe(true);
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(drawer).toBeVisible();
  await expect(saveButton(page)).toBeFocused();
  expect(await page.evaluate(() => {
    const root = document.documentElement;
    return root.scrollWidth <= innerWidth && root.scrollHeight <= innerHeight;
  })).toBe(true);
});

test('confirming two staged cards sends one update each in issue-number order and clears the staged count (R3, R4)', async ({ page }) => {
  const { issues, posts } = await setup(page);
  const finished: string[] = [];
  const sent = await mockUpdates(page, async (route, update) => {
    finished.push(`request ${update.number}`);
    // Holds the first answer back, so a second send that did not wait for it would be recorded before it.
    if (update.number === 3) await new Promise(resolve => setTimeout(resolve, 300));
    finished.push(`answer ${update.number}`);
    const row = issues.find(row => row.number === update.number)!;
    if (update.number === 3) Object.assign(row, { status: 'in_progress', labels: ['status:in-progress'] });
    else Object.assign(row, { status: 'todo', labels: [...row.labels, 'status:todo'] });
    await route.fulfill({ status: 201, json: operation(update.number, 'succeeded') });
  });
  await page.goto('/workspace/1?tab=board');
  // Staged highest first. As text, 12 would also sort before 3.
  await stageStatus(page, 12, 'Todo');
  await stageStatus(page, 3, 'In Progress');
  await confirmSave(page);

  await expect(page.getByText('0 staged changes')).toBeVisible();
  await expect(review(page)).toHaveCount(0);
  await expect(saveButton(page)).toBeDisabled();
  await expect(column(page, 'In Progress').locator('[data-issue-card="3"]')).toBeVisible();
  await expect(column(page, 'Todo').locator('[data-issue-card="12"]')).toBeVisible();
  expect(finished).toEqual(['request 3', 'answer 3', 'request 12', 'answer 12']);
  expect(sent.map(update => update.body)).toEqual([
    { labels: { add: ['status:in-progress'], remove: ['status:todo'] }, expect: { status: 'todo', assignees: ['alex'], statusLabels: ['status:todo'] } },
    { labels: { add: ['status:todo'] }, expect: { status: 'backlog', assignees: [], statusLabels: [] } },
  ]);
  for (const update of sent) expect(update.key).toMatch(/^[A-Za-z0-9_.:-]{1,128}$/);
  expect(sent[0].key).not.toBe(sent[1].key);
  expect(await boardKeys(page)).toEqual([]);
  expect(posts).toHaveLength(2);
});

test('a changed_on_github response marks the card Changed on GitHub, with no Board read before the send (R5)', async ({ page }) => {
  await setup(page);
  const events: string[] = [];
  page.on('request', request => {
    const path = new URL(request.url()).pathname;
    if (path === '/graphql' && String(request.postDataJSON().query).startsWith('query RepositoryBoard')) events.push('board read');
    else if (path.startsWith('/api/repositories/1/issues/')) events.push(`${request.method()} ${path}`);
  });
  const sent = await mockUpdates(page, route => route.fulfill({
    status: 409,
    json: { current: { state: 'open', labels: ['status:review'], assignees: ['sam'], status: 'in_review', updatedAt: '2026-11-25T11:00:00Z' } },
  }));
  await page.goto('/workspace/1?tab=board');
  await stageStatus(page, 3, 'In Progress');
  await saveButton(page).click();
  await expect(review(page)).toBeVisible();
  events.length = 0;
  await review(page).getByRole('button', { name: 'Confirm' }).click();

  await expect(card(page, 3)).toContainText('Changed on GitHub');
  await expect(review(page)).toHaveCount(0);
  // The Board is read again only after the send.
  await expect.poll(() => events).toEqual(['POST /api/repositories/1/issues/3/updates', 'board read']);
  expect(sent).toHaveLength(1);
  expect(await storedCard(page, 3)).toMatchObject({ phase: 'changed', request: null, snapshot: { status: 'in_review', assignees: ['sam'] }, target: { status: 'in_progress' } });
  await expect(saveButton(page)).toBeDisabled();
  const drawer = await openDrawer(page, 3);
  await expect(drawer).toContainText('This issue changed on GitHub. Stage your change again to save it, or discard it.');
  await drawer.getByRole('button', { name: 'Stage again' }).click();
  expect(await storedCard(page, 3)).toMatchObject({ phase: 'staged', snapshot: { status: 'in_review' }, target: { status: 'in_progress' } });
});

test('a not-recorded response leaves the card Failed, and saving it again sends a new Idempotency-Key (R6)', async ({ page }) => {
  await setup(page);
  const sent = await mockUpdates(page, route => route.fulfill({
    status: 503, contentType: 'text/plain', headers: { 'RepoOrbit-Write-Disposition': 'not-recorded' }, body: 'GitHub access could not be verified right now.',
  }));
  await page.goto('/workspace/1?tab=board');
  await stageStatus(page, 3, 'In Progress');
  await confirmSave(page);

  await expect(column(page, 'Todo').locator('[data-issue-card="3"]')).toContainText('Failed · keeps In Progress');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'true');
  await expect(saveButton(page)).toBeDisabled();
  expect(await storedCard(page, 3)).toMatchObject({ phase: 'failed', request: null, operationId: null });
  expect(sent).toHaveLength(1);

  // A failed card is saved again by staging it again.
  const drawer = await openDrawer(page, 3);
  await expect(drawer).toContainText('The save failed. Stage your change again to retry, or discard it.');
  await drawer.getByRole('button', { name: 'Stage again' }).click();
  expect(await storedCard(page, 3)).toMatchObject({ phase: 'staged', target: { status: 'in_progress' } });
  await drawer.getByRole('button', { name: 'Close task drawer' }).click();
  await card(page, 3).dragTo(column(page, 'In Review'), { steps: 15 });
  await expect(card(page, 3)).toContainText('Staged · from Todo');
  await confirmSave(page);

  await expect(card(page, 3)).toContainText('Failed · keeps In Review');
  expect(sent).toHaveLength(2);
  expect(sent[0].key).toBeTruthy();
  expect(sent[1].key).toBeTruthy();
  expect(sent[1].key).not.toBe(sent[0].key);
});

test('an unknown card with an operation id is locked and Recheck calls the recheck route (R7)', async ({ page }) => {
  await setup(page);
  const sent = await mockUpdates(page, route => route.fulfill({ status: 202, json: operation(3, 'unknown', 77) }));
  const rechecks: Record<string, unknown>[] = [];
  await page.route('**/api/write-operations/77/recheck', route => {
    rechecks.push(route.request().postDataJSON());
    return route.fulfill({ status: 200, json: operation(3, 'succeeded', 77) });
  });
  await page.goto('/workspace/1?tab=board');
  await stageStatus(page, 3, 'In Progress');
  await confirmSave(page);

  await expect(column(page, 'In Progress').locator('[data-issue-card="3"]')).toContainText('Unknown · from Todo');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'false');
  await expect(saveButton(page)).toBeDisabled();
  const drawer = await openDrawer(page, 3);
  await expect(drawer.getByRole('radio', { name: 'Done' })).toBeDisabled();
  await expect(drawer.getByRole('button', { name: 'Discard' })).toBeDisabled();
  await expect(drawer.getByRole('button', { name: 'Check recorded status' })).toHaveCount(0);
  await expect(drawer.getByRole('link', { name: 'Write history' })).toBeVisible();
  expect(rechecks).toEqual([]);
  await drawer.getByRole('button', { name: 'Recheck' }).click();

  await expect(drawer.getByRole('region', { name: 'Staged change' })).toHaveCount(0);
  expect(rechecks).toEqual([{ issueNumber: 3 }]);
  expect(sent).toHaveLength(1);
  expect(await boardKeys(page)).toEqual([]);
});

test('an unknown card with no operation id is locked and Check recorded status resends under the same Idempotency-Key (R7)', async ({ page }) => {
  await setup(page);
  let answered = false;
  const sent = await mockUpdates(page, route => {
    if (answered) return route.fulfill({ status: 200, json: operation(3, 'succeeded') });
    answered = true;
    return route.fulfill({ status: 502, contentType: 'text/plain', headers: { 'RepoOrbit-Write-Disposition': 'possibly-recorded' }, body: 'No reliable answer from GitHub.' });
  });
  await page.goto('/workspace/1?tab=board');
  await stageStatus(page, 3, 'In Progress');
  await confirmSave(page);

  await expect(column(page, 'In Progress').locator('[data-issue-card="3"]')).toContainText('Unknown · from Todo');
  await expect(card(page, 3)).toHaveAttribute('draggable', 'false');
  await expect(saveButton(page)).toBeDisabled();
  const stored = await storedCard(page, 3);
  expect(stored).toMatchObject({ phase: 'unknown', operationId: null, request: { idempotencyKey: sent[0].key } });
  expect(Date.now() - Date.parse(stored.attemptedAt)).toBeLessThan(60_000);

  const drawer = await openDrawer(page, 3);
  await expect(drawer.getByRole('radio', { name: 'Done' })).toBeDisabled();
  await expect(drawer.getByRole('button', { name: 'Discard' })).toBeDisabled();
  await expect(drawer.getByRole('button', { name: 'Recheck' })).toHaveCount(0);
  await expect(drawer.getByRole('link', { name: 'Write history' })).toBeVisible();
  expect(sent).toHaveLength(1);
  await drawer.getByRole('button', { name: 'Check recorded status' }).click();

  await expect(drawer.getByRole('region', { name: 'Staged change' })).toHaveCount(0);
  expect(sent).toHaveLength(2);
  expect(sent[1].key).toBe(sent[0].key);
  expect(sent[1].body).toEqual(sent[0].body);
  expect(await boardKeys(page)).toEqual([]);
});

for (const replacementId of [null, 78]) {
  test(`recovery preserves storage when the card is ${replacementId === null ? 'cleared' : 'given another operation id'} in flight`, async ({ page }) => {
    await page.addInitScript(() => Object.defineProperty(navigator, 'locks', { value: undefined }));
    await setup(page);
    await mockUpdates(page, route => route.fulfill({ status: 202, json: operation(3, 'unknown', 77) }));
    let pending: Route | undefined;
    await page.route('**/api/write-operations/77/recheck', route => { pending = route; });
    await page.goto('/workspace/1?tab=board');
    await stageStatus(page, 3, 'In Progress');
    await confirmSave(page);
    const drawer = await openDrawer(page, 3);
    await drawer.getByRole('button', { name: 'Recheck' }).click();
    await expect.poll(() => Boolean(pending)).toBe(true);
    await expect(drawer.getByRole('button', { name: 'Rechecking…' })).toBeVisible();

    await page.evaluate(id => {
      const key = 'repoorbit:issue-draft:real:ada:1:board:3';
      if (id === null) localStorage.removeItem(key);
      else localStorage.setItem(key, JSON.stringify({ ...JSON.parse(localStorage.getItem(key)!), operationId: id }));
    }, replacementId);
    const latest = await storedCard(page, 3);
    await pending!.fulfill({ json: operation(3, 'unknown', 77) });
    await expect(drawer.getByRole('button', { name: 'Rechecking…' })).toHaveCount(0);
    expect(await storedCard(page, 3)).toEqual(latest);
    if (replacementId === null) await expect(drawer.getByRole('region', { name: 'Staged change' })).toHaveCount(0);
  });
}

test('another tab waits for the save lock, rereads the card, and refreshes the Board on removal', async ({ page, context }) => {
  const { issues } = await setup(page);
  const other = await context.newPage();
  const { issues: otherIssues, boardIncludes } = await setup(other);
  let pending: Route | undefined;
  const sent = await mockUpdates(page, async route => { pending = route; });
  const replays = await mockUpdates(other);
  await page.goto('/workspace/1?tab=board');
  await stageStatus(page, 3, 'In Progress');
  await confirmSave(page);
  await expect.poll(() => Boolean(pending)).toBe(true);
  const lockName = 'repoorbit:issue-draft:real:ada:1:board:';
  expect((await page.evaluate(() => navigator.locks.query())).held).toEqual([expect.objectContaining({ name: lockName, mode: 'exclusive' })]);

  await other.goto('/workspace/1?tab=board');
  const drawer = await openDrawer(other, 3);
  await drawer.getByRole('button', { name: 'Check recorded status' }).click();
  await expect.poll(async () => (await other.evaluate(() => navigator.locks.query())).pending?.map(lock => lock.name)).toEqual([lockName]);
  const readsBeforeRemoval = boardIncludes.length;
  for (const rows of [issues, otherIssues]) Object.assign(rows.find(row => row.number === 3)!, { status: 'in_progress', labels: ['status:in-progress'] });
  await pending!.fulfill({ json: operation(3, 'succeeded') });

  await expect(review(page)).toHaveCount(0);
  await expect(drawer.getByRole('region', { name: 'Staged change' })).toHaveCount(0);
  await expect(column(other, 'In Progress').locator('[data-issue-card="3"]')).toBeVisible();
  expect(boardIncludes).toHaveLength(readsBeforeRemoval + 1);
  expect(sent).toHaveLength(1);
  expect(replays).toEqual([]);
});

test('a transport failure names the reviewed cards that remain staged and the next save clears the notice', async ({ page }) => {
  await setup(page);
  const sent = await mockUpdates(page, route => route.abort());
  await page.goto('/workspace/1?tab=board');
  for (const n of [1, 3, 5]) await stageStatus(page, n, 'In Review');
  await confirmSave(page);

  await expect(review(page)).toHaveCount(0);
  expect(sent.map(update => update.number)).toEqual([1]);
  const notice = page.getByRole('status').filter({ hasText: '#3, #5 were not sent and are still staged.' });
  await expect(notice).toHaveCount(1);
  await saveButton(page).click();
  await review(page).getByRole('button', { name: 'Cancel' }).click();
  await expect(notice).toHaveCount(0);
});

for (const [change, alter] of [
  ['removed', null],
  ['changed', { status: 'done', assigneesAdd: [], assigneesRemove: [] }],
] as const) {
  test(`a card whose stored entry is ${change} after the review is not sent (R9)`, async ({ page }) => {
    await setup(page);
    const sent = await mockUpdates(page);
    await page.goto('/workspace/1?tab=board');
    await stageStatus(page, 3, 'In Progress');
    await stageStatus(page, 12, 'Todo');
    await saveButton(page).click();
    await expect(review(page).locator('[data-review-card]')).toHaveCount(2);
    // A same-tab write fires no storage event, so the open review still shows card 3 as it was.
    await page.evaluate(([key, target]) => {
      if (target === null) return localStorage.removeItem(key);
      localStorage.setItem(key, JSON.stringify({ ...JSON.parse(localStorage.getItem(key)!), target }));
    }, ['repoorbit:issue-draft:real:ada:1:board:3', alter] as const);
    await expect(review(page).locator('[data-review-card="3"]')).toContainText('Todo → In Progress');
    await review(page).getByRole('button', { name: 'Confirm' }).click();

    await expect(review(page)).toHaveCount(0);
    expect(sent.map(update => update.number)).toEqual([12]);
    if (alter === null) {
      await expect(page.getByText('0 staged changes')).toBeVisible();
      expect(await storedCard(page, 3)).toBeNull();
    } else {
      await expect(page.getByText('1 staged change')).toBeVisible();
      expect(await storedCard(page, 3)).toMatchObject({ phase: 'staged', target: alter, request: null });
      const notice = page.getByRole('status').filter({ hasText: '#3 was not sent and is still staged.' });
      await expect(notice).toBeVisible();
      await stageStatus(page, 3, 'In Review');
      await expect(notice).toHaveCount(0);
    }
  });
}

test('the request and attemptedAt are in browser storage before the update request is observed', async ({ page }) => {
  await setup(page);
  // Snapshots each card's storage entry at the instant the app calls fetch for its update, before anything is sent.
  await page.addInitScript(() => {
    const send = window.fetch.bind(window);
    const storedAtSend: unknown[] = [];
    Object.assign(window, { storedAtSend });
    window.fetch = (input, init) => {
      const number = /\/issues\/(\d+)\/updates$/.exec(String(input))?.[1];
      if (number) storedAtSend.push(JSON.parse(localStorage.getItem(`repoorbit:issue-draft:real:ada:1:board:${number}`) ?? 'null'));
      return send(input, init);
    };
  });
  const sent = await mockUpdates(page);
  await page.goto('/workspace/1?tab=board');
  await stageStatus(page, 3, 'In Progress');
  await stageStatus(page, 12, 'Todo');
  const before = Date.now();
  await confirmSave(page);
  await expect(page.getByText('0 staged changes')).toBeVisible();

  const atSend = await page.evaluate(() => (window as unknown as { storedAtSend: Record<string, unknown>[] }).storedAtSend);
  expect(sent).toHaveLength(2);
  expect(atSend).toHaveLength(2);
  sent.forEach((update, i) => {
    // A send in flight is stored as unknown, so a reload during it leaves the card locked.
    expect(atSend[i]).toMatchObject({ number: update.number, phase: 'unknown', operationId: null });
    expect(atSend[i].request).toEqual({ draft: update.body, idempotencyKey: update.key });
    const attemptedAt = Date.parse(String(atSend[i].attemptedAt));
    expect(attemptedAt).toBeGreaterThanOrEqual(before);
    expect(attemptedAt).toBeLessThanOrEqual(Date.now());
  });
});

for (const { state, badge, columnName, draggable } of [
  { state: 'failed', badge: 'Failed · keeps In Progress', columnName: 'Todo', draggable: 'true' },
  { state: 'unknown', badge: 'Unknown · from Todo', columnName: 'In Progress', draggable: 'false' },
] as const) {
  test(`a resolved response with operation state ${state} shows that state, not success`, async ({ page }) => {
    await setup(page);
    const sent = await mockUpdates(page, route => route.fulfill({ status: 201, json: operation(3, state, 88) }));
    await page.goto('/workspace/1?tab=board');
    await stageStatus(page, 3, 'In Progress');
    await confirmSave(page);

    await expect(review(page)).toHaveCount(0);
    await expect(column(page, columnName).locator('[data-issue-card="3"]')).toContainText(badge);
    await expect(card(page, 3)).toHaveAttribute('draggable', draggable);
    expect(await storedCard(page, 3)).toMatchObject({ phase: state, operationId: 88 });
    expect(sent).toHaveLength(1);
  });
}
