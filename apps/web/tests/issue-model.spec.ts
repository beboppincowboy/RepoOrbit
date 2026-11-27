import { expect, test } from '@playwright/test';
import { hasSavedCreateDraft, loadEditorDraft, saveEditorDraft } from '../src/features/issues/editor-storage';
import { addTokens, buildIssueDraft, getPermissionNotice, hasIgnoreCase, removeIgnoreCase, reviewIssueChanges, safeHref, splitTokens, validateLabel, validateLogin, type IssueFields } from '../src/features/issues/model';
import { mockWindowStorage } from './mocks';

const noFields: IssueFields = { title: '', body: '', createLabels: [], createAssignees: [], addLabels: [], removeLabels: [], addAssignees: [], removeAssignees: [], targetState: '' };
const fields = (over: Partial<IssueFields>): IssueFields => ({ ...noFields, ...over });
const noWrite = { step: 'edit', idempotencyKey: null, attemptedAt: null, submittedDraft: null, keepKeyOnNextSubmit: false, lastError: null, lastOperation: null, duplicateOperationId: null };

test('safeHref accepts only http and https links', () => {
  expect(safeHref('http://github.com/a')).toBe('http://github.com/a');
  expect(safeHref('HTTPS://github.com/a')).toBe('HTTPS://github.com/a');
  for (const url of ['javascript:alert(1)', '//github.com', 'http:/github.com', ' https://github.com', 'github.com', '', null, undefined]) {
    expect(safeHref(url)).toBeNull();
  }
});

test('label and login validators', () => {
  const labelError = 'Labels must be 1 to 50 characters with no surrounding spaces.';
  expect(validateLabel('a'.repeat(50))).toBeNull();
  expect(validateLabel('good first issue')).toBeNull();
  for (const label of ['', ' bug', 'bug ', 'a'.repeat(51)]) expect(validateLabel(label)).toBe(labelError);

  for (const login of ['a', 'a-', 'a--b', 'A1', 'a'.repeat(39)]) expect(validateLogin(login)).toBeNull();
  for (const login of ['', '-a', 'a_b', '@ada', 'a'.repeat(40)]) expect(validateLogin(login)).toBe(`"${login}" is not a valid GitHub login.`);
});

test('token splitting and case-insensitive helpers', () => {
  expect(splitTokens(' bug, ,docs ,, ')).toEqual(['bug', 'docs']);
  expect(hasIgnoreCase(['Bug'], 'bUG')).toBe(true);
  expect(removeIgnoreCase(['Bug', 'docs', 'BUG'], 'bug')).toEqual(['docs']);
});

test('adding tokens deduplicates, keeps the first spelling and un-removes', () => {
  expect(addTokens('Bug, bug, BUG, docs', [], [], validateLabel, 'labels')).toEqual({ nextAdd: ['Bug', 'docs'], nextRemove: [], error: null });
  expect(addTokens('bug', ['Bug'], [], validateLabel, 'labels')).toEqual({ nextAdd: ['Bug'], nextRemove: [], error: null });
  expect(addTokens('old, new', [], ['Old'], validateLabel, 'labels')).toEqual({ nextAdd: ['new'], nextRemove: [], error: null });
});

test('adding tokens enforces the item limit and returns the partial lists', () => {
  const ten = Array.from({ length: 10 }, (_, i) => `l${i}`);
  expect(addTokens('L0, gone', ten, ['gone'], validateLabel, 'labels')).toEqual({ nextAdd: ten, nextRemove: [], error: null });
  expect(addTokens('extra', ten, [], validateLabel, 'labels')).toEqual({ nextAdd: ten, nextRemove: [], error: 'At most 10 labels can be added at once.' });
  expect(addTokens('ada, a_b, bob', [], [], validateLogin, 'assignees')).toEqual({ nextAdd: ['ada'], nextRemove: [], error: '"a_b" is not a valid GitHub login.' });
});

const review = (over: Partial<Parameters<typeof reviewIssueChanges>[0]>) =>
  reviewIssueChanges({ isCreate: true, fields: fields({ title: 'T' }), labelInput: '', assigneeInput: '', canCreateMetadata: true, canManage: true, ...over });

test('create review commits pending chips before a later error', () => {
  expect(review({ labelInput: 'bug', assigneeInput: 'a_b' })).toEqual({
    labels: { add: ['bug'], remove: [] }, assignees: null, error: '"a_b" is not a valid GitHub login.',
  });
  expect(review({ fields: noFields, labelInput: 'bug', assigneeInput: 'ada' })).toEqual({
    labels: { add: ['bug'], remove: [] }, assignees: { add: ['ada'], remove: [] }, error: 'Title must be 1 to 256 characters.',
  });
  // A label error wins over every later check and commits nothing.
  expect(review({ fields: noFields, labelInput: 'x'.repeat(51), assigneeInput: 'ada' })).toEqual({
    labels: null, assignees: null, error: 'Labels must be 1 to 50 characters with no surrounding spaces.',
  });
});

test('create review checks title, body, then permission', () => {
  const longBody = 'b'.repeat(65537);
  expect(review({ fields: fields({ title: 't'.repeat(257), body: longBody }) }).error).toBe('Title must be 1 to 256 characters.');
  expect(review({ fields: fields({ title: ` ${'t'.repeat(256)} `, body: longBody }) }).error).toBe('Description must be at most 65536 characters.');
  expect(review({ fields: fields({ title: 'T', body: 'b'.repeat(65536) }) }).error).toBeNull();
  expect(review({ fields: fields({ title: 'T', createLabels: ['bug'] }), canCreateMetadata: false }).error)
    .toBe('Adding labels or assignees on a new issue requires Push access on GitHub.');
});

test('review skips pending input without the permission or with only whitespace', () => {
  expect(review({ labelInput: 'x'.repeat(51), assigneeInput: 'a_b', canCreateMetadata: false })).toEqual({ labels: null, assignees: null, error: null });
  expect(review({ labelInput: '  ', assigneeInput: ' ' })).toEqual({ labels: null, assignees: null, error: null });
  expect(review({ isCreate: false, fields: fields({ targetState: 'closed' }), labelInput: 'x'.repeat(51), assigneeInput: 'a_b', canManage: false }))
    .toEqual({ labels: null, assignees: null, error: null });
});

test('update review commits pending chips and reports the first error', () => {
  expect(review({ isCreate: false, fields: fields({ removeLabels: ['Bug', 'old'] }), labelInput: 'bug, new', assigneeInput: 'a_b' })).toEqual({
    labels: { add: ['new'], remove: ['old'] }, assignees: null, error: '"a_b" is not a valid GitHub login.',
  });
  expect(review({ isCreate: false, fields: noFields, assigneeInput: 'ada' })).toEqual({
    labels: null, assignees: { add: ['ada'], remove: [] }, error: null,
  });
  expect(review({ isCreate: false, fields: fields({ addLabels: ['bug'] }), canManage: false }).error)
    .toBe('Changing labels or assignees requires Triage or higher permission on GitHub.');
  expect(review({ isCreate: false, fields: noFields }).error).toBe('Choose at least one label, assignee, or state change to review.');
});

test('current create draft trims the title only and omits empty properties', () => {
  expect(JSON.stringify(buildIssueDraft(true, fields({ title: ' T ' })))).toBe('{"title":"T"}');
  expect(JSON.stringify(buildIssueDraft(true, fields({ title: 'T', body: '  ', createLabels: ['bug'], createAssignees: ['ada'], addLabels: ['no'], targetState: 'closed' }))))
    .toBe('{"title":"T","body":"  ","labels":["bug"],"assignees":["ada"]}');
});

test('current update draft omits empty properties', () => {
  expect(JSON.stringify(buildIssueDraft(false, fields({ title: 'no', body: 'no' })))).toBe('{}');
  expect(JSON.stringify(buildIssueDraft(false, fields({ addLabels: ['a'], removeAssignees: ['bob'], targetState: 'open' }))))
    .toBe('{"labels":{"add":["a"]},"assignees":{"remove":["bob"]},"state":"open"}');
  expect(JSON.stringify(buildIssueDraft(false, fields({ removeLabels: ['r'], addLabels: ['a'], addAssignees: ['ada'] }))))
    .toBe('{"labels":{"add":["a"],"remove":["r"]},"assignees":{"add":["ada"]}}');
});

test('permission notice', () => {
  const none = { canCreateIssue: false, canCreateIssueWithMetadata: false, canManageIssues: false };
  const notice = (over: object) => getPermissionNotice({ demo: false, archived: false, capabilitiesLoading: false, capabilitiesError: null, capabilities: none as never, isCreate: true, isAuthor: false, ...over });
  expect(notice({ demo: true, archived: true })).toBeNull();
  expect(notice({ archived: true, capabilitiesLoading: true })).toBe('This repository is archived on GitHub and does not accept issue writes.');
  expect(notice({ capabilitiesLoading: true, capabilitiesError: 'boom' })).toBeNull();
  expect(notice({ capabilitiesError: 'boom' })).toBe('boom');
  expect(notice({ capabilities: null })).toBe('GitHub permissions could not be confirmed for this repository.');
  expect(notice({})).toBe('Your current GitHub permission does not allow creating issues in this repository.');
  expect(notice({ capabilities: { ...none, canCreateIssue: true } })).toBe('You can create an issue with a title and description. Adding labels or assignees on a new issue requires Push access on GitHub.');
  expect(notice({ capabilities: { ...none, canCreateIssue: true, canCreateIssueWithMetadata: true } })).toBeNull();
  expect(notice({ isCreate: false, capabilities: { ...none, canManageIssues: true } })).toBeNull();
  expect(notice({ isCreate: false, isAuthor: true, capabilities: { ...none, canCreateIssue: true } })).toBe('With Read access you can close or reopen issues you authored. Changing labels or assignees requires Triage or higher permission on GitHub.');
  expect(notice({ isCreate: false, capabilities: { ...none, canCreateIssue: true } })).toBe('Changing labels, assignees, or issue state requires Triage or higher permission on GitHub.');
});

test.describe('draft storage', () => {
  let mock: ReturnType<typeof mockWindowStorage>;
  test.afterEach(() => mock.restore());

  const attempt = {
    step: 'unknown' as const, idempotencyKey: 'k', attemptedAt: '2026-01-01T00:00:00.000Z', submittedDraft: { title: 'T' },
    keepKeyOnNextSubmit: true, lastError: 'e', lastOperation: null, duplicateOperationId: 5,
  };
  // As the version-1 editor wrote them.
  const storedCreate = '{"version":1,"title":"T","body":"B","labels":["bug"],"assignees":["ada"],"duplicateOperationId":5,"submittedDraft":{"title":"T"},"step":"unknown","idempotencyKey":"k","attemptedAt":"2026-01-01T00:00:00.000Z","keepKeyOnNextSubmit":true,"lastError":"e","lastOperation":null}';
  const storedUpdate = '{"version":1,"issueNumber":7,"addLabels":["a"],"removeLabels":["r"],"addAssignees":["ada"],"removeAssignees":["bob"],"state":"closed","submittedDraft":{"state":"closed"},"step":"unknown","idempotencyKey":"k","attemptedAt":"2026-01-01T00:00:00.000Z","keepKeyOnNextSubmit":true,"lastError":"e","lastOperation":null}';
  const createFields = fields({ title: 'T', body: 'B', createLabels: ['bug'], createAssignees: ['ada'] });
  const updateFields = fields({ addLabels: ['a'], removeLabels: ['r'], addAssignees: ['ada'], removeAssignees: ['bob'], targetState: 'closed' });

  test('saves a create draft in the version-1 shape and loads it back', () => {
    mock = mockWindowStorage();
    expect(saveEditorDraft('key', 'create', 0, createFields, attempt, { verify: true })).toBe(true);
    expect(mock.storage.getItem('key')).toBe(storedCreate);
    expect(loadEditorDraft('key', 'create')).toEqual({ fields: createFields, write: attempt });
  });

  test('saves an update draft without duplicateOperationId and loads it back', () => {
    mock = mockWindowStorage();
    expect(saveEditorDraft('key', 'update', 7, updateFields, { ...attempt, submittedDraft: { state: 'closed' } })).toBe(true);
    expect(mock.storage.getItem('key')).toBe(storedUpdate);
    expect(loadEditorDraft('key', 'update')).toEqual({
      fields: updateFields,
      write: { ...attempt, submittedDraft: { state: 'closed' }, duplicateOperationId: null },
    });
  });

  test('loads leniently: legacy, unknown version and stray fields', () => {
    mock = mockWindowStorage();
    mock.storage.setItem('legacy', '{"version":1,"title":"T","idempotencyKey":"k","step":"unknown"}');
    expect(loadEditorDraft('legacy', 'create')).toEqual({
      fields: fields({ title: 'T' }),
      write: { ...noWrite, step: 'unknown', idempotencyKey: 'k' },
    });
    mock.storage.setItem('stray', '{"version":2,"addLabels":["a"],"duplicateOperationId":5}');
    expect(loadEditorDraft('stray', 'update')).toEqual({ fields: fields({ addLabels: ['a'] }), write: noWrite });
  });

  test('loads defaults when there is no key or no draft', () => {
    mock = mockWindowStorage();
    mock.storage.setItem('key', storedCreate);
    expect(loadEditorDraft(null, 'create')).toEqual({ fields: noFields, write: noWrite });
    expect(loadEditorDraft('missing', 'update')).toEqual({ fields: noFields, write: noWrite });
  });

  test('verified save reports a write that did not persist', () => {
    mock = mockWindowStorage({ dropWrites: true });
    expect(saveEditorDraft('key', 'create', 0, createFields, attempt, { verify: true })).toBe(false);
    expect(saveEditorDraft('key', 'create', 0, createFields, attempt)).toBe(true);
  });

  test('save reports a refused write', () => {
    mock = mockWindowStorage({ refuseWrites: true });
    expect(saveEditorDraft('key', 'update', 7, updateFields, attempt)).toBe(false);
  });

  test('hasSavedCreateDraft ignores an empty or blank draft', () => {
    mock = mockWindowStorage();
    const key = 'repoorbit:issue-draft:real:ada:42:create';
    expect(hasSavedCreateDraft('real:ada', '42')).toBe(false);
    mock.storage.setItem(key, '{"title":"  ","body":"","labels":[],"assignees":[],"idempotencyKey":"k"}');
    expect(hasSavedCreateDraft('real:ada', '42')).toBe(false);
    mock.storage.setItem(key, '{"duplicateOperationId":5}');
    expect(hasSavedCreateDraft('real:ada', '42')).toBe(true);
  });
});
