import { test, expect } from '@playwright/test';
import type { BoardIssue, BoardStatus } from '../src/lib/api';
import type { BoardCard, BoardCardPhase, BoardTarget } from '../src/lib/board';
import { confirmDisabled, reviewItems, reviewableCards, saveButtonState } from '../src/lib/board-review';

const ada = { login: 'ada', assignable: true };
const names: Record<BoardStatus, string> = { backlog: 'Backlog', todo: 'Todo', in_progress: 'In Progress', in_review: 'In Review', done: 'Done' };

const card = (number: number, phase: BoardCardPhase = 'staged', target: Partial<BoardTarget> = {}): BoardCard => ({
  version: 1,
  number,
  snapshot: { state: 'open', labels: ['status:review'], assignees: ['sam'], status: 'in_review', updatedAt: '2026-11-20T10:00:00Z' },
  target: { status: 'done', assigneesAdd: [], assigneesRemove: [], ...target },
  phase,
  request: null,
  operationId: null,
});

const row = (number: number, title: string): BoardIssue => ({
  number, title, state: 'open', url: `https://github.com/o/r/issues/${number}`, labels: ['status:review'], assignees: ['sam'],
  status: 'in_review', multipleStatus: false, authorLogin: 'alex', updatedAt: '2026-11-20T10:00:00Z',
});

test.describe('board save review', () => {
  test('Happy path: with one staged card the Save button is enabled, and the review lists that card\'s change and side effects', () => {
    const reviewed = reviewableCards([card(7)]);
    expect(saveButtonState({ staged: reviewed.length, blocked: null, saving: false })).toEqual({ disabled: false, title: undefined });
    expect(reviewItems(reviewed, [row(7, 'Ship the Board')], ada, names)).toEqual([{
      number: 7,
      title: 'Ship the Board',
      change: 'In Review → Done',
      sideEffects: ['Closes the issue as completed', 'Removes label status:review'],
    }]);
  });

  test('Disabled: with no staged cards the Save button is disabled', () => {
    const cards = (['changed', 'pending', 'unknown', 'failed', 'confirmed'] as const).map((phase, i) => card(i + 1, phase));
    expect(reviewableCards(cards)).toEqual([]);
    expect(saveButtonState({ staged: 0, blocked: null, saving: false })).toEqual({ disabled: true, title: 'No staged changes to save.' });
  });

  test('Blocked: in demo mode the Save button is disabled and its title explains why', () => {
    const blocked = 'Demo mode — remote GitHub writes are disabled, so Board changes cannot be staged.';
    expect(saveButtonState({ staged: 1, blocked, saving: false })).toEqual({ disabled: true, title: blocked });
    expect(saveButtonState({ staged: 0, blocked, saving: false })).toEqual({ disabled: true, title: blocked });
    expect(confirmDisabled({ blocked, saving: false })).toBe(true);
  });

  test('Busy: while a save runs, Confirm is disabled', () => {
    expect(confirmDisabled({ blocked: null, saving: true })).toBe(true);
    expect(saveButtonState({ staged: 1, blocked: null, saving: true }).disabled).toBe(true);
    expect(confirmDisabled({ blocked: null, saving: false })).toBe(false);
    expect(saveButtonState({ staged: 1, blocked: null, saving: false }).disabled).toBe(false);
  });

  test('only staged cards are reviewable, in issue-number order', () => {
    expect(reviewableCards([card(9), card(2, 'failed'), card(4)]).map(c => c.number)).toEqual([4, 9]);
  });

  test('an assignee-only change names the added and removed logins, and a card missing from the Board keeps its number', () => {
    const reviewed = [card(3, 'staged', { status: 'in_review', assigneesAdd: ['alex'], assigneesRemove: ['sam'] })];
    expect(reviewItems(reviewed, [], ada, names)).toEqual([{ number: 3, title: 'Issue not on the Board', change: 'In Review · +@alex · -@sam', sideEffects: [] }]);
  });

  test('a move lists explicit assignee edits and keeps auto-assignment only in the side effects', () => {
    const autoAssigned = card(4, 'staged', { status: 'in_progress' });
    autoAssigned.snapshot.assignees = [];
    const reviewed = [card(3, 'staged', { assigneesAdd: ['alex'], assigneesRemove: ['sam'] }), autoAssigned];
    const items = reviewItems(reviewed, [row(3, 'Ship the Board'), row(4, 'Start the next task')], ada, names);
    expect(items.map(item => item.change)).toEqual(['In Review → Done · +@alex · -@sam', 'In Review → In Progress']);
    expect(items[1].sideEffects).toContain('Assigns you (ada)');
  });
});
