import { test, expect } from '@playwright/test';
import { IssueWriteError, type IssueUpdateDraft } from '../src/lib/api';
import type { BoardCard } from '../src/lib/board';
import { recoveryAction, runRecovery } from '../src/lib/board-recovery';

const now = Date.parse('2026-11-25T12:00:00Z');
const day = 24 * 60 * 60 * 1000;
const draft: IssueUpdateDraft = { labels: { add: ['status:in-progress'], remove: ['status:todo'] } };

const unknown = (over: Partial<BoardCard> = {}): BoardCard => ({
  version: 1,
  number: 3,
  snapshot: { state: 'open', labels: ['status:todo'], assignees: ['sam'], status: 'todo', updatedAt: '2026-11-20T10:00:00Z' },
  target: { status: 'in_progress', assigneesAdd: [], assigneesRemove: [] },
  phase: 'unknown',
  request: { draft, idempotencyKey: 'ro-stored' },
  operationId: null,
  attemptedAt: new Date(now - day).toISOString(),
  ...over,
});

// Fakes for both server calls that record every use and fail the test's expectations if a call was not wanted.
function fakes(submitError: unknown = new Error('unexpected send')) {
  const sends: unknown[][] = [];
  const rechecks: unknown[][] = [];
  return {
    sends,
    rechecks,
    options: {
      repositoryId: 'repo-1',
      now,
      submit: async (...args: unknown[]): Promise<never> => { sends.push(args); throw submitError; },
      recheck: async (...args: unknown[]): Promise<never> => { rechecks.push(args); throw new Error('unexpected recheck'); },
    },
  };
}

test.describe('board recovery', () => {
  test('Happy path: an unknown card with an operation id offers recheck', () => {
    expect(recoveryAction(unknown({ operationId: 41 }), now)).toBe('recheck');
    // An operation can be rechecked however old the send is.
    expect(recoveryAction(unknown({ operationId: 41, attemptedAt: new Date(now - 400 * day).toISOString() }), now)).toBe('recheck');
  });

  test('Happy path: an unknown card with no operation id and an attemptedAt inside the window offers check-recorded-status', () => {
    expect(recoveryAction(unknown(), now)).toBe('check-recorded-status');
  });

  test('Edge: an unknown card with no operation id and an attemptedAt outside the window offers none, and the helper returns no resend action', async () => {
    const card = unknown({ attemptedAt: new Date(now - 31 * day).toISOString() });
    expect(recoveryAction(card, now)).toBe('none');
    const { sends, rechecks, options } = fakes();
    expect(await runRecovery(card, options)).toBeNull();
    expect(sends).toEqual([]);
    expect(rechecks).toEqual([]);
  });

  test('Edge: a card whose phase is not unknown offers none', async () => {
    for (const phase of ['staged', 'changed', 'pending', 'failed', 'confirmed'] as const) {
      for (const operationId of [null, 41]) {
        const card = unknown({ phase, operationId });
        expect(recoveryAction(card, now)).toBe('none');
        const { sends, rechecks, options } = fakes();
        expect(await runRecovery(card, options)).toBeNull();
        expect([...sends, ...rechecks]).toEqual([]);
      }
    }
  });

  test('Edge: an unknown card with a null attemptedAt offers none', () => {
    expect(recoveryAction(unknown({ attemptedAt: null }), now)).toBe('none');
  });

  test('Error path: a resend that fails with a thrown error is mapped through applySaveResult with isReplay: true', async () => {
    // On a first send this refusal would fail the card and drop its request. On a replay it proves nothing about the
    // first send, so the card stays unknown.
    const refusal = new IssueWriteError('Permission refused.', { status: 403, kind: 'forbidden', disposition: 'not-recorded' });
    const card = unknown();
    const { sends, options } = fakes(refusal);
    expect(await runRecovery(card, options)).toEqual({ outcome: { kind: 'card', card }, error: 'Permission refused.' });
    expect(sends).toEqual([['repo-1', 3, 'ro-stored', draft]]);
  });
});
