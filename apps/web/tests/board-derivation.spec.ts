import { test, expect } from '@playwright/test';
import type { BoardIssue } from '../src/features/board/api';
import type { BoardSnapshot, IssueCapabilities, WriteOperation } from '../src/features/issues/api';
import { IssueWriteError } from '../src/features/issues/write-errors';
import { loadBoardCards, saveBoardCard } from '../src/features/board/card-storage';
import { applySaveResult, columnOf, deriveChange, filterOptions, getBlockedReason, isChangedOnGitHub, matchesFilters, searchNeedle, stageAssignees, targetOf, type BoardCard, type BoardCardPhase, type BoardTarget } from '../src/features/board/model';
import { issueDraftStorageKey } from '../src/features/issues/drafts';
import { mockWindowStorage } from './mocks';


const ada = { login: 'ada', assignable: true };
const t0 = '2026-11-20T10:00:00Z';
const t1 = '2026-11-21T10:00:00.123456Z';

function snap(over: Partial<BoardSnapshot>): BoardSnapshot {
  return { state: 'open', labels: [], assignees: [], status: 'backlog', updatedAt: t0, ...over };
}
function to(status: BoardTarget['status'], assigneesAdd: string[] = [], assigneesRemove: string[] = []): BoardTarget {
  return { status, assigneesAdd, assigneesRemove };
}

test.describe('deriveChange', () => {
  test('Todo to In Progress with an assignee moves the label and expects the snapshot', () => {
    const snapshot = snap({ status: 'todo', labels: ['status:todo'], assignees: ['sam'] });
    expect(deriveChange(snapshot, to('in_progress'), ada)).toEqual({
      draft: {
        labels: { add: ['status:in-progress'], remove: ['status:todo'] },
        expect: { status: 'todo', assignees: ['sam'], statusLabels: ['status:todo'] },
      },
      sideEffects: ['Removes label status:todo'],
    });
  });

  test('assignee edits survive a re-stage and never remove a teammate', () => {
    const fresh = snap({ status: 'todo', labels: ['status:todo'], assignees: ['bob'] });
    const draft = deriveChange(fresh, to('in_progress', ['sam']), ada)?.draft;
    expect(draft?.assignees).toEqual({ add: ['sam'] });
    expect(draft?.expect?.assignees).toEqual(['bob']);
  });

  test('Backlog with no assignees to In Progress assigns an assignable saver (AE8)', () => {
    const change = deriveChange(snap({}), to('in_progress'), ada);
    expect(change?.draft).toEqual({
      labels: { add: ['status:in-progress'] },
      assignees: { add: ['ada'] },
      expect: { status: 'backlog', assignees: [], statusLabels: [] },
    });
    expect(change?.sideEffects).toEqual(['Assigns you (ada)']);
  });

  test('Backlog to In Progress skips the assignment when GitHub will not accept the saver (AE8)', () => {
    const change = deriveChange(snap({}), to('in_progress'), { login: 'ada', assignable: false });
    expect(change?.draft.assignees).toBeUndefined();
    expect(change?.draft.labels).toEqual({ add: ['status:in-progress'] });
    expect(change?.sideEffects.some(line => /assignment skipped/i.test(line))).toBe(true);
  });

  test('In Progress after removing every assignee does not self-assign', () => {
    const snapshot = snap({ status: 'in_progress', labels: ['status:in-progress'], assignees: ['sam', 'bob'] });
    expect(deriveChange(snapshot, to('in_progress', [], ['sam', 'bob']), ada)?.draft).toEqual({
      assignees: { remove: ['sam', 'bob'] },
      expect: { status: 'in_progress', assignees: ['sam', 'bob'], statusLabels: ['status:in-progress'] },
    });
  });

  test('In Review to Done closes the issue and removes its status label (AE8)', () => {
    const change = deriveChange(snap({ status: 'in_review', labels: ['bug', 'status:review'] }), to('done'), ada);
    expect(change?.draft.state).toBe('closed');
    expect(change?.draft.labels).toEqual({ remove: ['status:review'] });
    expect(change?.sideEffects).toEqual(['Closes the issue as completed', 'Removes label status:review']);
  });

  test('Done with a leftover label to Todo reopens, adds the target label and removes the leftover', () => {
    const change = deriveChange(snap({ state: 'closed', status: 'done', labels: ['status:review'] }), to('todo'), ada);
    expect(change?.draft.state).toBe('open');
    expect(change?.draft.labels).toEqual({ add: ['status:todo'], remove: ['status:review'] });
    expect(change?.sideEffects).toContain('Reopens the issue');
  });

  test('Done still carrying the target label to Todo reopens and still adds the label', () => {
    const draft = deriveChange(snap({ state: 'closed', status: 'done', labels: ['Status:Todo'] }), to('todo'), ada)?.draft;
    expect(draft?.state).toBe('open');
    expect(draft?.labels).toEqual({ add: ['status:todo'] });
  });

  test('a flagged open issue moved to Todo only removes the other status label', () => {
    const snapshot = snap({ status: 'in_review', labels: ['status:todo', 'status:review'] });
    expect(deriveChange(snapshot, to('todo'), ada)?.draft.labels).toEqual({ remove: ['status:review'] });
  });

  test('an assignee-only edit on a flagged card leaves its labels alone', () => {
    const snapshot = snap({ status: 'in_review', labels: ['status:todo', 'status:review'] });
    const draft = deriveChange(snapshot, to('in_review', ['sam']), ada)?.draft;
    expect(draft?.labels).toBeUndefined();
    expect(draft?.assignees).toEqual({ add: ['sam'] });
  });

  test("removal uses the issue's own spelling", () => {
    const snapshot = snap({ status: 'todo', labels: ['Status:Todo'] });
    expect(deriveChange(snapshot, to('in_progress'), ada)?.draft.labels).toEqual({ add: ['status:in-progress'], remove: ['Status:Todo'] });
  });

  test('a target equal to the snapshot is no change', () => {
    expect(deriveChange(snap({ status: 'todo', labels: ['status:todo'] }), to('todo'), ada)).toBeNull();
    expect(deriveChange(snap({ status: 'in_progress', labels: ['status:in-progress'] }), to('in_progress'), ada)).toBeNull();
    const assigned = snap({ status: 'todo', labels: ['status:todo'], assignees: ['sam'] });
    expect(deriveChange(assigned, to('todo', ['Sam'], ['bob']), ada)).toBeNull();
  });

  test('assigning on a Backlog target moves it to Todo and names the side effect', () => {
    const target = stageAssignees(to('backlog'), ['sam'], []);
    expect(target).toEqual({ status: 'todo', assigneesAdd: ['sam'], assigneesRemove: [], assignedFromBacklog: true });
    const change = deriveChange(snap({}), target, ada);
    expect(change?.draft.labels).toEqual({ add: ['status:todo'] });
    expect(change?.draft.assignees).toEqual({ add: ['sam'] });
    expect(change?.sideEffects).toEqual(['Moves to Todo because an assignee was added']);
  });

  test('assigning on a Todo target leaves the status alone', () => {
    expect(stageAssignees(to('todo'), ['sam'], [])).toEqual({ status: 'todo', assigneesAdd: ['sam'], assigneesRemove: [] });
  });
});

test.describe('isChangedOnGitHub', () => {
  function card(snapshot: BoardSnapshot, phase: BoardCardPhase = 'staged'): BoardCard {
    return { version: 1, number: 7, snapshot, target: to('in_progress'), phase, request: null, operationId: null };
  }
  function row(over: Partial<BoardIssue>): BoardIssue {
    return { number: 7, title: 'Seven', url: 'https://github.com/o/r/issues/7', state: 'open', updatedAt: t1, authorLogin: null, labels: [], assignees: [], status: 'backlog', multipleStatus: false, ...over };
  }
  const todo = snap({ status: 'todo', labels: ['status:todo'], assignees: ['Sam'] });

  test('a newer row with a different status is changed', () => {
    expect(isChangedOnGitHub(card(todo), row({ status: 'in_review', labels: ['status:review'], assignees: ['sam'] }))).toBe(true);
  });

  test('a newer row with a different assignee set is changed', () => {
    expect(isChangedOnGitHub(card(todo), row({ status: 'todo', labels: ['status:todo'], assignees: ['sam', 'bob'] }))).toBe(true);
  });

  test('a newer row differing only in spelling is not changed', () => {
    expect(isChangedOnGitHub(card(todo), row({ status: 'todo', labels: ['Status:Todo', 'bug'], assignees: ['sam'] }))).toBe(false);
  });

  test('a newer open row with a different status-label set but the same status is changed', () => {
    const review = snap({ status: 'in_review', labels: ['status:review'] });
    expect(isChangedOnGitHub(card(review), row({ status: 'in_review', labels: ['status:todo', 'status:review'], multipleStatus: true }))).toBe(true);
  });

  test('a newer closed row with only a leftover label removed is not changed', () => {
    const done = snap({ state: 'closed', status: 'done', labels: ['status:review'] });
    expect(isChangedOnGitHub(card(done), row({ state: 'closed', status: 'done', labels: [] }))).toBe(false);
  });

  test('an older row with a different status is not changed (projection lag)', () => {
    const staged = card({ ...todo, updatedAt: t1 });
    expect(isChangedOnGitHub(staged, row({ updatedAt: t0, status: 'in_review', labels: ['status:review'], assignees: ['sam'] }))).toBe(false);
  });

  test('a pending or unknown card is never changed, nor a confirmed or failed one still holding its pre-send snapshot', () => {
    const moved = row({ status: 'in_review', labels: ['status:review'], assignees: ['sam'] });
    for (const phase of ['pending', 'unknown', 'confirmed', 'failed'] as const) expect(isChangedOnGitHub(card(todo, phase), moved)).toBe(false);
  });
});

test.describe('applySaveResult', () => {
  const attemptedAt = '2026-11-21T10:00:00.000Z';
  const request = {
    draft: { labels: { add: ['status:in-progress'], remove: ['status:todo'] }, expect: { status: 'todo' as const, assignees: ['sam'], statusLabels: ['status:todo'] } },
    idempotencyKey: 'idem-key-7',
  };
  const snapshot = snap({ status: 'todo', labels: ['status:todo'], assignees: ['sam'] });

  // The card as the save loop holds it: the request, key and attempt time are filled in before the send.
  function sent(over: Partial<BoardCard> = {}): BoardCard {
    return { version: 1, number: 7, snapshot, target: to('in_progress'), phase: 'staged', request, operationId: null, attemptedAt, ...over };
  }
  function operation(state: WriteOperation['state'], id = 42): WriteOperation {
    return { id, repositoryId: 1, kind: 'update', state, issueNumber: 7, draft: request.draft, outcome: null, actor: 'ada', createdAt: attemptedAt, updatedAt: attemptedAt };
  }
  function cleared(phase: BoardCardPhase, over: Partial<BoardCard> = {}) {
    return { kind: 'card', card: sent({ phase, request: null, attemptedAt: null, ...over }) };
  }
  function lockedUnknown(operationId: number | null) {
    return { kind: 'card', card: sent({ phase: 'unknown', operationId }) };
  }

  test('a write that resolves succeeded removes the card', () => {
    expect(applySaveResult(sent(), { operation: operation('succeeded') })).toEqual({ kind: 'removed' });
  });

  test('a write that resolves failed maps to failed and clears the request, key and attemptedAt', () => {
    expect(applySaveResult(sent(), { operation: operation('failed') })).toEqual(cleared('failed', { operationId: 42 }));
  });

  for (const state of ['unknown', 'pending'] as const) {
    test(`a write that resolves ${state} maps to a locked unknown card without any thrown error`, () => {
      expect(applySaveResult(sent(), { operation: operation(state) })).toEqual(lockedUnknown(42));
    });
  }

  test('a 409 changed_on_github maps to changed and stores the current snapshot', () => {
    const current = snap({ status: 'in_review', labels: ['status:review'], assignees: ['bob'], updatedAt: t1 });
    const error = new IssueWriteError('changed', { status: 409, kind: 'changed_on_github', current });
    expect(applySaveResult(sent(), { error })).toEqual(cleared('changed', { snapshot: current }));
  });

  test('an assignee-only edit keeps a teammate’s closed status when staged again', () => {
    const current = snap({ state: 'closed', status: 'done', assignees: ['sam'], updatedAt: t1 });
    const error = new IssueWriteError('changed', { status: 409, kind: 'changed_on_github', current });
    const target = to('todo', ['alex'], ['sam']);
    expect(applySaveResult(sent({ target }), { error })).toEqual(cleared('changed', { snapshot: current, target: { ...target, status: 'done' } }));
  });

  test('a not_recorded failure maps to failed and clears the request and key', () => {
    const error = new IssueWriteError('refused', { status: 422, kind: 'unprocessable', disposition: 'not-recorded' });
    expect(applySaveResult(sent(), { error })).toEqual(cleared('failed'));
  });

  test('a re-saved failed card does not keep the earlier operation id when it is not recorded', () => {
    const error = new IssueWriteError('refused', { status: 403, kind: 'forbidden' });
    expect(applySaveResult(sent({ phase: 'failed', operationId: 41 }), { error })).toEqual(cleared('failed'));
  });

  const possiblyRecorded: [string, unknown][] = [
    ['a gateway timeout', new IssueWriteError('timeout', { status: 504, kind: 'transport' })],
    ['a possibly-recorded disposition', new IssueWriteError('unverified', { status: 503, kind: 'unverified', disposition: 'possibly-recorded' })],
    ['a network rejection', new TypeError('Failed to fetch')],
  ];
  for (const [name, error] of possiblyRecorded) {
    test(`possibly_recorded from ${name} maps to a locked unknown card with no operation id`, () => {
      expect(applySaveResult(sent({ phase: 'failed', operationId: 41 }), { error })).toEqual(lockedUnknown(null));
    });
  }

  test('recheck_unresolved maps to a locked unknown card holding the operation id', () => {
    const error = new IssueWriteError('unresolved', { status: 409, kind: 'recheck_unresolved', operation: operation('unknown') });
    expect(applySaveResult(sent(), { error })).toEqual(lockedUnknown(42));
  });

  test('recheck_unresolved without an operation on a new send drops the earlier operation id', () => {
    const error = new IssueWriteError('unresolved', { status: 409, kind: 'recheck_unresolved' });
    expect(applySaveResult(sent({ phase: 'failed', operationId: 41 }), { error })).toEqual(lockedUnknown(null));
  });

  test('a replay of the same key keeps the operation id the card already has', () => {
    for (const error of [
      new IssueWriteError('timeout', { status: 504, kind: 'transport' }),
      new IssueWriteError('unresolved', { status: 409, kind: 'recheck_unresolved' }),
    ]) {
      expect(applySaveResult(sent({ phase: 'unknown', operationId: 41 }), { error, isReplay: true })).toEqual(lockedUnknown(41));
    }
  });

  test('a refused replay of the same key stays a locked unknown card', () => {
    const error = new IssueWriteError('refused', { status: 403, kind: 'forbidden', disposition: 'not-recorded' });
    expect(applySaveResult(sent(), { error, isReplay: true })).toEqual(lockedUnknown(null));
  });

  test('a duplicate_create refusal, which an update never gets, stays a locked unknown card', () => {
    const error = new IssueWriteError('duplicate', { status: 409, kind: 'duplicate_create', operationId: 9 });
    expect(applySaveResult(sent(), { error })).toEqual(lockedUnknown(null));
  });

  // Contract for the save loop: cards already unknown or pending are skipped, never re-sent or mapped.
  test('contract: the save loop only maps staged, changed or failed cards, and never mutates the card it passes', () => {
    for (const phase of ['staged', 'changed', 'failed'] as const) {
      const card = sent({ phase });
      const before = structuredClone(card);
      applySaveResult(card, { operation: operation('failed') });
      expect(card).toEqual(before);
    }
  });
});

test.describe('saveBoardCard', () => {
  test('persists a pending card as unknown while keeping other fields intact', () => {
    const { restore } = mockWindowStorage();
    try {
      const snapshot = snap({ status: 'todo', labels: ['status:todo'], assignees: ['sam'] });
      const target = to('in_progress', ['ada']);
      const pendingCard: BoardCard = {
        version: 1,
        number: 7,
        snapshot,
        target,
        phase: 'pending',
        request: {
          draft: { labels: { add: ['status:in-progress'], remove: ['status:todo'] }, assignees: { add: ['ada'] } },
          idempotencyKey: 'idem-key-7',
        },
        operationId: 42,
      };
      const stagedCard: BoardCard = {
        version: 1,
        number: 8,
        snapshot,
        target,
        phase: 'staged',
        request: null,
        operationId: null,
      };
      expect(saveBoardCard('user:ada', 'repo-1', pendingCard)).toBe(true);
      expect(saveBoardCard('user:ada', 'repo-1', stagedCard)).toBe(true);
      expect(loadBoardCards('user:ada', 'repo-1')).toEqual([
        { ...pendingCard, phase: 'unknown' },
        stagedCard,
      ]);
    } finally {
      restore();
    }
  });
});

function issueRow(number: number, over: Partial<BoardIssue> = {}): BoardIssue {
  return { number, title: `Issue ${number}`, url: `https://github.com/o/r/issues/${number}`, state: 'open', updatedAt: t0, authorLogin: null, labels: [], assignees: [], status: 'backlog', multipleStatus: false, ...over };
}
function stagedCard(number: number, target: BoardTarget, phase: BoardCardPhase = 'staged'): BoardCard {
  return { version: 1, number, snapshot: snap({}), target, phase, request: null, operationId: null };
}

test.describe('filters', () => {
  const rows = [
    issueRow(7, { title: 'Fix Login', assignees: ['Sam'], labels: ['bug'] }),
    issueRow(17, { title: 'Add search', assignees: ['bob'], labels: ['Bug', 'ui'] }),
    issueRow(70, { title: 'Login page copy', assignees: ['Sam', 'bob'], labels: ['ui'] }),
  ];
  const shown = (assignee = '', label = '', text = '') => {
    const needle = searchNeedle(text);
    return rows.filter(row => matchesFilters(row, { assignee, label, needle })).map(row => row.number);
  };

  test('no filter keeps every row', () => {
    expect(shown()).toEqual([7, 17, 70]);
    expect(shown('', '', '  # ')).toEqual([7, 17, 70]);
  });

  test('assignee and label match the exact spelling', () => {
    expect(shown('Sam')).toEqual([7, 70]);
    expect(shown('sam')).toEqual([]);
    expect(shown('', 'bug')).toEqual([7]);
    expect(shown('', 'ui')).toEqual([17, 70]);
  });

  test('text matches part of the title in any case, or the whole issue number', () => {
    expect(shown('', '', '  LOGIN ')).toEqual([7, 70]);
    expect(shown('', '', '#7')).toEqual([7]);
    expect(shown('', '', '7')).toEqual([7]);
    expect(shown('', '', '##7')).toEqual([]);
  });

  test('filters combine', () => {
    expect(shown('Sam', 'ui', 'login')).toEqual([70]);
    expect(shown('bob', 'bug')).toEqual([]);
  });

  test('options are unique and sorted by locale', () => {
    expect(filterOptions(['ui', 'bug', 'Bug', 'ui'])).toEqual(['bug', 'Bug', 'ui']);
  });
});

test.describe('card placement', () => {
  const todoRow = issueRow(7, { status: 'todo' });

  test('an unstaged row sits in its status column', () => {
    expect(columnOf(todoRow, undefined)).toBe('todo');
    expect(columnOf(issueRow(8, { status: null }), undefined)).toBeNull();
  });

  test('a staged card moves to its target column, a failed one stays', () => {
    for (const phase of ['staged', 'changed', 'pending', 'unknown', 'confirmed'] as const) {
      expect(columnOf(todoRow, stagedCard(7, to('done'), phase))).toBe('done');
    }
    expect(columnOf(todoRow, stagedCard(7, to('done'), 'failed'))).toBe('todo');
  });

  test('the drop target starts from the card target, or from where the row is', () => {
    expect(JSON.stringify(targetOf(todoRow, undefined))).toBe('{"status":"todo","assigneesAdd":[],"assigneesRemove":[]}');
    const target = to('done', ['sam']);
    expect(targetOf(todoRow, stagedCard(7, target, 'failed'))).toBe(target);
  });
});

test.describe('getBlockedReason', () => {
  const board = { statusLabels: true, truncated: false, issues: [issueRow(9, { state: 'closed', status: 'done' })] };
  const writer: IssueCapabilities = { canCreateIssue: true, canCreateIssueWithMetadata: true, canManageIssues: true };
  const input = { demo: false, board, capabilitiesError: null, capabilities: writer };

  test('Demo wins over every other reason', () => {
    expect(getBlockedReason({ demo: true, board: { ...board, statusLabels: false }, capabilitiesError: 'nope', capabilities: null }))
      .toBe('Demo mode — remote GitHub writes are disabled, so Board changes cannot be staged.');
  });

  test('a Board without status labels, then a capability error, then unchecked permissions', () => {
    expect(getBlockedReason({ ...input, board: { ...board, statusLabels: false }, capabilitiesError: 'nope' }))
      .toBe('No status source: turn status labels on to stage Board changes.');
    expect(getBlockedReason({ ...input, capabilitiesError: 'Could not check permissions' })).toBe('Could not check permissions');
    expect(getBlockedReason({ ...input, board: null, capabilities: null })).toBe('Checking your current GitHub permissions…');
  });

  test('a read-only user is told which permission is needed', () => {
    expect(getBlockedReason({ ...input, capabilities: { ...writer, canManageIssues: false } }))
      .toBe('Moving cards, changing assignees, and closing or reopening issues requires Triage or higher permission on GitHub.');
  });

  test('a Board holding a closed issue gives a user with write access no reason', () => {
    expect(getBlockedReason(input)).toBeNull();
  });
});

test.describe('loadBoardCards', () => {
  test('reads a stored pending card as pending and does not rewrite it', () => {
    const { storage, restore } = mockWindowStorage();
    try {
      const key = issueDraftStorageKey('user:ada', 'repo-1', { board: 7 });
      const raw = JSON.stringify(stagedCard(7, to('in_progress'), 'pending'));
      storage.setItem(key, raw);
      expect(loadBoardCards('user:ada', 'repo-1').map(card => card.phase)).toEqual(['pending']);
      expect(storage.getItem(key)).toBe(raw);
    } finally {
      restore();
    }
  });
});
