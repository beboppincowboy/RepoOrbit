import { test, expect } from '@playwright/test';
import { IssueWriteError, canReplayWithSameKey, classifyWriteFailure, mayHaveBeenRecorded } from '../src/lib/api';

test('mayHaveBeenRecorded and classifyWriteFailure use RepoOrbit-Write-Disposition without message matching', () => {
  const error = (status: number, kind: IssueWriteError['kind'], disposition: IssueWriteError['disposition'] = null, message = 'arbitrary prose') =>
    new IssueWriteError(message, { status, kind, disposition });
  for (const recorded of [
    error(0, 'transport'), error(502, 'transport'), error(504, 'transport'), error(500, 'server'), error(418, 'server'),
    error(500, 'server', 'not-recorded'),
    error(503, 'unverified', 'possibly-recorded', 'different message without key hint'),
    error(503, 'unverified', null, 'proxy 503 without disposition header'),
    new SyntaxError('Unexpected end of JSON input'),
  ]) {
    expect(mayHaveBeenRecorded(recorded)).toBe(true);
    expect(classifyWriteFailure(recorded).outcome).toBe('possibly_recorded');
  }
  for (const definite of [
    error(400, 'validation', null), error(401, 'unauthenticated', null),
    error(403, 'forbidden', null), error(404, 'not_found', null),
    error(422, 'unprocessable', null), error(409, 'changed_on_github', null),
    error(400, 'validation', 'not-recorded'), error(403, 'forbidden', 'not-recorded'),
    error(503, 'unverified', 'not-recorded', 'retry with the same Idempotency-Key'),
  ]) {
    expect(mayHaveBeenRecorded(definite)).toBe(false);
    expect(classifyWriteFailure(definite).outcome).toBe('not_recorded');
  }
  const changed = new IssueWriteError('changed on GitHub', {
    status: 409,
    kind: 'changed_on_github',
    disposition: 'not-recorded',
    current: { state: 'open', labels: ['status:todo'], assignees: [], status: 'todo', updatedAt: '2026-11-24T12:00:00Z' },
  });
  expect(mayHaveBeenRecorded(changed)).toBe(false);
  expect(classifyWriteFailure(changed).outcome).toBe('changed_on_github');

  expect(mayHaveBeenRecorded(error(503, 'unverified', 'not-recorded'), { isReplay: true })).toBe(true);
  expect(mayHaveBeenRecorded(error(403, 'forbidden', 'not-recorded'), { isReplay: true })).toBe(true);
  expect(classifyWriteFailure(error(503, 'unverified', 'not-recorded'), { isReplay: true }).outcome).toBe('possibly_recorded');
});

test('canReplayWithSameKey enforces the retention window and rejects undated or implausible timestamps', () => {
  const DAY_MS = 24 * 60 * 60 * 1000;
  const now = Date.parse('2026-11-24T12:00:00Z');
  expect(canReplayWithSameKey(new Date(now - 5 * 60 * 1000).toISOString(), now)).toBe(true);
  expect(canReplayWithSameKey(new Date(now - 28 * DAY_MS).toISOString(), now)).toBe(true);
  expect(canReplayWithSameKey(new Date(now - 29 * DAY_MS - 1000).toISOString(), now)).toBe(false);
  expect(canReplayWithSameKey(null, now)).toBe(false);
  expect(canReplayWithSameKey(undefined, now)).toBe(false);
  expect(canReplayWithSameKey('not-a-date', now)).toBe(false);
  expect(canReplayWithSameKey(new Date(now + 60 * 60 * 1000).toISOString(), now)).toBe(false);
});
