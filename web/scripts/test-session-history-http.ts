import assert from 'node:assert/strict';
import { once } from 'node:events';
import { createServer } from 'node:http';
import type { ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';
import { test } from 'node:test';
import { api } from '../src/api';
import type { SessionMessage } from '../src/api';

// Real loopback HTTP fixture, not the backend handler/provider or a browser.
// Oracle: docs/api.md (default 50, maximum 200, stable IDs, tuple ordering).
const stamp = '2026-11-25T09:00:01.123456789Z';
function fact(id: string, session: string, fields: Partial<SessionMessage> = {}): SessionMessage {
  return { id, session_id: session, run_id: null, turn_id: null, role: 'assistant',
    kind: 'response', status: 'recorded', content: '合成持久正文 🌨️\r\n第二行',
    event: '', created_at: stamp, ...fields };
}
function cursor(session: string, id: string, createdAt = stamp): string {
  return Buffer.from(JSON.stringify({ v: 1, session_id: session, created_at: createdAt, id })).toString('base64url');
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
function deadline<T>(promise: Promise<T>, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(label + ' timed out')), 4000);
  });
  return Promise.race([promise, timeout]).finally(() => { if (timer) clearTimeout(timer); });
}

type Seen = { path: string; session: string; query: URLSearchParams; authorization: string | undefined };
type Reply = { status?: number; body?: unknown; raw?: string; hold?: 'headers' | 'body' };
const requests: Seen[] = [];
const serverErrors: unknown[] = [];
const holds = new Map<string, { arrived: ReturnType<typeof deferred<void>>; closed: ReturnType<typeof deferred<void>>; response?: ServerResponse }>();
for (const id of ['ses_abort', 'ses_old', 'ses_preaborted']) {
  holds.set(id, { arrived: deferred<void>(), closed: deferred<void>() });
}

// 253 distinct facts cross both the default 50 and maximum 200 boundaries.
// The first 250 share one timestamp; every legal page size splits a tie.
// This list is already in golden order; no production sorting is reused.
const many = Array.from({ length: 253 }, (_, index) => fact('msg_response_' + String(index).padStart(3, '0'), 'ses_many', {
  run_id: index % 2 === 0 ? 'run_a' : 'run_b',
  created_at: index < 250 ? stamp : '2026-11-25T09:00:01.123456790Z',
}));
const positions = new Map([['', 0], ...many.map((row, index): [string, number] => [cursor('ses_many', row.id, row.created_at), index + 1])]);
const escapedSession = 'ses /?#雪%';
// Exact documented cursor, independent of the fixture encoder above.
const goldenCursor = 'eyJ2IjoxLCJzZXNzaW9uX2lkIjoic2VzX2RlbW8iLCJjcmVhdGVkX2F0IjoiMjAyNi0xMS0yNVQwOTowMDowMS4wMDAwMDAwMDBaIiwiaWQiOiJtc2dfcmVzcG9uc2VfcnNwX2RlbW8ifQ';

function replyFor(session: string, query: URLSearchParams): Reply {
  if (session === 'ses_many') {
    const rawLimit = query.get('limit') ?? '50';
    assert.match(rawLimit, /^[1-9][0-9]*$/);
    const limit = Number(rawLimit);
    assert.ok(limit <= 200, 'oversized page request');
    const start = positions.get(query.get('cursor') ?? '');
    assert.notEqual(start, undefined, 'unexpected continuation');
    const messages = many.slice(start!, start! + limit);
    const last = messages.at(-1);
    return { body: { messages, next_cursor: last && start! + messages.length < many.length ? cursor(session, last.id, last.created_at) : '' } };
  }
  if (session === escapedSession || session === 'ses_demo') {
    return { body: { messages: [fact('msg_response_direct', session)], next_cursor: '' } };
  }
  if (session === 'ses_empty') return { body: { messages: [], next_cursor: '' } };
  if (session === 'ses_exact_200') {
    assert.equal(query.get('limit'), '200');
    return { body: { messages: many.slice(0, 200).map((row) => ({ ...row, session_id: session })), next_cursor: '' } };
  }
  if (session === 'ses_unauthorized') return { status: 401, body: { error: { code: 'auth_denied', message: 'fixture auth failure' } } };
  if (session === 'ses_missing') return { status: 404, body: { error: { code: 'not_found', message: 'record not found', ref: session } } };
  if (session === 'ses_error') return { status: 500, body: { error: { code: 'internal', message: 'request failed' } } };
  if (session === 'ses_unavailable') return { status: 503, raw: 'fixture unavailable' };
  if (session === 'ses_bad_json') return { raw: '{"messages":[' };
  if (session === 'ses_late_error') {
    if (!(query.get('cursor') ?? '')) return { body: { messages: [fact('msg_response_first', session)], next_cursor: 'next' } };
    return { status: 500, body: { error: { code: 'internal', message: 'request failed' } } };
  }
  if (session === 'ses_abort') {
    if (!(query.get('cursor') ?? '')) return { body: { messages: [fact('msg_response_partial', session)], next_cursor: 'next' } };
    return { hold: 'body' };
  }
  if (session === 'ses_old' || session === 'ses_preaborted') return { hold: 'headers' };
  if (session === 'ses_new') return { body: { messages: [fact('msg_response_new', session)], next_cursor: '' } };
  throw new Error('unexpected fixture session: ' + session);
}

const server = createServer((request, response) => {
  try {
    const url = new URL(request.url ?? '', 'http://fixture.invalid');
    const match = /^\/v1\/sessions\/([^/]+)\/messages$/.exec(url.pathname);
    assert.ok(match, 'unexpected HTTP route');
    const session = decodeURIComponent(match[1]);
    requests.push({ path: url.pathname, session, query: url.searchParams, authorization: request.headers.authorization });
    assert.equal(request.method, 'GET');
    assert.equal(url.searchParams.getAll('limit').length <= 1, true);
    assert.equal(url.searchParams.getAll('cursor').length <= 1, true);
    const reply = replyFor(session, url.searchParams);
    if (reply.hold) {
      const held = holds.get(session)!;
      held.response = response;
      response.once('close', () => held.closed.resolve());
      if (reply.hold === 'body') {
        response.writeHead(200, { 'Content-Type': 'application/json' });
        response.write('{"messages":[');
      }
      held.arrived.resolve();
      return;
    }
    response.writeHead(reply.status ?? 200, { 'Content-Type': 'application/json' });
    response.end(reply.raw ?? JSON.stringify(reply.body));
  } catch (error) {
    serverErrors.push(error);
    response.destroy(error instanceof Error ? error : undefined);
  }
});

server.listen(0, '127.0.0.1');
await once(server, 'listening');
const origin = 'http://127.0.0.1:' + (server.address() as AddressInfo).port;
const originalFetch = globalThis.fetch;
const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
const values = new Map<string, string>();
const sentSignals: (AbortSignal | null | undefined)[] = [];
Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: {
  getItem: (key: string) => values.get(key) ?? null,
  setItem: () => { throw new Error('history must not store transcript locally'); },
} });
globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
  assert.equal(typeof input, 'string');
  assert.ok(String(input).startsWith('/v1/sessions/'), 'only same-origin history calls are permitted');
  sentSignals.push(init?.signal);
  return originalFetch(new URL(String(input), origin), init);
}) as typeof fetch;

try {
  await test('HTTP session history public contract', async (suite) => {
    await suite.test('HTTP listMessages: encoded path, opaque cursor, limit, signal and bearer token', async () => {
      values.set('cloud-harness.http-token', 'synthetic-test-token');
      const controller = new AbortController();
      const sentinel = 'opaque+/%=&?# 雪'; // Transport sentinel, not a valid server cursor v1.
      const result = await deadline(api.listMessages(escapedSession, { limit: 200, cursor: sentinel, signal: controller.signal }), 'direct page');
      assert.deepEqual(result, { messages: [fact('msg_response_direct', escapedSession)], next_cursor: '' });
      const seen = requests.at(-1)!;
      assert.equal(seen.path, '/v1/sessions/' + encodeURIComponent(escapedSession) + '/messages');
      assert.deepEqual([...seen.query.keys()].sort(), ['cursor', 'limit']);
      assert.deepEqual(seen.query.getAll('cursor'), [sentinel]);
      assert.deepEqual(seen.query.getAll('limit'), ['200']);
      assert.equal(seen.authorization, 'Bearer synthetic-test-token');
      assert.equal(sentSignals.at(-1), controller.signal);
      values.clear(); // All later loads start without saved transcript state/token.
    });

    await suite.test('HTTP listMessages: documented v1 cursor is preserved byte for byte', async () => {
      await deadline(api.listMessages('ses_demo', { limit: 1, cursor: goldenCursor }), 'golden cursor');
      assert.deepEqual(requests.at(-1)!.query.getAll('cursor'), [goldenCursor]);
      assert.equal(requests.at(-1)!.authorization, undefined);
    });

    await suite.test('HTTP listMessages: omitted limit returns 50; explicit maximum returns 200 without truncation', async () => {
      const first = await deadline(api.listMessages('ses_many'), 'default page');
      assert.deepEqual(first.messages, many.slice(0, 50));
      assert.equal(first.next_cursor, cursor('ses_many', 'msg_response_049'));
      const maximum = await deadline(api.listMessages('ses_many', { limit: 200 }), 'maximum page');
      assert.deepEqual(maximum.messages, many.slice(0, 200));
      assert.equal(maximum.next_cursor, cursor('ses_many', 'msg_response_199'));
      const remainder = await deadline(api.listMessages('ses_many', { limit: 200, cursor: maximum.next_cursor }), 'beyond 200');
      assert.deepEqual(remainder, { messages: many.slice(200), next_cursor: '' });
      const exact = await deadline(api.listMessages('ses_exact_200', { limit: 200 }), 'exact 200');
      assert.equal(exact.messages.length, 200);
      assert.equal(exact.next_cursor, '', 'exactly limit records without a successor have no continuation');
    });

    for (const [session, pattern] of [
      ['ses_unauthorized', /token/i], ['ses_missing', /record not found/],
      ['ses_error', /request failed/], ['ses_unavailable', /503/],
    ] as const) {
      await suite.test('HTTP listMessages: ' + session + ' rejects with existing HTTP error semantics', async () => {
        await assert.rejects(deadline(api.listMessages(session), session), pattern);
      });
    }
    await suite.test('HTTP listMessages: invalid JSON cannot masquerade as an empty transcript', async () => {
      await assert.rejects(deadline(api.listMessages('ses_bad_json'), 'bad JSON'));
    });

    // Real dependency: baseline failure is intentional when the sibling module
    // is absent. Never skip these checks or install a fake production module.
    const { loadSessionMessages, mergeSessionMessages } = await import('../src/history');

    await suite.test('HTTP load: all 253 facts cross timestamp ties and a one-nanosecond boundary', async () => {
      const offset = requests.length;
      const signalOffset = sentSignals.length;
      const controller = new AbortController();
      const result = await deadline(loadSessionMessages('ses_many', { signal: controller.signal }), 'all pages');
      assert.deepEqual(result, many);
      assert.equal(new Set(result.map((row) => row.id)).size, 253);
      const seen = requests.slice(offset);
      assert.ok(seen.length >= 2, 'must traverse past a 200-record page');
      let consumed = 0;
      for (const row of seen) {
        assert.equal(row.session, 'ses_many');
        assert.equal(row.authorization, undefined);
        const previous = many[consumed - 1];
        assert.equal(row.query.get('cursor') ?? '', previous ? cursor('ses_many', previous.id, previous.created_at) : '');
        consumed = Math.min(253, consumed + Number(row.query.get('limit') ?? 50));
      }
      assert.equal(consumed, 253);
      assert.ok(sentSignals.slice(signalOffset).every((signal) => signal === controller.signal));
      assert.deepEqual(mergeSessionMessages(result, result), many, 'full refresh must not duplicate durable facts');
    });

    await suite.test('HTTP load: empty session succeeds; later-page 500 delivers no partial history', async () => {
      assert.deepEqual(await deadline(loadSessionMessages('ses_empty'), 'empty session'), []);
      const offset = requests.length;
      await assert.rejects(deadline(loadSessionMessages('ses_late_error'), 'later-page failure'), /request failed/);
      assert.equal(requests.length - offset, 2);
    });

    await suite.test('HTTP load: cancelling a partially received second page rejects and closes HTTP', async () => {
      const controller = new AbortController();
      const held = holds.get('ses_abort')!;
      const loading = deadline(loadSessionMessages('ses_abort', { signal: controller.signal }), 'aborted body');
      const outcome = loading.then(() => null, (error: unknown) => error);
      try {
        await deadline(held.arrived.promise, 'second page arrival');
        controller.abort();
        const error = await outcome;
        assert.ok(error instanceof Error && error.name === 'AbortError');
        await deadline(held.closed.promise, 'aborted connection close');
      } finally { controller.abort(); held.response?.destroy(); }
    });

    await suite.test('HTTP load: old/new sessions stay separate and old cancellation is forwarded', async () => {
      // App owns stale-result adoption. This checks client isolation/abort only,
      // and does not claim that App/Chat rendering was exercised.
      const controller = new AbortController();
      const held = holds.get('ses_old')!;
      const old = deadline(loadSessionMessages('ses_old', { signal: controller.signal }), 'old session');
      const outcome = old.then(() => null, (error: unknown) => error);
      try {
        await deadline(held.arrived.promise, 'old session arrival');
        const current = await deadline(loadSessionMessages('ses_new'), 'new session');
        assert.deepEqual(current, [fact('msg_response_new', 'ses_new')]);
        controller.abort();
        const error = await outcome;
        assert.ok(error instanceof Error && error.name === 'AbortError');
        await deadline(held.closed.promise, 'old connection close');
        assert.deepEqual(current, [fact('msg_response_new', 'ses_new')]);
      } finally { controller.abort(); held.response?.destroy(); }
    });

    await suite.test('HTTP load: a pre-aborted signal never produces an empty success', async () => {
      const controller = new AbortController();
      controller.abort();
      await assert.rejects(deadline(loadSessionMessages('ses_preaborted', { signal: controller.signal }), 'pre-aborted load'),
        (error: unknown) => error instanceof Error && error.name === 'AbortError');
    });
    assert.deepEqual(serverErrors, []);
  });
} finally {
  globalThis.fetch = originalFetch;
  if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage);
  else Reflect.deleteProperty(globalThis, 'localStorage');
  server.closeAllConnections();
  await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
}
