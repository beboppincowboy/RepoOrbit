import { test, expect } from '@playwright/test';
import { disableStatusLabels, recheckWriteOperation, submitIssueUpdate } from '../src/features/issues/api';
import { IssueWriteError, canReplayWithSameKey, classifyWriteFailure, mayHaveBeenRecorded, toIssueWriteError } from '../src/features/issues/write-errors';
import { GraphQLUnavailableError } from '../src/shared/api/errors';
import { graphqlQuery } from '../src/shared/api/transport';

async function withFetch<T>(fake: () => Promise<Response>, work: () => Promise<T>): Promise<T> {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = fake as typeof fetch;
  try {
    return await work();
  } finally {
    globalThis.fetch = originalFetch;
  }
}

const rejection = (promise: Promise<unknown>): Promise<unknown> => promise.then(
  () => { throw new Error('Expected a rejection'); },
  (error: unknown) => error,
);

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

test('graphqlQuery reports an unreachable server as GraphQLUnavailableError', async () => {
  const error = await withFetch(async () => { throw new TypeError('Failed to fetch'); }, () => rejection(graphqlQuery('query { x }', {})));
  expect(error).toBeInstanceOf(GraphQLUnavailableError);
  expect(error).toMatchObject({ message: 'RepoOrbit could not reach the server.' });
});

test('graphqlQuery rethrows the fetch error when the request was cancelled', async () => {
  const controller = new AbortController();
  controller.abort();
  const thrown = new TypeError('Cancelled');
  const error = await withFetch(async () => { throw thrown; }, () => rejection(graphqlQuery('query { x }', {}, controller.signal)));
  expect(error).toBe(thrown);
  expect(error).not.toBeInstanceOf(GraphQLUnavailableError);
});

test('graphqlQuery reports temporary gateway errors as GraphQLUnavailableError', async () => {
  for (const status of [502, 503, 504]) {
    const error = await withFetch(async () => new Response(null, { status }), () => rejection(graphqlQuery('query { x }', {})));
    expect(error).toBeInstanceOf(GraphQLUnavailableError);
    expect(error).toMatchObject({ message: 'The server is temporarily unavailable.' });
  }
});

test('an issue write that gets no answer may have been recorded', async () => {
  for (const [send, message] of [
    [() => submitIssueUpdate('repo-1', 3, 'ro-key', {}), 'No reliable answer from the server. Your draft is preserved; check Write history before retrying.'],
    [() => recheckWriteOperation(43, 3), 'Could not reach the server to recheck this operation. Try again shortly.'],
    [() => disableStatusLabels('repo-1'), 'Could not reach the server. Check the setting before trying again.'],
  ] as const) {
    const error = await withFetch(async () => { throw new TypeError('Failed to fetch'); }, () => rejection(send()));
    expect(error).toBeInstanceOf(IssueWriteError);
    expect(error).toMatchObject({ message, status: 0, kind: 'transport' });
    expect(mayHaveBeenRecorded(error)).toBe(true);
    expect(classifyWriteFailure(error).outcome).toBe('possibly_recorded');
  }
});

test('toIssueWriteError maps gateway statuses to uncertain write failures', async () => {
  for (const [status, kind, message] of [
    [502, 'transport', 'No reliable answer from the server. Your draft is preserved; check Write history before retrying.'],
    [503, 'unverified', 'GitHub access could not be verified right now. Try again shortly.'],
    [504, 'transport', 'The request timed out before a reliable answer arrived. Your draft is preserved; check Write history before retrying.'],
  ] as const) {
    const error = await toIssueWriteError(new Response(null, { status }));
    expect(error).toMatchObject({ message, status, kind, disposition: null, retryAfter: null });
    expect(mayHaveBeenRecorded(error)).toBe(true);
    expect(classifyWriteFailure(error).outcome).toBe('possibly_recorded');
  }
});

test('toIssueWriteError reads Retry-After as seconds or an HTTP date', async () => {
  const retryAfter = async (header: string | null) =>
    (await toIssueWriteError(new Response(null, { status: 503, headers: header === null ? {} : { 'Retry-After': header } }))).retryAfter;
  const before = Date.now();
  const fromSeconds = Date.parse((await retryAfter('30'))!);
  expect(fromSeconds).toBeGreaterThanOrEqual(before + 30_000);
  expect(fromSeconds).toBeLessThanOrEqual(Date.now() + 30_000);
  expect(await retryAfter('Wed, 25 Nov 2026 12:05:00 GMT')).toBe('2026-11-25T12:05:00.000Z');
  expect(await retryAfter('soon')).toBeNull();
  expect(await retryAfter(null)).toBeNull();
});

test('toIssueWriteError applies each RepoOrbit-Write-Disposition value', async () => {
  // recorded lists, per status, whether the write may have been recorded.
  for (const { header, disposition, recorded } of [
    { header: 'not-recorded', disposition: 'not-recorded', recorded: { 403: false, 502: true, 503: false, 504: true } },
    { header: ' Not-Recorded ', disposition: 'not-recorded', recorded: { 403: false, 502: true, 503: false, 504: true } },
    { header: 'possibly-recorded', disposition: 'possibly-recorded', recorded: { 403: true, 502: true, 503: true, 504: true } },
    { header: 'unknown', disposition: null, recorded: { 403: false, 502: true, 503: true, 504: true } },
    { header: null, disposition: null, recorded: { 403: false, 502: true, 503: true, 504: true } },
  ]) {
    for (const status of [403, 502, 503, 504] as const) {
      const headers: Record<string, string> = header === null ? {} : { 'RepoOrbit-Write-Disposition': header };
      const error = await toIssueWriteError(new Response(null, { status, headers }));
      const label = `${status} with disposition header ${JSON.stringify(header)}`;
      expect(error.disposition, label).toBe(disposition);
      expect(mayHaveBeenRecorded(error), label).toBe(recorded[status]);
      expect(classifyWriteFailure(error).outcome, label).toBe(recorded[status] ? 'possibly_recorded' : 'not_recorded');
    }
  }
});
