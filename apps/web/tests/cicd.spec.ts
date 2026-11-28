import { expect, test, type Page } from '@playwright/test';
import { mockSelected, mockSession, selectedRepo, signedIn } from './mocks';

// Fully mocked GraphQL (no Go API or Demo DB): CI/CD delivery monitoring, plan U5-U6 and acceptance examples AE1-AE4.
const covered = { covered: true, reason: null, through: '2026-11-17T08:00:00Z' };
const denied = { covered: false, reason: 'permission_denied', through: null };
const stream = { phase: 'current', boundary: '2026-08-01T00:00:00Z', catchupThrough: null, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, jobState: 'queued', nextAttemptAt: '2026-11-17T09:00:00Z' };
const child = { complete: true, pendingChildren: 0, lastSuccessAt: '2026-11-17T08:00:00Z', failureCode: null, nextAttemptAt: null };
const repoOverview = {
  fullName: 'ada/repo-1', syncState: 'awaiting-sync',
  activity: { synchronized: true, message: '', archived: false, issues: stream, pullRequests: stream, releases: stream, reviews: child, comments: child, issueEvents: { traversal: stream, historicalGap: false }, futureResources: [] },
  openIssues: { value: 5, coverage: covered }, staleOpenIssues: { value: 0, coverage: covered }, pullRequestsMerged: { value: 1, coverage: covered }, openPullRequests: { value: 1, coverage: covered },
  firstResponseTime: { seconds: 3600, sampleSize: 2, coverage: covered }, mergeTime: { seconds: 7200, sampleSize: 1, coverage: covered }, issuesWithoutResponse: { value: 0 },
  eventsCoveredFrom: '2026-11-03T00:00:00Z', windowStart: '2026-10-19T00:00:00Z', activeBotContributors: { value: 0, coverage: covered }, contributorsCoverage: covered,
  contributors: [{ login: 'alex', isBot: false, pullRequestsOpened: 3, reviewsSubmitted: 2, commentsCreated: 4 }],
  timeline: [], daily: [],
};

const run = (id: string, workflowName: string, status: string, conclusion: string, runNumber: number) => ({
  id, workflowId: `wf-${workflowName}`, workflowName, runNumber, event: 'push', status, conclusion, headBranch: 'main', headSha: 'a1b2c3d4e5f6', htmlUrl: `https://github.com/ada/repo-1/actions/runs/${id}`,
  createdAt: '2026-11-20T10:00:00Z', runStartedAt: '2026-11-20T10:00:30Z', updatedAt: '2026-11-20T10:05:00Z', durationSeconds: status === 'completed' ? 270 : null, queueSeconds: 30,
});
const noMedian = { seconds: null, sampleSize: null, coverage: covered };
const actions = (over: Record<string, unknown> = {}) => ({
  days: 30, windowStart: '2026-10-19T00:00:00Z', now: '2026-11-20T12:00:00Z',
  actionsCoverage: covered, deploymentsCoverage: covered,
  buildSuccessRate: { rate: null, numerator: null, denominator: null, coverage: covered },
  failedJobs: { value: 0, coverage: covered }, runningJobs: { value: 0, coverage: covered },
  durationP50: noMedian, durationP95: noMedian, queueWaitP50: noMedian,
  daily: [{ day: '2026-11-19T00:00:00Z', succeeded: 3, failed: 1, other: 2 }, { day: '2026-11-20T00:00:00Z', succeeded: 0, failed: 0, other: 0 }],
  failuresByWorkflow: [], recentRuns: [], deployments: [], deploymentUnavailable: true,
  ...over,
});
// AE2: three succeeded, one failed and two still running.
const ae2Runs = [
  run('9001', 'CI', 'completed', 'failure', 41), run('9002', 'CI', 'completed', 'success', 40), run('9003', 'Lint', 'completed', 'success', 12),
  run('9004', 'Docs', 'completed', 'success', 7), run('9005', 'CI', 'in_progress', '', 42), run('9006', 'Lint', 'queued', '', 13),
];
const ae2 = actions({
  buildSuccessRate: { rate: 0.75, numerator: 3, denominator: 4, coverage: covered },
  failedJobs: { value: 1, coverage: covered }, runningJobs: { value: 2, coverage: covered },
  durationP50: { seconds: 270, sampleSize: 4, coverage: covered }, durationP95: { seconds: 300, sampleSize: 4, coverage: covered }, queueWaitP50: { seconds: 30, sampleSize: 6, coverage: covered },
  failuresByWorkflow: [{ workflowId: 'wf-CI', workflowName: 'CI', failedRuns: 1, totalRuns: 3, latestRunId: '9001', latestStatus: 'completed', latestConclusion: 'failure', latestAt: '2026-11-20T10:05:00Z', htmlUrl: 'https://github.com/ada/repo-1/actions/runs/9001' }],
  recentRuns: ae2Runs,
});

const detail = (id = '9001') => ({
  run: run(id, 'CI', 'completed', 'failure', 41),
  jobs: [
    { id: '7001', runId: id, name: 'lint', status: 'completed', conclusion: 'success', htmlUrl: 'https://github.com/ada/repo-1/actions/runs/9001/job/7001', createdAt: '2026-11-20T10:00:00Z', startedAt: '2026-11-20T10:00:30Z', completedAt: '2026-11-20T10:02:00Z', durationSeconds: 90, queueSeconds: 30 },
    { id: '7002', runId: id, name: 'integration (ubuntu)', status: 'completed', conclusion: 'failure', htmlUrl: 'https://github.com/ada/repo-1/actions/runs/9001/job/7002', createdAt: '2026-11-20T10:00:00Z', startedAt: '2026-11-20T10:00:30Z', completedAt: '2026-11-20T10:05:00Z', durationSeconds: 270, queueSeconds: 30 },
  ],
});

const crossOverview = {
  days: 30, windowStart: '2026-10-18T00:00:00Z', now: '2026-11-17T00:00:00Z',
  repositoriesCounted: 1, repositoriesSyncing: 0, withheld: 0, retryAt: null,
  openIssues: { value: 5, coverage: covered }, reviewBacklog: { value: 0, coverage: covered },
  failingWorkflows: { value: 1, coverage: covered },
  buildSuccessRate: { rate: 0.75, numerator: 3, denominator: 4, coverage: covered },
  needsAttention: [{ kind: 'failed_workflow_run', repositoryId: '1', fullName: 'ada/repo-1', number: 41, title: 'CI', url: 'https://github.com/ada/repo-1/actions/runs/9001', since: '2026-11-20T10:05:00Z', runId: '9001' }],
  assignedToMe: [], daily: [],
};

const issue = (number: number, title: string, over: Record<string, unknown> = {}) => ({
  number, title, url: `https://github.com/ada/repo-1/issues/${number}`, state: 'open', updatedAt: '2026-11-20T10:00:00Z',
  authorLogin: 'alex', labels: [] as string[], assignees: [] as string[], status: 'backlog', multipleStatus: false, ...over,
});
const boardIssues = [issue(1, 'Plan the roadmap'), issue(3, 'Write the importer', { labels: ['status:todo'], assignees: ['alex'], status: 'todo' })];

// AE1: issue #3 has a confirmed linked pull request with a failed Integration Tests check and no GitHub Deployment records.
const delivery = (over: Record<string, unknown> = {}, pr: Record<string, unknown> = {}) => ({
  issueNumber: 3, actionsCoverage: covered, deploymentsCoverage: covered, deploymentUnavailable: true,
  pullRequests: [{
    number: 42, title: 'Add importer retries', state: 'open', url: 'https://github.com/ada/repo-1/pull/42', reviewState: 'changes_requested', headSha: 'a1b2c3d4e5f6',
    checks: [
      { runId: '9001', workflowName: 'Lint', status: 'completed', conclusion: 'success', htmlUrl: 'https://github.com/ada/repo-1/actions/runs/9003', updatedAt: '2026-11-20T10:05:00Z', failingJobName: null, failingJobUrl: null },
      { runId: '9001', workflowName: 'Integration Tests', status: 'completed', conclusion: 'failure', htmlUrl: 'https://github.com/ada/repo-1/actions/runs/9001', updatedAt: '2026-11-20T10:05:00Z', failingJobName: 'integration (ubuntu)', failingJobUrl: 'https://github.com/ada/repo-1/actions/runs/9001/job/7002' },
    ],
    deployments: [], ...pr,
  }],
  ...over,
});

type Fixtures = { actions?: object; delivery?: object | null };
async function setup(page: Page, fixtures: Fixtures = {}) {
  await mockSession(page, signedIn);
  await mockSelected(page, [selectedRepo('1')]);
  await page.route('**/graphql', route => {
    const { query, variables } = route.request().postDataJSON();
    const name = String(query).match(/^query (\w+)/)?.[1];
    switch (name) {
      case 'RepositoryOverview': return route.fulfill({ json: { data: { repositoryOverview: repoOverview } } });
      case 'CrossRepositoryOverview': return route.fulfill({ json: { data: { crossRepositoryOverview: crossOverview } } });
      case 'RepositoryActions': return route.fulfill({ json: { data: { repositoryActions: fixtures.actions ?? ae2 } } });
      case 'WorkflowRun': return route.fulfill({ json: { data: { workflowRun: detail(variables.runId) } } });
      case 'IssueDelivery': return route.fulfill({ json: { data: { issueDelivery: fixtures.delivery === undefined ? delivery() : fixtures.delivery } } });
      case 'RepositoryBoard': return route.fulfill({ json: { data: { repositoryBoard: { statusLabels: true, truncated: false, issues: boardIssues } } } });
      default: return route.fallback();
    }
  });
  await page.route('**/api/repositories/1/issue-capabilities', route => route.fulfill({ json: { canCreateIssue: true, canCreateIssueWithMetadata: true, canManageIssues: true } }));
  await page.route('**/api/repositories/1/assignees', route => route.fulfill({ json: { assignees: ['ada', 'alex'] } }));
  await page.route('**/api/repositories/1/status-labels', route => route.fulfill({ json: { enabled: true, canChange: false } }));
}

const noWriteControls = /re-?run|cancel/i;

test.beforeEach(async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
});

test('engineering overview shows CI/CD tiles and a failed run row that opens the read-only run drawer', async ({ page }) => {
  await setup(page);
  await page.goto('/');
  const totals = page.getByRole('region', { name: 'Totals' });
  await expect(totals.getByText('Failing workflows', { exact: true })).toBeVisible();
  await expect(totals.getByText('Build success', { exact: true })).toBeVisible();
  await expect(totals.getByText('75.0%')).toBeVisible();
  await expect(totals.getByText('3 of 4 completed')).toBeVisible();

  const row = page.getByRole('region', { name: 'Needs attention' }).getByRole('link', { name: /repo-1 · Run #41/ });
  await expect(row).toContainText('Workflow failed');
  await row.click();
  await expect(page).toHaveURL(/\/workspace\/1\?.*tab=cicd/);
  await expect(page).toHaveURL(/run=(%22)?9001/);
  const drawer = page.getByRole('dialog', { name: 'Workflow run detail' });
  await expect(drawer).toBeVisible();
  await expect(drawer.getByRole('heading', { name: 'CI' })).toBeVisible();
  await expect(drawer.getByText('integration (ubuntu)')).toBeVisible();
  await expect(drawer.getByRole('link', { name: /View logs on GitHub/ }).nth(1)).toHaveAttribute('href', 'https://github.com/ada/repo-1/actions/runs/9001/job/7002');
  await expect(drawer.getByRole('button', { name: noWriteControls })).toHaveCount(0);
  await expect(drawer.getByRole('link', { name: noWriteControls })).toHaveCount(0);

  await drawer.getByRole('button', { name: 'Close run detail' }).click();
  await expect(drawer).toHaveCount(0);
  await expect(page).not.toHaveURL(/run=/);
  await expect(page).toHaveURL(/tab=cicd/);
});

test('AE2: three succeeded, one failed and two in progress report 75.0% of completed runs and 2 running separately', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=cicd');
  const kpis = page.getByRole('region', { name: 'CI/CD KPIs' });
  await expect(kpis.getByText('75.0%')).toBeVisible();
  await expect(kpis.getByText('3 of 4 completed')).toBeVisible();
  await expect(kpis.getByText('1 failed')).toBeVisible();
  await expect(kpis.getByText('2 running')).toBeVisible();
  await expect(page.getByRole('region', { name: 'Runs over time' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Failures by workflow' }).getByRole('button', { name: 'CI' })).toBeVisible();
  const recent = page.getByRole('region', { name: 'Recent workflow runs' }).getByRole('listitem');
  await expect(recent).toHaveCount(6);
  await expect(recent.filter({ hasText: 'in progress' })).toHaveCount(1);
  await expect(recent.filter({ hasText: 'queued' })).toHaveCount(1);
  await expect(recent.filter({ hasText: 'success' })).toHaveCount(3);
});

test('selecting a run opens the drawer, keeps it in the URL, and switching tab clears it', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=cicd');
  await page.getByRole('region', { name: 'Recent workflow runs' }).getByRole('button', { name: /CI #41/ }).click();
  await expect(page).toHaveURL(/run=(%22)?9001/);
  await expect(page.getByRole('dialog', { name: 'Workflow run detail' })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('dialog', { name: 'Workflow run detail' })).toBeVisible();
  await page.getByRole('navigation', { name: 'Workspace sections' }).getByRole('link', { name: 'Overview' }).click();
  await expect(page.getByRole('dialog', { name: 'Workflow run detail' })).toHaveCount(0);
  await expect(page).not.toHaveURL(/run=/);
  // An invalid run id is ignored instead of opening a drawer.
  await page.goto('/workspace/1?tab=cicd&run=abc');
  await expect(page.getByRole('region', { name: 'CI/CD KPIs' })).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Workflow run detail' })).toHaveCount(0);
});

test('switching to the 7-day window requests seven days of CI/CD data', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=cicd');
  await expect(page.getByRole('region', { name: 'CI/CD KPIs' })).toBeVisible();
  const request = page.waitForRequest(r => r.url().endsWith('/graphql') && /^query RepositoryActions\b/.test(r.postDataJSON().query) && r.postDataJSON().variables.days === 7);
  await page.getByRole('group', { name: 'Time window' }).getByRole('link', { name: '7d' }).click();
  await request;
  await expect(page.getByRole('region', { name: 'Runs over time' })).toContainText('last 7 days');
});

test('AE3: permission denied shows the permission notice on CI/CD while other tabs keep working', async ({ page }) => {
  const unavailable = { covered: false, reason: 'permission_denied', through: null };
  await setup(page, {
    actions: actions({
      actionsCoverage: denied, deploymentsCoverage: denied,
      buildSuccessRate: { rate: null, numerator: null, denominator: null, coverage: unavailable },
      failedJobs: { value: null, coverage: unavailable }, runningJobs: { value: null, coverage: unavailable },
      durationP50: { seconds: null, sampleSize: null, coverage: unavailable }, durationP95: { seconds: null, sampleSize: null, coverage: unavailable }, queueWaitP50: { seconds: null, sampleSize: null, coverage: unavailable },
      daily: [],
    }),
  });
  await page.goto('/workspace/1?tab=cicd');
  await expect(page.getByText('CI/CD data unavailable: missing GitHub Actions read permission.')).toBeVisible();
  await expect(page.getByRole('region', { name: 'CI/CD KPIs' }).getByText('Permission denied').first()).toBeVisible();
  await expect(page.getByRole('region', { name: 'CI/CD KPIs' }).getByText('0%')).toHaveCount(0);

  const nav = page.getByRole('navigation', { name: 'Workspace sections' });
  await nav.getByRole('link', { name: 'Overview' }).click();
  await expect(page.getByRole('group', { name: 'Open issues' })).toBeVisible();
  await nav.getByRole('link', { name: 'Board' }).click();
  await expect(page.locator('[data-issue-card="3"]')).toBeVisible();
});

test('AE4: a passing Deploy workflow with no GitHub Deployments shows Deployment data unavailable', async ({ page }) => {
  await setup(page, {
    actions: actions({
      buildSuccessRate: { rate: 1, numerator: 1, denominator: 1, coverage: covered },
      recentRuns: [run('9100', 'Deploy', 'completed', 'success', 5)],
      deployments: [], deploymentUnavailable: true,
    }),
  });
  await page.goto('/workspace/1?tab=cicd');
  await expect(page.getByRole('region', { name: 'Recent workflow runs' }).getByText('Deploy')).toBeVisible();
  const deployments = page.getByRole('region', { name: 'Deployments' });
  await expect(deployments.getByText('Deployment data unavailable')).toBeVisible();
  await expect(deployments.getByRole('listitem')).toHaveCount(0);
});

test('Deployments lists GitHub Deployment records when they exist', async ({ page }) => {
  await setup(page, {
    actions: actions({
      deploymentUnavailable: false,
      deployments: [{ id: '5001', environment: 'production', sha: 'a1b2c3d4e5f6', ref: 'main', state: 'success', description: 'Deploy to prod', htmlUrl: 'https://github.com/ada/repo-1/deployments/5001', createdAt: '2026-11-20T11:00:00Z', updatedAt: '2026-11-20T11:05:00Z' }],
    }),
  });
  await page.goto('/workspace/1?tab=cicd');
  const deployments = page.getByRole('region', { name: 'Deployments' });
  await expect(deployments.getByText('production')).toBeVisible();
  await expect(deployments.getByText('Deployment data unavailable')).toHaveCount(0);
});

test('AE1: the Board task drawer shows the linked PR, review state, failed check with job logs, and unavailable deployments', async ({ page }) => {
  await setup(page);
  await page.goto('/workspace/1?tab=board');
  await page.locator('[data-issue-card="3"]').getByRole('button').click();
  const drawer = page.getByRole('dialog', { name: 'Issue #3' });
  const lifecycle = drawer.getByRole('region', { name: 'Delivery lifecycle' });
  await expect(lifecycle.getByRole('link', { name: 'PR #42' })).toHaveAttribute('href', 'https://github.com/ada/repo-1/pull/42');
  await expect(lifecycle.getByText('Add importer retries')).toBeVisible();
  await expect(lifecycle.getByText('Changes requested')).toBeVisible();
  const checks = lifecycle.getByRole('list', { name: 'Checks for PR #42' });
  await expect(checks.getByText('Lint')).toBeVisible();
  const failed = checks.getByRole('listitem').filter({ hasText: 'Integration Tests' });
  await expect(failed).toContainText('failure');
  await expect(failed).toContainText('integration (ubuntu)');
  await expect(failed.getByRole('link', { name: /View logs on GitHub/ })).toHaveAttribute('href', 'https://github.com/ada/repo-1/actions/runs/9001/job/7002');
  await expect(lifecycle.getByText('Deployment data unavailable')).toBeVisible();
  await expect(lifecycle.getByRole('button', { name: noWriteControls })).toHaveCount(0);

  // The failed check opens the CI/CD run detail.
  await failed.getByRole('button', { name: 'Open run detail' }).click();
  await expect(page).toHaveURL(/tab=cicd/);
  await expect(page).toHaveURL(/run=(%22)?9001/);
  await expect(page).not.toHaveURL(/issue=/);
  await expect(page.getByRole('dialog', { name: 'Workflow run detail' })).toBeVisible();
});

test('the task drawer lists PR deployments, and rejects non-GitHub log links', async ({ page }) => {
  const pr = {
    checks: [{ runId: '9001', workflowName: 'Integration Tests', status: 'completed', conclusion: 'failure', htmlUrl: 'https://example.com/run', updatedAt: '2026-11-20T10:05:00Z', failingJobName: 'integration (ubuntu)', failingJobUrl: 'https://example.com/job' }],
    deployments: [{ id: '5001', environment: 'staging', sha: 'a1b2c3d4e5f6', ref: 'main', state: 'success', description: '', htmlUrl: 'https://github.com/ada/repo-1/deployments/5001', createdAt: '2026-11-20T11:00:00Z', updatedAt: '2026-11-20T11:05:00Z' }],
  };
  await setup(page, { delivery: delivery({ deploymentUnavailable: false }, pr) });
  await page.goto('/workspace/1?tab=board&issue=3');
  const lifecycle = page.getByRole('dialog', { name: 'Issue #3' }).getByRole('region', { name: 'Delivery lifecycle' });
  await expect(lifecycle.getByRole('list', { name: 'Deployments for PR #42' }).getByText('staging')).toBeVisible();
  await expect(lifecycle.getByText('Deployment data unavailable')).toHaveCount(0);
  await expect(lifecycle.getByText('integration (ubuntu)')).toBeVisible();
  await expect(lifecycle.getByRole('link', { name: /View logs on GitHub/ })).toHaveCount(0);
  await expect(lifecycle.getByRole('link', { name: /View run on GitHub/ })).toHaveAttribute('href', 'https://github.com/ada/repo-1/actions/runs/9001');
});

test('the task drawer shows the permission notice when Actions read permission is missing', async ({ page }) => {
  await setup(page, { delivery: delivery({ actionsCoverage: denied, deploymentsCoverage: denied }, { checks: [] }) });
  await page.goto('/workspace/1?tab=board&issue=3');
  const lifecycle = page.getByRole('dialog', { name: 'Issue #3' }).getByRole('region', { name: 'Delivery lifecycle' });
  await expect(lifecycle.getByText('CI/CD data unavailable: missing GitHub Actions read permission.')).toBeVisible();
  await expect(lifecycle.getByRole('link', { name: 'PR #42' })).toBeVisible();
  await expect(lifecycle.getByRole('button', { name: noWriteControls })).toHaveCount(0);
});

test('an issue with no confirmed linked pull requests says so', async ({ page }) => {
  await setup(page, { delivery: delivery({ pullRequests: [] }) });
  await page.goto('/workspace/1?tab=board&issue=3');
  const lifecycle = page.getByRole('dialog', { name: 'Issue #3' }).getByRole('region', { name: 'Delivery lifecycle' });
  await expect(lifecycle.getByText('No confirmed GitHub-linked pull requests.')).toBeVisible();
});
