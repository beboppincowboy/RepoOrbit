import type { Page } from '@playwright/test';

// Shared API fixtures for browser tests that run without the Go API.
export type Session = { mode: 'demo' | 'real'; user: { login: string; demo: boolean } | null; csrfToken: string | null };
export const signedIn: Session = { mode: 'real', user: { login: 'ada', demo: false }, csrfToken: 't' };
export const signedOut: Session = { mode: 'real', user: null, csrfToken: 't' };
export const demo: Session = { mode: 'demo', user: { login: 'demo', demo: true }, csrfToken: 't' };

export const quietActivity = { synchronized: false, message: 'Activity import in progress; continuous reconciliation is not yet complete.', issues: null, pullRequests: null, releases: null, reviews: null, comments: null, issueEvents: null, futureResources: [] };

export const metadata = (over: object = {}) => ({ state: 'queued', lastSuccessAt: null, nextAttemptAt: null, paused: false, failureCode: null, followSuspended: false, ...over });

export function selectedRepo(id: string, over: object = {}) {
  return { id, fullName: `ada/repo-${id}`, description: 'Fixture repository', syncState: 'awaiting-sync', metadataStatus: null, activity: quietActivity, ...over };
}

export async function mockSession(page: Page, session: Session) {
  await page.route('**/auth/me', route => route.fulfill({ json: session }));
}

// Answers the selected-repositories query with `nodes` (each may carry an `activity` fixture), and the per-page detail
// query (activity per repository, issues per repository as r0, r1, ...) with `issues` applied to every repository.
export async function mockSelected(page: Page, nodes: Array<Record<string, unknown>>, issues: object = {}) {
  await page.route('**/graphql', async route => {
    const body = route.request().postDataJSON();
    if (String(body.query).startsWith('query RepositoryDetails')) {
      const data: Record<string, unknown> = {};
      // Details arrive in chunks, so each alias maps to the repository whose id the request passed as id0, id1, ...
      Object.values(body.variables as Record<string, string>).forEach((id, i) => {
        const node = nodes.find(n => n.id === id);
        data[`a${i}`] = { activity: node?.activity ?? null };
        data[`r${i}`] = { nodes: [], hasNextPage: false, withheld: 0, retryAt: null, ...issues };
      });
      return route.fulfill({ json: { data } });
    }
    const bare = nodes.map(node => { const copy = { ...node }; delete copy.activity; return copy; });
    return route.fulfill({ json: { data: { selectedRepositories: { nodes: bare, endCursor: null, hasNextPage: false, withheld: 0, retryAt: null } } } });
  });
}

export type ListedRepo = { id: string; fullName: string; description: string; private: boolean; selected: boolean; syncState: string };
export const listed = (id: string, over: Partial<ListedRepo> = {}): ListedRepo => ({ id, fullName: `ada/repo-${id}`, description: 'Fixture repository', private: false, selected: false, syncState: 'not-synchronized', ...over });

// Serves the owned list from `rows`. PUT/DELETE on a selection updates `rows`, like the server would; ids in `failIds` answer with that status instead.
export async function mockOwned(page: Page, rows: ListedRepo[], failIds: Record<string, number> = {}) {
  const writes: string[] = [];
  await page.route(/\/api\/repositories\/owned/, route => route.fulfill({ json: { repositories: rows.map(r => ({ ...r, syncState: r.selected ? 'awaiting-sync' : 'not-synchronized' })), nextPage: 0, refreshFailed: false, withheld: 0, retryAt: null, installUrl: '' } }));
  await page.route(/\/api\/repositories\/([^/?]+)\/selection$/, route => {
    const id = decodeURIComponent(route.request().url().match(/repositories\/([^/]+)\/selection$/)![1]);
    const method = route.request().method();
    writes.push(`${method} ${id}`);
    if (failIds[id]) return route.fulfill({ status: failIds[id], json: {} });
    const row = rows.find(r => r.id === id);
    if (row) row.selected = method === 'PUT';
    return route.fulfill({ status: 204, body: '' });
  });
  return writes;
}

// Serves the stored star snapshot. Following is a per-row flag here, like the owned list.
export async function mockStarred(page: Page, rows: ListedRepo[]) {
  await page.route(/\/api\/repositories\/starred/, route => route.fulfill({ json: { repositories: rows, nextPage: 0, refreshedAt: '2026-11-16T08:00:00Z', refreshFailed: false, withheld: 0, retryAt: null, installUrl: '' } }));
}
