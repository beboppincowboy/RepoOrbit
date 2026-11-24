import { expect, test, type Page } from '@playwright/test';
import { demo, mockSelected, mockSession, selectedRepo, signedIn } from './mocks';

const covered = { covered: true, reason: null, through: '2026-11-17T08:00:00Z' };
const stream = { phase: 'current', boundary: '2026-08-01T00:00:00Z', catchupThrough: null, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, jobState: 'queued', nextAttemptAt: '2026-11-17T09:00:00Z' };
const child = { complete: true, pendingChildren: 0, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, nextAttemptAt: null };
const fullActivity = { synchronized: true, message: '', archived: false, issues: stream, pullRequests: stream, releases: stream, reviews: child, comments: child, issueEvents: { traversal: stream, historicalGap: false }, futureResources: [] };

function overview(over: Record<string, unknown> = {}) {
  return {
    fullName: 'ada/repo-1',
    syncState: 'awaiting-sync',
    activity: fullActivity,
    openIssues: { value: 2, coverage: covered },
    staleOpenIssues: { value: 0, coverage: covered },
    pullRequestsMerged: { value: 1, coverage: covered },
    openPullRequests: { value: 1, coverage: covered },
    firstResponseTime: { seconds: 3600, sampleSize: 2, coverage: covered },
    mergeTime: { seconds: 7200, sampleSize: 1, coverage: covered },
    issuesWithoutResponse: { value: 0 },
    eventsCoveredFrom: '2026-11-03T00:00:00Z',
    windowStart: '2026-10-19T00:00:00Z',
    activeBotContributors: { value: 0, coverage: covered },
    contributorsCoverage: covered,
    contributors: [
      { login: 'alex', isBot: false, pullRequestsOpened: 3, reviewsSubmitted: 2, commentsCreated: 4 },
      { login: 'sam', isBot: false, pullRequestsOpened: 1, reviewsSubmitted: 3, commentsCreated: 2 },
    ],
    timeline: [],
    daily: [],
    ...over,
  };
}

type MockIssue = {
  id: string;
  repositoryId: string;
  number: number;
  title: string;
  state: string;
  url: string;
  authorLogin: string | null;
  updatedAt: string;
  labels: string[];
  assignees: string[];
};

const sampleIssues: MockIssue[] = [
  {
    id: '1012',
    repositoryId: '1',
    number: 12,
    title: 'Fix pagination edge case',
    state: 'open',
    url: 'https://github.com/ada/repo-1/issues/12',
    authorLogin: 'alex',
    updatedAt: '2026-11-16T09:00:00Z',
    labels: ['triage', 'keep-me'],
    assignees: ['alex'],
  },
  {
    id: '1015',
    repositoryId: '1',
    number: 15,
    title: 'Authored by viewer',
    state: 'open',
    url: 'https://github.com/ada/repo-1/issues/15',
    authorLogin: 'ada',
    updatedAt: '2026-11-16T10:00:00Z',
    labels: ['docs'],
    assignees: [],
  },
];

async function setupWorkspace(
  page: Page,
  opts: {
    session?: typeof signedIn | typeof demo;
    overviewOver?: Record<string, unknown>;
    issues?: MockIssue[];
    capabilities?: { canCreateIssue: boolean; canCreateIssueWithMetadata: boolean; canManageIssues: boolean };
    operations?: Record<string, unknown>[];
  } = {},
) {
  const session = opts.session ?? signedIn;
  const issues = opts.issues ?? sampleIssues;
  const operations = opts.operations ?? [];
  await mockSession(page, session);
  await mockSelected(page, [selectedRepo('1', { syncState: session.mode === 'demo' ? 'demo' : 'awaiting-sync' })]);

  await page.route('**/graphql', async route => {
    const query = String(route.request().postDataJSON().query);
    if (query.startsWith('query RepositoryOverview')) {
      return route.fulfill({ json: { data: { repositoryOverview: overview(opts.overviewOver) } } });
    }
    if (query.startsWith('query WorkspaceIssues')) {
      return route.fulfill({
        json: {
          data: {
            issues: {
              nodes: issues,
              withheld: 0,
              retryAt: null,
              hasNextPage: false,
              endCursor: issues.at(-1)?.id ?? null,
            },
          },
        },
      });
    }
    return route.fallback();
  });

  if (session.mode === 'real') {
    const caps = opts.capabilities ?? { canCreateIssue: true, canCreateIssueWithMetadata: true, canManageIssues: true };
    await page.route('**/api/repositories/1/issue-capabilities', route => route.fulfill({ json: caps }));
    await page.route('**/api/repositories/1/write-operations', route => route.fulfill({ json: { operations } }));
  }
}

test.beforeEach(async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
});

test('creates a new issue through Edit -> Review -> Submit -> Confirmed and shows it in write history', async ({ page }) => {
  const ops: Record<string, unknown>[] = [];
  const createRequests: { headers: Record<string, string>; body: Record<string, unknown> }[] = [];

  await setupWorkspace(page, { operations: ops });
  await page.route('**/api/repositories/1/issues', async route => {
    const body = route.request().postDataJSON() as Record<string, unknown>;
    const headers = route.request().headers();
    createRequests.push({ headers, body });
    const created = {
      id: 501,
      repositoryId: 1,
      kind: 'create',
      state: 'succeeded',
      issueNumber: 42,
      draft: body,
      outcome: {
        issue: {
          number: 42,
          url: 'https://github.com/ada/repo-1/issues/42',
          title: body.title,
          state: 'open',
          labels: body.labels ?? [],
          assignees: body.assignees ?? [],
        },
      },
      actor: 'ada',
      createdAt: '2026-11-17T12:00:00Z',
      updatedAt: '2026-11-17T12:00:01Z',
    };
    ops.unshift(created);
    return route.fulfill({ status: 201, json: created });
  });

  await page.goto('/workspace/1?tab=issues');
  await expect(page.getByRole('region', { name: 'Repository issues' })).toBeVisible();
  expect(await page.getByRole('main').evaluate(el => el.scrollHeight <= el.clientHeight)).toBe(true);

  await page.getByRole('region', { name: 'Repository issues' }).getByRole('button', { name: '+ New issue' }).click();
  const dialog = page.getByRole('dialog', { name: 'New issue' });
  await expect(dialog).toBeVisible();

  await dialog.getByLabel('Title').fill('Support keyboard shortcuts');
  await dialog.getByLabel('Description').fill('Add shortcuts for workspace tabs.');
  await dialog.getByLabel('Labels (optional)').fill('enhancement');
  await dialog.getByRole('button', { name: 'Add label' }).click();
  await dialog.getByLabel('Assignees (optional)').fill('alex');
  await dialog.getByRole('button', { name: 'Add assignee' }).click();

  await dialog.getByRole('button', { name: 'Review' }).click();
  const review = dialog.getByRole('region', { name: 'Reviewed changes' });
  await expect(review).toContainText('+ create issue in ada/repo-1: “Support keyboard shortcuts”');
  await expect(review).toContainText('+ label: enhancement');
  await expect(review).toContainText('+ assignee: @alex');

  await dialog.getByRole('button', { name: 'Submit to GitHub' }).click();
  await expect(dialog.getByText('Confirmed on GitHub.')).toBeVisible();
  await expect(dialog).toContainText('#42 · Support keyboard shortcuts');
  await expect(dialog).toContainText('Assignees: @alex');
  await expect(dialog.getByRole('link', { name: /Open on GitHub/ })).toHaveAttribute('href', 'https://github.com/ada/repo-1/issues/42');

  expect(createRequests).toHaveLength(1);
  expect(createRequests[0].headers['x-csrf-token']).toBe('t');
  expect(createRequests[0].headers['idempotency-key']).toMatch(/^[A-Za-z0-9_.:-]{1,128}$/);
  expect(createRequests[0].body).toEqual({
    title: 'Support keyboard shortcuts',
    body: 'Add shortcuts for workspace tabs.',
    labels: ['enhancement'],
    assignees: ['alex'],
  });

  await dialog.getByRole('button', { name: 'Done' }).click();
  await expect(dialog).toHaveCount(0);

  const history = page.getByRole('region', { name: 'Write history' });
  await expect(history).toContainText('Confirmed');
  await expect(history).toContainText('Create "Support keyboard shortcuts"');
  await expect(history.getByRole('link', { name: /#42/ })).toHaveAttribute('href', 'https://github.com/ada/repo-1/issues/42');
});

test('edits an existing issue with add/remove deltas and open-state change', async ({ page }) => {
  const updateRequests: Record<string, unknown>[] = [];
  const ops: Record<string, unknown>[] = [];

  await setupWorkspace(page, { operations: ops });
  await page.route('**/api/repositories/1/issues/12/updates', async route => {
    const body = route.request().postDataJSON() as Record<string, unknown>;
    updateRequests.push(body);
    const updated = {
      id: 601,
      repositoryId: 1,
      kind: 'update',
      state: 'succeeded',
      issueNumber: 12,
      draft: body,
      outcome: {
        issue: {
          number: 12,
          url: 'https://github.com/ada/repo-1/issues/12',
          title: 'Fix pagination edge case',
          state: 'closed',
          labels: ['keep-me', 'bug'],
          assignees: ['sam'],
        },
        allStepsAnswered: true,
      },
      actor: 'ada',
      createdAt: '2026-11-17T12:05:00Z',
      updatedAt: '2026-11-17T12:05:01Z',
    };
    ops.unshift(updated);
    return route.fulfill({ status: 201, json: updated });
  });

  await page.goto('/workspace/1?tab=issues');
  await page.getByRole('button', { name: 'Edit issue #12' }).click();
  const dialog = page.getByRole('dialog', { name: 'Edit issue #12' });

  await dialog.getByLabel('Open / closed state').selectOption('closed');
  await dialog.getByRole('button', { name: /^triage/ }).click();
  await dialog.getByLabel('Labels (add or remove)').fill('bug');
  await dialog.getByRole('button', { name: 'Add label' }).click();
  await dialog.getByRole('button', { name: /^@alex/ }).click();
  await dialog.getByLabel('Assignees (add or remove)').fill('sam');
  await dialog.getByRole('button', { name: 'Add assignee' }).click();

  await dialog.getByRole('button', { name: 'Review' }).click();
  const review = dialog.getByRole('region', { name: 'Reviewed changes' });
  await expect(review).toContainText('+ add label: bug');
  await expect(review).toContainText('- remove label: triage');
  await expect(review).toContainText('+ add assignee: @sam');
  await expect(review).toContainText('- remove assignee: @alex');
  await expect(review).toContainText('~ state: closed');

  await dialog.getByRole('button', { name: 'Submit to GitHub' }).click();
  await expect(dialog.getByText('Confirmed on GitHub.')).toBeVisible();
  await expect(dialog).toContainText('Issue #12 (closed) matches all requested changes.');

  expect(updateRequests).toEqual([
    {
      labels: { add: ['bug'], remove: ['triage'] },
      assignees: { add: ['sam'], remove: ['alex'] },
      state: 'closed',
    },
  ]);
});

test('unknown create never resends, rejects mismatched recheck, and links verified issue after reload', async ({ page }) => {
  let createCalls = 0;
  const recheckCalls: number[] = [];
  let operation: Record<string, unknown> = {
    id: 701,
    repositoryId: 1,
    kind: 'create',
    state: 'unknown',
    issueNumber: null,
    draft: { title: 'Investigate slow query', body: 'Query takes 5s' },
    outcome: { error: 'no reliable answer from GitHub; the change may or may not have been applied' },
    actor: 'ada',
    searchUrl: 'https://github.com/ada/repo-1/issues?q=is%3Aissue+author%3Aada',
    createdAt: '2026-11-17T12:10:00Z',
    updatedAt: '2026-11-17T12:10:00Z',
  };
  const ops: Record<string, unknown>[] = [];

  await setupWorkspace(page, { operations: ops });
  await page.route('**/api/repositories/1/issues', async route => {
    createCalls++;
    ops.splice(0, ops.length, operation);
    return route.fulfill({ status: 202, json: operation });
  });
  await page.route('**/api/write-operations/701/recheck', async route => {
    const body = route.request().postDataJSON() as { issueNumber?: number };
    recheckCalls.push(body.issueNumber ?? 0);
    if (body.issueNumber !== 43) {
      return route.fulfill({ status: 409, json: operation });
    }
    operation = {
      ...operation,
      state: 'succeeded',
      issueNumber: 43,
      searchUrl: undefined,
      outcome: {
        issue: {
          number: 43,
          url: 'https://github.com/ada/repo-1/issues/43',
          title: 'Investigate slow query',
          state: 'open',
          labels: [],
          assignees: [],
        },
      },
      updatedAt: '2026-11-17T12:11:00Z',
    };
    ops.splice(0, ops.length, operation);
    return route.fulfill({ status: 200, json: operation });
  });

  await page.goto('/workspace/1?tab=issues');
  await page.getByRole('region', { name: 'Repository issues' }).getByRole('button', { name: '+ New issue' }).click();
  const dialog = page.getByRole('dialog', { name: 'New issue' });

  await dialog.getByLabel('Title').fill('Investigate slow query');
  await dialog.getByLabel('Description').fill('Query takes 5s');
  await dialog.getByRole('button', { name: 'Review' }).click();
  await dialog.getByRole('button', { name: 'Submit to GitHub' }).click();

  await expect(dialog.getByText('Outcome unknown.')).toBeVisible();
  await expect(dialog.getByRole('link', { name: /Search GitHub for this issue/ })).toHaveAttribute(
    'href',
    'https://github.com/ada/repo-1/issues?q=is%3Aissue+author%3Aada',
  );
  expect(createCalls).toBe(1);

  // Mismatched issue number cannot resolve the operation and does not resend the create.
  await dialog.getByLabel(/Found the created issue on GitHub/).fill('99');
  await dialog.getByRole('button', { name: 'Recheck' }).click();
  await expect(dialog.getByRole('alert')).toContainText('Issue #99 could not be matched to this draft');
  await expect(dialog.getByText('Outcome unknown.')).toBeVisible();
  expect(createCalls).toBe(1);
  expect(recheckCalls).toEqual([99]);

  // Reloading preserves the unknown operation and lets the user resolve it with the real issue number.
  await page.reload();
  const reloadedDialog = page.getByRole('dialog', { name: 'New issue' });
  await expect(reloadedDialog.getByText('Outcome unknown.')).toBeVisible();

  await reloadedDialog.getByLabel(/Found the created issue on GitHub/).fill('43');
  await reloadedDialog.getByRole('button', { name: 'Recheck' }).click();
  await expect(reloadedDialog.getByText('Confirmed on GitHub.')).toBeVisible();
  await expect(reloadedDialog).toContainText('#43 · Investigate slow query');
  expect(createCalls).toBe(1);
  expect(recheckCalls).toEqual([99, 43]);
});

test('failed submit preserves draft across reload and retries with a new idempotency key', async ({ page }) => {
  const keys: string[] = [];
  await setupWorkspace(page);
  await page.route('**/api/repositories/1/issues', async route => {
    const body = route.request().postDataJSON() as { title: string; body?: string; assignees?: string[] };
    keys.push(route.request().headers()['idempotency-key'] ?? '');
    if (body.assignees?.includes('stranger')) {
      return route.fulfill({ status: 422, contentType: 'text/plain', body: 'cannot be assigned in this repository: stranger' });
    }
    return route.fulfill({
      status: 201,
      json: {
        id: 801,
        repositoryId: 1,
        kind: 'create',
        state: 'succeeded',
        issueNumber: 44,
        draft: body,
        outcome: {
          issue: {
            number: 44,
            url: 'https://github.com/ada/repo-1/issues/44',
            title: body.title,
            state: 'open',
            labels: [],
            assignees: body.assignees ?? [],
          },
        },
        actor: 'ada',
        createdAt: '2026-11-17T12:20:00Z',
        updatedAt: '2026-11-17T12:20:01Z',
      },
    });
  });

  await page.goto('/workspace/1');
  await page.getByRole('button', { name: '+ New issue' }).click();
  const dialog = page.getByRole('dialog', { name: 'New issue' });

  await dialog.getByLabel('Title').fill('Fix flaky login');
  await dialog.getByLabel('Description').fill('Steps to reproduce');
  await dialog.getByLabel('Assignees (optional)').fill('stranger');
  await dialog.getByRole('button', { name: 'Add assignee' }).click();

  await dialog.getByRole('button', { name: 'Review' }).click();
  await dialog.getByRole('button', { name: 'Submit to GitHub' }).click();
  await expect(dialog.getByRole('alert')).toContainText('cannot be assigned in this repository: stranger');

  // Reloading restores the failed state and full draft without losing entered text.
  await page.reload();
  const reloaded = page.getByRole('dialog', { name: 'New issue' });
  await expect(reloaded.getByRole('alert')).toContainText('cannot be assigned in this repository: stranger');
  await reloaded.getByRole('button', { name: 'Back to edit' }).click();

  await expect(reloaded.getByLabel('Title')).toHaveValue('Fix flaky login');
  await expect(reloaded.getByLabel('Description')).toHaveValue('Steps to reproduce');
  await expect(reloaded.getByRole('button', { name: 'Remove assignee stranger' })).toBeVisible();

  await reloaded.getByRole('button', { name: 'Remove assignee stranger' }).click();
  await reloaded.getByLabel('Assignees (optional)').fill('alex');
  await reloaded.getByRole('button', { name: 'Add assignee' }).click();
  await reloaded.getByRole('button', { name: 'Review' }).click();
  await reloaded.getByRole('button', { name: 'Submit to GitHub' }).click();

  await expect(reloaded.getByText('Confirmed on GitHub.')).toBeVisible();
  expect(keys).toHaveLength(2);
  expect(keys[0]).not.toBe(keys[1]);
});

test('disables disallowed actions with the GitHub permission reason', async ({ page }) => {
  await setupWorkspace(page, {
    capabilities: { canCreateIssue: true, canCreateIssueWithMetadata: false, canManageIssues: false },
  });

  await page.goto('/workspace/1?tab=issues');

  // New issue: Read user can create plain issue, but labels/assignees are disabled with Push reason.
  await page.getByRole('region', { name: 'Repository issues' }).getByRole('button', { name: '+ New issue' }).click();
  let dialog = page.getByRole('dialog', { name: 'New issue' });
  await expect(dialog.getByLabel('Title')).toBeEnabled();
  await expect(dialog.getByLabel('Labels (optional)')).toBeDisabled();
  await expect(dialog.getByLabel('Assignees (optional)')).toBeDisabled();
  await expect(dialog).toContainText('requires Push access on GitHub');
  await dialog.getByRole('button', { name: 'Close issue editor' }).click();

  // Edit another user's issue (#12, authored by alex): all controls disabled with Triage reason.
  await page.getByRole('button', { name: 'Edit issue #12' }).click();
  dialog = page.getByRole('dialog', { name: 'Edit issue #12' });
  await expect(dialog).toContainText('requires Triage or higher permission on GitHub');
  await expect(dialog.getByLabel('Open / closed state')).toBeDisabled();
  await expect(dialog.getByLabel('Labels (add or remove)')).toBeDisabled();
  await expect(dialog.getByRole('button', { name: 'Review' })).toBeDisabled();
  await dialog.getByRole('button', { name: 'Close issue editor' }).click();

  // Edit viewer's own issue (#15, authored by ada): state is enabled, labels/assignees stay disabled.
  await page.getByRole('button', { name: 'Edit issue #15' }).click();
  dialog = page.getByRole('dialog', { name: 'Edit issue #15' });
  await expect(dialog).toContainText('With Read access you can close or reopen issues you authored');
  await expect(dialog.getByLabel('Open / closed state')).toBeEnabled();
  await expect(dialog.getByLabel('Labels (add or remove)')).toBeDisabled();
});

test('demo mode shows the issue editor with writes disabled and labeled', async ({ page }) => {
  await setupWorkspace(page, { session: demo });

  await page.goto('/workspace/1?tab=issues');
  await expect(page.getByRole('region', { name: 'Write history' })).toContainText('Demo mode — remote GitHub writes are disabled');

  await page.getByRole('region', { name: 'Repository issues' }).getByRole('button', { name: '+ New issue' }).click();
  const dialog = page.getByRole('dialog', { name: 'New issue' });
  await expect(dialog).toContainText('Demo mode — remote GitHub writes are disabled');
  await expect(dialog.getByLabel('Title')).toBeDisabled();
  await expect(dialog.getByLabel('Description')).toBeDisabled();
  await expect(dialog.getByRole('button', { name: 'Review' })).toBeDisabled();
});

test('write history supports inline recheck of unknown operations and follow-up edit of partially failed creates', async ({ page }) => {
  const ops: Record<string, unknown>[] = [
    {
      id: 901,
      repositoryId: 1,
      kind: 'create',
      state: 'unknown',
      issueNumber: null,
      draft: { title: 'Timeout create from earlier' },
      outcome: { error: 'no reliable answer from GitHub; the change may or may not have been applied' },
      actor: 'ada',
      searchUrl: 'https://github.com/ada/repo-1/issues?q=is%3Aissue+author%3Aada',
      createdAt: '2026-11-17T11:00:00Z',
      updatedAt: '2026-11-17T11:00:00Z',
    },
    {
      id: 902,
      repositoryId: 1,
      kind: 'create',
      state: 'failed',
      issueNumber: 45,
      draft: { title: 'Partial create', assignees: ['octocat'] },
      outcome: {
        issue: {
          number: 45,
          url: 'https://github.com/ada/repo-1/issues/45',
          title: 'Partial create',
          state: 'open',
          labels: [],
          assignees: [],
        },
        unapplied: ['add assignee octocat'],
      },
      actor: 'ada',
      createdAt: '2026-11-17T10:30:00Z',
      updatedAt: '2026-11-17T10:30:01Z',
    },
  ];

  await setupWorkspace(page, { operations: ops });
  await page.route('**/api/write-operations/901/recheck', async route => {
    const body = route.request().postDataJSON() as { issueNumber?: number };
    if (body.issueNumber !== 55) {
      return route.fulfill({ status: 409, json: ops[0] });
    }
    const resolved = {
      ...ops[0],
      state: 'succeeded',
      issueNumber: 55,
      searchUrl: undefined,
      outcome: {
        issue: {
          number: 55,
          url: 'https://github.com/ada/repo-1/issues/55',
          title: 'Timeout create from earlier',
          state: 'open',
          labels: [],
          assignees: [],
        },
      },
    };
    ops[0] = resolved;
    return route.fulfill({ status: 200, json: resolved });
  });

  await page.goto('/workspace/1?tab=issues');
  const history = page.getByRole('region', { name: 'Write history' });
  await expect(history).toContainText('Unapplied: add assignee octocat');

  // Inline recheck in Write history rejects mismatch and resolves on match.
  await history.getByLabel('Issue number for operation #901').fill('12');
  await history.getByRole('button', { name: 'Recheck' }).click();
  await expect(history.getByRole('alert')).toContainText('Issue #12 did not match this draft');

  await history.getByLabel('Issue number for operation #901').fill('55');
  await history.getByRole('button', { name: 'Recheck' }).click();
  await expect(history.getByRole('link', { name: /#55/ })).toHaveAttribute('href', 'https://github.com/ada/repo-1/issues/55');

  // Clicking "Edit issue #45" opens the update editor for that linked issue instead of creating a duplicate.
  await history.getByRole('button', { name: 'Edit issue #45' }).click();
  await expect(page.getByRole('dialog', { name: 'Edit issue #45' })).toBeVisible();
});

test('duplicate create 409 offers inspecting the existing unresolved operation without resending', async ({ page }) => {
  let createCalls = 0;
  const unresolvedOp = {
    id: 950,
    repositoryId: 1,
    kind: 'create',
    state: 'unknown',
    issueNumber: null,
    draft: { title: 'Existing unresolved issue' },
    outcome: { error: 'no reliable answer from GitHub; the change may or may not have been applied' },
    actor: 'ada',
    searchUrl: 'https://github.com/ada/repo-1/issues?q=is%3Aissue+author%3Aada',
    createdAt: '2026-11-17T11:30:00Z',
    updatedAt: '2026-11-17T11:30:00Z',
  };

  await setupWorkspace(page, { operations: [unresolvedOp] });
  await page.route('**/api/repositories/1/issues', async route => {
    createCalls++;
    return route.fulfill({
      status: 409,
      json: { error: 'an earlier create of this issue is still unresolved; recheck it instead of resubmitting', operationId: 950 },
    });
  });
  await page.route('**/api/write-operations/950', route => route.fulfill({ json: unresolvedOp }));

  await page.goto('/workspace/1');
  await page.getByRole('button', { name: '+ New issue' }).click();
  const dialog = page.getByRole('dialog', { name: 'New issue' });
  await dialog.getByLabel('Title').fill('Existing unresolved issue');
  await dialog.getByRole('button', { name: 'Review' }).click();
  await dialog.getByRole('button', { name: 'Submit to GitHub' }).click();

  await expect(dialog).toContainText('already recorded (operation #950)');
  await dialog.getByRole('button', { name: 'Inspect operation #950' }).click();
  await expect(dialog).toContainText('Operation #950 · unknown');
  await expect(dialog.getByRole('button', { name: 'Recheck' })).toBeVisible();
  expect(createCalls).toBe(1);
});

test('reload during create recovers with the original key after a server error', async ({ page }) => {
  await setupWorkspace(page);
  const keys: string[] = [];
  let releaseFirstRequest!: () => void;
  const firstRequest = new Promise<void>(resolve => { releaseFirstRequest = resolve; });
  await page.route('**/api/repositories/1/issues', async route => {
    keys.push(route.request().headers()['idempotency-key']);
    if (keys.length === 1) {
      await firstRequest;
      return;
    }
    await route.fulfill(keys.length === 2
      ? { status: 500, body: 'Temporary server error' }
      : { status: 503, body: 'GitHub access could not be verified' });
  });

  try {
    await page.goto('/workspace/1?editor=new');
    const dialog = page.getByRole('dialog', { name: 'New issue' });
    await dialog.getByLabel('Title').fill('Keep the original request');
    await dialog.getByRole('button', { name: 'Review' }).click();
    await dialog.getByRole('button', { name: 'Submit to GitHub' }).click();
    await expect.poll(() => keys.length).toBe(1);

    await page.reload();
    const recover = dialog.getByRole('button', { name: 'Check recorded status with same key' });
    await expect(recover).toBeVisible();
    expect(keys).toHaveLength(1);
    await recover.click();
    await expect(dialog.getByRole('status').filter({ hasText: 'Outcome unknown.' })).toContainText('Temporary server error');
    await recover.click();
    await expect(dialog.getByRole('status').filter({ hasText: 'Outcome unknown.' })).toContainText('GitHub access could not be verified');
    await expect(recover).toBeVisible();
    expect(keys).toEqual([keys[0], keys[0], keys[0]]);
  } finally {
    releaseFirstRequest();
  }
});

test('keeps a draft when access is unverified and clears it when access is lost', async ({ page }) => {
  await setupWorkspace(page);
  await page.goto('/workspace/1?editor=new');
  await page.getByRole('dialog', { name: 'New issue' }).getByLabel('Title').fill('Saved while GitHub is unavailable');
  const draftExists = () => page.evaluate(() =>
    Object.keys(localStorage).some(key => key.startsWith('repoorbit:issue-draft:')),
  );
  await expect.poll(draftExists).toBe(true);

  let denied = false;
  await page.route('**/graphql', route => {
    if (!String(route.request().postDataJSON().query).startsWith('query RepositoryOverview')) return route.fallback();
    return route.fulfill({
      json: {
        data: { repositoryOverview: null },
        ...(denied ? {} : { errors: [{ message: 'Rate limited', extensions: {
          code: 'ACCESS_UNVERIFIED', retryAt: new Date(Date.now() + 600_000).toISOString(),
        } }] }),
      },
    });
  });
  await page.reload();
  await expect(page.getByText(/GitHub limited access checks/i).first()).toBeVisible();
  expect(await draftExists()).toBe(true);

  denied = true;
  await page.reload();
  await expect.poll(draftExists).toBe(false);
});
