import { test, expect } from '@playwright/test';
test('shows database-backed demo repositories and issues', async ({ page }) => {
  await page.goto('http://127.0.0.1:3000');
  await expect(page.getByText('Sample data stored in the local database.', { exact: false })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'orbit/example' })).toBeVisible();
  await expect(page.getByText('Improve onboarding documentation')).toBeVisible();
});
test('distinguishes an empty dataset from an error', async ({ page }) => {
  await page.route('**/graphql', route => route.fulfill({ json: { data: { repositories: { nodes: [], endCursor: null, hasNextPage: false } } } }));
  await page.goto('http://127.0.0.1:3000');
  await expect(page.getByText('No demo repositories yet.')).toBeVisible();
  await expect(page.getByText('Could not load repositories.', { exact: false })).toHaveCount(0);
});
test('renders a repository inserted into the database', async ({ page }) => {
  const { execFileSync } = await import('node:child_process');
  const id = Date.now();
  const sql = (statement: string) => execFileSync('docker', ['compose', 'exec', '-T', 'db', 'psql', '-U', 'repoorbit', '-d', 'repoorbit', '-c', statement], { cwd: '../..' });
  try {
    sql(`INSERT INTO repositories (id, name, description, is_demo) VALUES (${id}, 'demo/db-change', 'Inserted by browser test', true)`);
    await page.goto('http://127.0.0.1:3000');
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
