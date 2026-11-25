import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { api } from '../src/api';
import type { HarnessEvent } from '../src/api';
import { mergeEvents } from '../src/timeline';

const encoder = new TextEncoder();
const requests: { path: string; lastEventId: string | null; closed: Promise<void> }[] = [];
const requestErrors: unknown[] = [];
let markPartialFrameSent!: () => void;
const partialFrameSent = new Promise<void>((resolve) => { markPartialFrameSent = resolve; });
const server = createServer((request, response) => {
  const closed = new Promise<void>((resolve) => response.once('close', resolve));
  const lastEventId = request.headers['last-event-id'];
  requests.push({ path: request.url || '', lastEventId: Array.isArray(lastEventId) ? lastEventId[0] ?? null : lastEventId ?? null, closed });
  void reply(requests.length, response).catch((error: unknown) => {
    requestErrors.push(error);
    response.destroy(error instanceof Error ? error : undefined);
  });
});

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => globalThis.setTimeout(resolve, ms));
}

function frame(seq: number, type: string, data: string): string {
  return 'id: ' + seq + '\r\nevent: ' + type + '\r\ndata: ' + data + '\r\n\r\n';
}

async function writeBytewise(response: import('node:http').ServerResponse, body: Uint8Array): Promise<void> {
  for (let offset = 0; offset < body.byteLength; offset += 1) {
    response.write(body.subarray(offset, offset + 1));
    await wait(1);
  }
}

async function reply(index: number, response: import('node:http').ServerResponse): Promise<void> {
  response.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
  });
  response.flushHeaders();

  if (index === 1) {
    const interrupted = frame(1, 'run.created', '{"label":"雪"}') + 'id: 2\r\nevent: tool.batch\r\ndata: {"calls":["半';
    await writeBytewise(response, encoder.encode(interrupted));
    response.end();
    return;
  }
  if (index === 2) {
    response.end([
      frame(2, 'tool.batch', '{"calls":["半"]}'),
      frame(2, 'tool.batch', '{"calls":["半"]}'),
      frame(3, 'run.completed', '{}'),
    ].join(''));
    return;
  }
  if (index === 3) {
    response.write(frame(4, 'run.resumed', '{}'));
    response.write('id: 5\r\nevent: tool.batch\r\ndata: {"calls":["不完整', markPartialFrameSent);
    return;
  }
  if (index === 4) {
    response.end([frame(5, 'tool.batch', '{"calls":["恢复"]}'), frame(6, 'run.completed', '{}')].join(''));
    return;
  }
  if (index === 5) {
    response.write(frame(7, 'run.completed', '{}'));
    return;
  }
  response.writeHead(404).end();
}

function withDeadline<T>(promise: Promise<T>, label: string): Promise<T> {
  let timer: ReturnType<typeof globalThis.setTimeout> | undefined;
  const deadline = new Promise<never>((_, reject) => {
    timer = globalThis.setTimeout(() => reject(new Error(label + ' timed out')), 3000);
  });
  return Promise.race([promise, deadline]).finally(() => {
    if (timer) globalThis.clearTimeout(timer);
  });
}

function lastSequence(events: HarnessEvent[], fallback: number): number {
  return events.reduce((last, event) => event.seq > 0 ? Math.max(last, event.seq) : last, fallback);
}

await new Promise<void>((resolve, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', resolve);
});
const address = server.address() as AddressInfo;
const origin = 'http://127.0.0.1:' + address.port;
const originalFetch = globalThis.fetch;
const originalWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
const originalLocalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
const apiTimers = new Set<ReturnType<typeof globalThis.setTimeout>>();

Object.defineProperty(globalThis, 'localStorage', { value: { getItem: () => null }, configurable: true });
Object.defineProperty(globalThis, 'window', {
  value: {
    setTimeout: ((callback: TimerHandler, delay?: number) => {
      if (typeof callback !== 'function') throw new TypeError('expected a timer callback');
      let timer: ReturnType<typeof globalThis.setTimeout>;
      timer = globalThis.setTimeout(() => {
        apiTimers.delete(timer);
        (callback as () => void)();
      }, delay);
      apiTimers.add(timer);
      return timer as unknown as number;
    }) as typeof window.setTimeout,
    clearTimeout: ((handle?: number) => {
      if (handle === undefined) return;
      const timer = handle as unknown as ReturnType<typeof globalThis.setTimeout>;
      apiTimers.delete(timer);
      globalThis.clearTimeout(timer);
    }) as typeof window.clearTimeout,
  },
  configurable: true,
});
globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
  const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname + input.search : input.url;
  if (!path.startsWith('/')) return Promise.reject(new Error('unexpected non-relative request: ' + path));
  return originalFetch(new URL(path, origin), init);
}) as typeof fetch;

try {
  const firstConnection: HarnessEvent[] = [];
  await withDeadline(api.streamEvents('run_http', 0, (event) => firstConnection.push(event), new AbortController().signal), 'initial stream');
  assert.deepEqual(firstConnection, [{ seq: 1, type: 'run.created', data: '{"label":"雪"}' }]);
  await withDeadline(requests[0].closed, 'initial connection close');

  let events = mergeEvents([], firstConnection);
  const reconnect: HarnessEvent[] = [];
  await withDeadline(api.streamEvents('run_http', lastSequence(events, 0), (event) => reconnect.push(event), new AbortController().signal), 'reconnect stream');
  events = mergeEvents(events, reconnect);
  assert.deepEqual(reconnect.map(({ seq }) => seq), [2, 2, 3]);
  assert.deepEqual(events.map(({ seq }) => seq), [1, 2, 3]);
  await withDeadline(requests[1].closed, 'reconnect connection close');

  const aborted: HarnessEvent[] = [];
  const controller = new AbortController();
  let markCompleteFrameSeen!: () => void;
  const completeFrameSeen = new Promise<void>((resolve) => { markCompleteFrameSeen = resolve; });
  const interruptedStream = withDeadline(api.streamEvents('run_http', lastSequence(events, 0), (event) => {
    aborted.push(event);
    if (event.seq === 4) markCompleteFrameSeen();
  }, controller.signal), 'aborted stream');
  const abortWhenReady = withDeadline(Promise.all([partialFrameSent, completeFrameSeen]), 'abort boundary')
    .then(() => controller.abort());
  try {
    await Promise.all([interruptedStream, abortWhenReady]);
  } finally {
    controller.abort();
  }
  assert.deepEqual(aborted.map(({ seq }) => seq), [4]);
  events = mergeEvents(events, aborted);
  const cursorAfterAbort = lastSequence(events, 0);
  assert.equal(cursorAfterAbort, 4);
  await withDeadline(requests[2].closed, 'aborted connection close');

  const resumed: HarnessEvent[] = [];
  await withDeadline(api.streamEvents('run_http', cursorAfterAbort, (event) => resumed.push(event), new AbortController().signal), 'post-abort reconnect');
  events = mergeEvents(events, resumed);
  assert.deepEqual(events.map(({ seq }) => seq), [1, 2, 3, 4, 5, 6]);
  await withDeadline(requests[3].closed, 'post-abort connection close');

  const polled = await withDeadline(api.fetchEvents('run_http', lastSequence(events, 0)), 'event poll');
  events = mergeEvents(events, polled);
  assert.deepEqual(polled.map(({ seq }) => seq), [7]);
  assert.deepEqual(events.map(({ seq }) => seq), [1, 2, 3, 4, 5, 6, 7]);
  await withDeadline(requests[4].closed, 'poll connection close');

  assert.deepEqual(requests.map(({ path, lastEventId }) => [path, lastEventId]), [
    ['/v1/runs/run_http/events', null],
    ['/v1/runs/run_http/events', '1'],
    ['/v1/runs/run_http/events', '3'],
    ['/v1/runs/run_http/events', '4'],
    ['/v1/runs/run_http/events', '6'],
  ]);
  assert.deepEqual(requestErrors, []);
  assert.equal(apiTimers.size, 0);
  console.log('loopback HTTP SSE: fragmented UTF-8/CRLF, reconnect, merge, abort cursor and cleanup: ok');
} finally {
  globalThis.fetch = originalFetch;
  for (const timer of apiTimers) globalThis.clearTimeout(timer);
  server.closeAllConnections();
  await new Promise<void>((resolve, reject) => {
    server.close((error) => error ? reject(error) : resolve());
  });
  if (originalWindow) Object.defineProperty(globalThis, 'window', originalWindow);
  else Reflect.deleteProperty(globalThis, 'window');
  if (originalLocalStorage) Object.defineProperty(globalThis, 'localStorage', originalLocalStorage);
  else Reflect.deleteProperty(globalThis, 'localStorage');
}
