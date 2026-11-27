import { test, expect } from '@playwright/test';
import { QueryClient } from '@tanstack/react-query';
import { logout, sessionQuery, type ViewerSession } from '../src/features/auth/api';
import { issueDraftStorageKey } from '../src/features/issues/drafts';
import { repositorySnapshotKey, selectedRepositoriesKey } from '../src/features/repositories/query-keys';
import { currentAccessSeq, recordAccessDenied, recordAccessUnverified, recordAccessVerified, resetViewerSelection } from '../src/features/repositories/access-store';
import { mockWindowStorage } from './mocks';

const session: ViewerSession = { mode: 'real', user: { login: 'ada', demo: false }, csrfToken: null };

async function withWindow<T>(fn: (storage: Storage) => Promise<T> | T): Promise<T> {
  const { storage, restore } = mockWindowStorage();
  try {
    return await fn(storage);
  } finally {
    restore();
  }
}

test.describe('access outcome ordering', () => {
  test('a verified read started before a repeated denial is rejected; one started after it is accepted', () => withWindow(() => {
    const qc = new QueryClient();
    recordAccessDenied(qc, session, 'r-denied');
    const startedBetween = currentAccessSeq();
    recordAccessDenied(qc, session, 'r-denied');
    expect(recordAccessVerified(session, 'r-denied', startedBetween)).toBe(false);
    expect(recordAccessVerified(session, 'r-denied', currentAccessSeq())).toBe(true);
  }));

  test('a verified read started before a repeated identical unverified outcome is rejected', () => withWindow(() => {
    const qc = new QueryClient();
    const retryAt = '2026-11-25T00:00:00Z';
    recordAccessUnverified(qc, session, 'r-unverified', retryAt, { preserveSnapshot: true });
    const startedBetween = currentAccessSeq();
    recordAccessUnverified(qc, session, 'r-unverified', retryAt, { preserveSnapshot: true });
    expect(recordAccessVerified(session, 'r-unverified', startedBetween)).toBe(false);
    expect(recordAccessVerified(session, 'r-unverified', currentAccessSeq())).toBe(true);
  }));
});

test('resetting a viewer selection forgets their access outcomes and cached data but keeps another viewer\'s', () => withWindow(() => {
  const qc = new QueryClient();
  const resetting: ViewerSession = { mode: 'real', user: { login: 'grace', demo: false }, csrfToken: null };
  const other: ViewerSession = { mode: 'real', user: { login: 'linus', demo: false }, csrfToken: null };
  const keys = (viewer: ViewerSession) => [
    selectedRepositoriesKey(viewer),
    [...selectedRepositoriesKey(viewer), 'metadata'],
    repositorySnapshotKey(viewer, 'r-open', 7),
    repositorySnapshotKey(viewer, 'r-open', 30),
  ];
  const startedBeforeDenial = currentAccessSeq();
  recordAccessDenied(qc, resetting, 'r-reset');
  recordAccessDenied(qc, other, 'r-reset');
  expect(recordAccessVerified(resetting, 'r-reset', startedBeforeDenial)).toBe(false);
  for (const key of [...keys(resetting), ...keys(other)]) qc.setQueryData(key, { cached: true });

  resetViewerSelection(qc, resetting);

  expect(recordAccessVerified(resetting, 'r-reset', startedBeforeDenial)).toBe(true);
  expect(recordAccessVerified(other, 'r-reset', startedBeforeDenial)).toBe(false);
  for (const key of keys(resetting)) expect(qc.getQueryData(key)).toBeUndefined();
  for (const key of keys(other)) expect(qc.getQueryData(key)).toEqual({ cached: true });
}));

test('logout removes every saved issue draft and keeps unrelated storage', () => withWindow(async storage => {
  storage.setItem(issueDraftStorageKey('real:ada', '1', 'create'), '{}');
  storage.setItem(issueDraftStorageKey('real:ada', '2', { board: 7 }), '{}');
  storage.setItem('unrelated', 'keep');
  const originalFetch = globalThis.fetch;
  const requests: { url: string; init: RequestInit | undefined }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    requests.push({ url: String(input), init });
    if (String(input) === '/auth/me') return Response.json({ ...session, csrfToken: 'session-token' });
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  try {
    await sessionQuery.queryFn();
    await logout();
  } finally {
    globalThis.fetch = originalFetch;
  }
  expect(requests.map(request => request.url)).toEqual(['/auth/me', '/auth/logout']);
  expect(requests[1].init).toEqual({ method: 'POST', headers: { 'X-CSRF-Token': 'session-token' } });
  expect(Object.keys(storage)).toEqual(['unrelated']);
  expect(storage.getItem('unrelated')).toBe('keep');
}));
