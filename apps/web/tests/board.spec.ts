import { expect, test, type Page } from '@playwright/test';
import { demo, mockSelected, mockSession, selectedRepo, signedIn } from './mocks';

// The Board tab (U6d U4): columns from status labels, filters, the task drawer and browser-only staging. Nothing is
// sent to GitHub before Save, so every test also checks that no POST reached the API.

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
  const issues = labelsOn ? boardIssues : boardIssues.map(i => ({ ...i, status: null, multipleStatus: false }));
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
  return { boardIncludes, posts };
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
