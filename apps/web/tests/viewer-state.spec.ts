import { test, expect } from '@playwright/test';
import { QueryClient } from '@tanstack/react-query';
import { logout, type ViewerSession } from '../src/lib/api';
import { issueDraftStorageKey } from '../src/lib/drafts';
import { currentAccessSeq, recordAccessDenied, recordAccessUnverified, recordAccessVerified } from '../src/lib/selected-repositories';
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

test('logout removes every saved issue draft and keeps unrelated storage', () => withWindow(async storage => {
  storage.setItem(issueDraftStorageKey('real:ada', '1', 'create'), '{}');
  storage.setItem(issueDraftStorageKey('real:ada', '2', { board: 7 }), '{}');
  storage.setItem('unrelated', 'keep');
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async () => new Response(null, { status: 204 })) as typeof fetch;
  try {
    await logout();
  } finally {
    globalThis.fetch = originalFetch;
  }
  expect(Object.keys(storage).filter(key => key.startsWith('repoorbit:issue-draft:'))).toEqual([]);
  expect(storage.getItem('unrelated')).toBe('keep');
}));
