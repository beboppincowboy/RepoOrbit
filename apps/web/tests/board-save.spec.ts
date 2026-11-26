import { test, expect } from '@playwright/test';
import { IssueWriteError, recheckWriteOperation, submitIssueUpdate, type IssueUpdateDraft, type WriteOperation } from '../src/lib/api';
import { loadBoardCards, removeBoardCard, saveBoardCard, type BoardCard } from '../src/lib/board';
import { saveBoardChanges, type BoardSaveOptions } from '../src/lib/board-save';

const viewer = 'user:ada';
const repo = 'repo-1';
const ada = { login: 'ada', assignable: true };

const staged = (number: number): BoardCard => ({
  version: 1,
  number,
  snapshot: { state: 'open', labels: ['status:todo'], assignees: ['sam'], status: 'todo', updatedAt: '2026-11-20T10:00:00Z' },
  target: { status: 'in_progress', assigneesAdd: [], assigneesRemove: [] },
  phase: 'staged',
  request: null,
  operationId: null,
});

const operation = (issueNumber: number, state: WriteOperation['state']): WriteOperation => ({
  id: 40 + issueNumber, repositoryId: 1, kind: 'update', state, issueNumber, draft: null, outcome: null, actor: 'ada',
  createdAt: '2026-11-25T10:00:00Z', updatedAt: '2026-11-25T10:00:00Z',
});

const stored = (number: number) => loadBoardCards(viewer, repo).find(card => card.number === number);
// Stores the cards and returns them as the review would have captured them.
function review(...numbers: number[]): BoardCard[] {
  for (const number of numbers) saveBoardCard(viewer, repo, staged(number));
  return loadBoardCards(viewer, repo);
}

type Send = { number: number; key: string; draft: IssueUpdateDraft };
// A fake submitIssueUpdate that records each send and answers with respond, or with a succeeded operation.
function fakeSubmit(respond: (number: number) => WriteOperation | Promise<WriteOperation> = number => operation(number, 'succeeded')) {
  const sends: Send[] = [];
  const submit: NonNullable<BoardSaveOptions['submit']> = async (_repositoryId, number, key, draft) => {
    sends.push({ number, key, draft });
    return respond(number);
  };
  return { sends, submit };
}
const save = (reviewed: BoardCard[], submit: BoardSaveOptions['submit'], onChange: () => void = () => {}) =>
  saveBoardChanges({ viewerKey: viewer, repositoryId: repo, reviewed, saver: ada, onChange, submit });

test.describe('saveBoardChanges', () => {
  const originalWindow = globalThis.window;
  let refuseWrites = false;
  let dropWrites = false;

  test.beforeEach(() => {
    refuseWrites = false;
    dropWrites = false;
    // Stored entries are own properties, as in the browser, so Object.keys lists them.
    const localStorage = Object.create({
      getItem(this: Record<string, string>, key: string): string | null {
        return Object.prototype.hasOwnProperty.call(this, key) ? this[key] : null;
      },
      setItem(this: Record<string, string>, key: string, value: string): void {
        if (refuseWrites) throw new Error('QuotaExceededError');
        if (!dropWrites) this[key] = String(value);
      },
      removeItem(this: Record<string, string>, key: string): void {
        delete this[key];
      },
    }) as Storage;
    globalThis.window = { localStorage } as unknown as Window & typeof globalThis;
  });

  test.afterEach(() => {
    if (originalWindow === undefined) Reflect.deleteProperty(globalThis, 'window');
    else globalThis.window = originalWindow;
  });

  test('happy path: two staged cards are sent one at a time in issue-number order and both leave the staged list', async () => {
    const reviewed = review(12, 3).reverse();
    let inFlight = 0;
    let most = 0;
    const { sends, submit } = fakeSubmit(async number => {
      most = Math.max(most, ++inFlight);
      await new Promise(resolve => setTimeout(resolve, 5));
      inFlight--;
      return operation(number, 'succeeded');
    });
    await save(reviewed, submit);
    expect(sends.map(send => send.number)).toEqual([3, 12]);
    expect(most).toBe(1);
    expect(new Set(sends.map(send => send.key)).size).toBe(2);
    expect(sends[0].draft).toEqual({
      labels: { add: ['status:in-progress'], remove: ['status:todo'] },
      expect: { status: 'todo', assignees: ['sam'], statusLabels: ['status:todo'] },
    });
    expect(loadBoardCards(viewer, repo)).toEqual([]);
  });

  test('error path: a card that was not recorded is failed with its request cleared and the next card still runs', async () => {
    const { sends, submit } = fakeSubmit(number => {
      if (number === 3) throw new IssueWriteError('refused', { status: 403, kind: 'forbidden', disposition: 'not-recorded' });
      return operation(number, 'succeeded');
    });
    await save(review(3, 12), submit);
    expect(sends.map(send => send.number)).toEqual([3, 12]);
    expect(loadBoardCards(viewer, repo)).toEqual([{ ...staged(3), phase: 'failed', attemptedAt: null }]);
  });

  test('error path: a send with no reliable answer leaves the card unknown with its request, key and attemptedAt', async () => {
    const { sends, submit } = fakeSubmit(() => { throw new IssueWriteError('timeout', { status: 504, kind: 'transport' }); });
    expect(await save(review(3, 12), submit)).toBe(true);
    expect(sends.map(send => send.number)).toEqual([3]);
    expect(stored(3)).toMatchObject({ phase: 'unknown', request: { draft: sends[0].draft, idempotencyKey: sends[0].key } });
    expect(Number.isFinite(Date.parse(stored(3)!.attemptedAt!))).toBe(true);
    expect(stored(12)).toEqual(staged(12));
  });

  for (const [name, error, phase] of [
    ['an expired session', new IssueWriteError('Sign in again', { status: 401, kind: 'unauthenticated' }), 'failed'],
    ['an unexpected exception', new Error('Unexpected failure'), 'unknown'],
  ] as const) {
    test(`the batch stops after storing the outcome of ${name}`, async () => {
      const { sends, submit } = fakeSubmit(() => { throw error; });
      expect(await save(review(3, 12), submit)).toBe(true);
      expect(sends.map(send => send.number)).toEqual([3]);
      expect(stored(3)).toMatchObject({ phase });
      expect(stored(12)).toEqual(staged(12));
    });
  }

  test('a thrown send error preserves the operation id already saved by recovery', async () => {
    let recovered: BoardCard | undefined;
    const { submit } = fakeSubmit(number => {
      recovered = { ...stored(number)!, operationId: 43 };
      saveBoardCard(viewer, repo, recovered);
      throw new IssueWriteError('timeout', { status: 504, kind: 'transport' });
    });
    await save(review(3), submit);
    expect(stored(3)).toEqual(recovered);
    expect(stored(3)?.operationId).toBe(43);
  });

  test('edge: a card that became unknown in storage after the review opened is skipped and not sent', async () => {
    const reviewed = review(3, 12);
    const locked: BoardCard = { ...staged(3), phase: 'unknown', request: { draft: {}, idempotencyKey: 'other-tab' }, attemptedAt: '2026-11-25T09:00:00Z' };
    saveBoardCard(viewer, repo, locked);
    const { sends, submit } = fakeSubmit();
    await save(reviewed, submit);
    expect(sends.map(send => send.number)).toEqual([12]);
    expect(loadBoardCards(viewer, repo)).toEqual([locked]);
  });

  test('the repository lock covers the reread, send and outcome writes for the batch', async () => {
    const reviewed = review(3, 12);
    const locked: BoardCard = { ...staged(3), phase: 'unknown', operationId: 77 };
    const originalNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
    let lockName: string | undefined;
    let held = false;
    const changesWhileHeld: boolean[] = [];
    const locks = {
      request: async (name: string, work: () => Promise<boolean>) => {
        lockName = name;
        saveBoardCard(viewer, repo, locked);
        held = true;
        try { return await work(); }
        finally { held = false; }
      },
    };
    Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { locks } });
    try {
      const { sends, submit } = fakeSubmit();
      expect(await save(reviewed, submit, () => changesWhileHeld.push(held))).toBe(true);
      expect(lockName).toBe('repoorbit:issue-draft:user:ada:repo-1:board:');
      expect(sends.map(send => send.number)).toEqual([12]);
      expect(loadBoardCards(viewer, repo)).toEqual([locked]);
      expect(changesWhileHeld).toEqual([true, true]);
    } finally {
      if (originalNavigator) Object.defineProperty(globalThis, 'navigator', originalNavigator);
      else Reflect.deleteProperty(globalThis, 'navigator');
    }
  });

  test('edge: a card whose stored snapshot or target was replaced since review, or which was discarded, is not sent', async () => {
    const reviewed = review(3, 4, 5, 6);
    const newSnapshot = { ...staged(3), snapshot: { ...staged(3).snapshot, assignees: ['bob'], updatedAt: '2026-11-25T09:00:00Z' } };
    const newTarget: BoardCard = { ...staged(4), target: { status: 'done', assigneesAdd: [], assigneesRemove: [] } };
    saveBoardCard(viewer, repo, newSnapshot);
    saveBoardCard(viewer, repo, newTarget);
    removeBoardCard(viewer, repo, 5);
    const { sends, submit } = fakeSubmit();
    await save(reviewed, submit);
    expect(sends.map(send => send.number)).toEqual([6]);
    expect(loadBoardCards(viewer, repo)).toEqual([newSnapshot, newTarget]);
  });

  test('integration: the request, key and attemptedAt are in storage before submitIssueUpdate is called', async () => {
    const seen: (BoardCard | undefined)[] = [];
    let changes = 0;
    const changesAtSend: number[] = [];
    const { sends, submit } = fakeSubmit(number => {
      seen.push(stored(number));
      changesAtSend.push(changes);
      return operation(number, 'succeeded');
    });
    const before = Date.now();
    await save(review(3), (...args) => {
      expect(args[4]).toBeInstanceOf(AbortSignal);
      return submit(...args);
    }, () => { changes++; });
    expect(seen[0]).toMatchObject({ phase: 'unknown', request: { draft: sends[0].draft, idempotencyKey: sends[0].key } });
    expect(sends[0].key).toMatch(/^ro-/);
    expect(Date.parse(seen[0]!.attemptedAt!)).toBeGreaterThanOrEqual(before);
    expect(Date.parse(seen[0]!.attemptedAt!)).toBeLessThanOrEqual(Date.now());
    // The caller hears about the lock before the send, and about the outcome after it.
    expect(changesAtSend).toEqual([1]);
    expect(changes).toBe(2);
  });

  test('edge: a card whose request cannot be stored is not sent and stays staged', async () => {
    const reviewed = review(3);
    refuseWrites = true;
    const { sends, submit } = fakeSubmit();
    await save(reviewed, submit);
    expect(sends).toEqual([]);
    expect(stored(3)).toEqual(staged(3));
  });

  test('edge: a card whose storage write is silently dropped is not sent and stays staged', async () => {
    const reviewed = review(3);
    dropWrites = true;
    const { sends, submit } = fakeSubmit();
    await save(reviewed, submit);
    expect(sends).toEqual([]);
    expect(stored(3)).toEqual(staged(3));
  });

  test('edge: a result arriving after the stored card was cleared does not bring it back', async () => {
    const { submit } = fakeSubmit(number => {
      removeBoardCard(viewer, repo, number);
      return operation(number, 'unknown');
    });
    await save(review(3), submit);
    expect(loadBoardCards(viewer, repo)).toEqual([]);
  });

  test('edge: a second save started while one is running sends nothing', async () => {
    const reviewed = review(3, 12);
    let release = () => {};
    const held = new Promise<void>(resolve => { release = resolve; });
    const first = fakeSubmit(async number => { await held; return operation(number, 'succeeded'); });
    const second = fakeSubmit();
    const running = save(reviewed, first.submit);
    expect(await save(reviewed, second.submit)).toBe(false);
    release();
    expect(await running).toBe(true);
    expect(second.sends).toEqual([]);
    expect(first.sends.map(send => send.number)).toEqual([3, 12]);
    // The guard is released, so a later save runs.
    expect(await save(review(20), second.submit)).toBe(true);
    expect(second.sends.map(send => send.number)).toEqual([20]);
  });
});

test('an aborted update or recheck fetch is reported as an issue write transport error', async () => {
  const originalFetch = globalThis.fetch;
  const controller = new AbortController();
  const signals: (AbortSignal | null | undefined)[] = [];
  globalThis.fetch = async (_input, init) => {
    signals.push(init?.signal);
    controller.abort();
    controller.signal.throwIfAborted();
    throw new Error('Expected the fetch to abort');
  };
  try {
    for (const send of [
      () => submitIssueUpdate(repo, 3, 'ro-key', {}, controller.signal),
      () => recheckWriteOperation(43, 3, controller.signal),
    ]) {
      const result = send();
      await expect(result).rejects.toBeInstanceOf(IssueWriteError);
      await expect(result).rejects.toMatchObject({ kind: 'transport', status: 0 });
    }
    expect(signals).toEqual([controller.signal, controller.signal]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
