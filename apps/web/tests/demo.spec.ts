import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';

// Needs the API in Demo mode (no GitHub credentials). E2E_COMPOSE selects the compose command, e.g. "docker compose -p u5a".
const compose = (process.env.E2E_COMPOSE ?? 'docker compose').split(' ');

test('shows database-backed demo repositories and issues', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByText('Sample data stored in the local database.', { exact: false })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'orbit/example' })).toBeVisible();
  await expect(page.getByText('Improve onboarding documentation')).toBeVisible();
});

test('distinguishes an empty dataset from an error', async ({ page }) => {
  await page.route('**/graphql', route => route.fulfill({ json: { data: { selectedRepositories: { nodes: [], endCursor: null, hasNextPage: false } } } }));
  await page.goto('/');
  await expect(page.getByText('No demo repositories yet.')).toBeVisible();
  await expect(page.getByText('Could not load repositories.', { exact: false })).toHaveCount(0);
});

test('renders a repository inserted into the database', async ({ page }) => {
  const id = Date.now();
  const sql = (statement: string) => execFileSync(compose[0], [...compose.slice(1), 'exec', '-T', 'db', 'psql', '-U', 'repoorbit', '-d', 'repoorbit', '-c', statement], { cwd: '../..' });
  try {
    sql(`INSERT INTO repositories (id, name, description, is_demo) VALUES (${id}, 'demo/db-change', 'Inserted by browser test', true)`);
    await page.goto('/');
    const heading = page.getByRole('heading', { name: 'demo/db-change' });
    const more = page.getByRole('button', { name: 'Load more repositories' });
    await expect(page.getByRole('heading', { name: 'orbit/example' })).toBeVisible();
    // The new id sorts last; page through in case the local volume holds 20+ repositories.
    while (!(await heading.isVisible()) && (await more.isVisible())) { await more.click(); await expect(page.getByRole('status')).toHaveCount(0); }
    await expect(heading).toBeVisible();
  } finally {
    sql(`DELETE FROM repositories WHERE id = ${id}`);
  }
});

// More than one page of repositories: the details query runs once per chunk, and each chunk must stay under the API's complexity budget.
test('pages through more than one full page of repositories', async ({ page }) => {
  const base = Date.now();
  const sql = (statement: string) => execFileSync(compose[0], [...compose.slice(1), 'exec', '-T', 'db', 'psql', '-U', 'repoorbit', '-d', 'repoorbit', '-c', statement], { cwd: '../..' });
  try {
    const values = Array.from({ length: 21 }, (_, i) => `(${base + i}, 'demo/bulk-${i}', 'Bulk browser test', true)`).join(', ');
    sql(`INSERT INTO repositories (id, name, description, is_demo) VALUES ${values}`);
    const detailResponses: { ids: string[]; ok: boolean; body: { errors?: unknown; data?: Record<string, unknown> } }[] = [];
    await page.route('**/graphql', async route => {
      const operation = route.request().postDataJSON();
      if (!operation.query.startsWith('query RepositoryDetails')) return route.continue();
      const response = await route.fetch();
      detailResponses.push({ ids: Object.values(operation.variables), ok: response.ok(), body: await response.json() });
      await route.fulfill({ response });
    });
    await page.goto('/');
    const more = page.getByRole('button', { name: 'Load more repositories' });
    const rows = page.getByRole('heading', { name: /^demo\/bulk-/ });
    await expect(rows.first()).toBeVisible();
    // Each click must add rows before the next one: the button is removed once the last page has loaded.
    for (let loaded = await rows.count(); loaded < 21; loaded = await rows.count()) {
      await more.click();
      await expect.poll(() => rows.count()).toBeGreaterThan(loaded);
    }
    await expect(rows).toHaveCount(21);
    const fullBatches = detailResponses.filter(response => response.ids.length === 8);
    expect(fullBatches.length).toBeGreaterThan(0);
    for (const response of fullBatches) {
      expect(response.ok).toBe(true);
      expect(response.body.errors).toBeUndefined();
      expect(Object.keys(response.body.data ?? {})).toHaveLength(16);
    }
    await expect(page.getByRole('heading', { name: 'demo/bulk-20', exact: true })).toBeVisible();
  } finally {
    sql(`DELETE FROM repositories WHERE id >= ${base} AND id < ${base + 21}`);
  }
});
