import assert from 'node:assert/strict';
import { api, parseEventStream } from '../src/api';
import { EventStreamDecoder } from '../src/sse';
import { mergeEvents } from '../src/timeline';
import type { HarnessEvent } from '../src/api';

const encoder = new TextEncoder();
const firstFrame = 'id: 1\r\nevent: run.created\r\ndata: {"name":"雪"}\r\n\r\n';
const halfSecondFrame = 'id: 2\r\nevent: tool.batch\r\ndata: {"calls":["半';
assert.deepEqual(parseEventStream(firstFrame + halfSecondFrame), [
  { seq: 1, type: 'run.created', data: '{"name":"雪"}' },
]);
const incremental = new EventStreamDecoder();
const decoded: HarnessEvent[] = [];
for (const byte of encoder.encode(firstFrame + halfSecondFrame)) decoded.push(...incremental.push(Uint8Array.of(byte)));
assert.deepEqual(decoded, [{ seq: 1, type: 'run.created', data: '{"name":"雪"}' }]);
assert.deepEqual(incremental.finish(), []);

const incomplete = new EventStreamDecoder();
assert.deepEqual(incomplete.push(encoder.encode('id: 9\nevent: run.completed\ndata: {}')), []);
assert.deepEqual(incomplete.finish(), []);

const completedAtEOF = new EventStreamDecoder();
assert.deepEqual(completedAtEOF.push(encoder.encode('id: 10\nevent: run.completed\ndata: {}\n\n')), [
  { seq: 10, type: 'run.completed', data: '{}' },
]);

Object.defineProperty(globalThis, 'localStorage', { value: { getItem: () => null }, configurable: true });
Object.defineProperty(globalThis, 'window', {
  value: { setTimeout: globalThis.setTimeout, clearTimeout: globalThis.clearTimeout }, configurable: true,
});
const firstChunk = encoder.encode(firstFrame + halfSecondFrame);
const secondConnection = 'id: 2\nevent: tool.batch\ndata: {"calls":["半"]}\n\nid: 2\nevent: tool.batch\ndata: {"calls":["半"]}\n\nid: 3\nevent: run.completed\ndata: {}\n\n';
const requests: { after: string | null; signal: AbortSignal }[] = [];
const responses = [
  new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(firstChunk);
      controller.close();
    },
  }), { status: 200 }),
  new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(encoder.encode(secondConnection));
      controller.close();
    },
  }), { status: 200 }),
];
Object.defineProperty(globalThis, 'fetch', {
  value: async (_url: string | URL | Request, options?: RequestInit) => {
    requests.push({ after: new Headers(options?.headers).get('Last-Event-ID'), signal: options?.signal as AbortSignal });
    return responses.shift()!;
  }, configurable: true,
});
const firstRead = await api.fetchEvents('run_test', 0);
assert.deepEqual(firstRead, [{ seq: 1, type: 'run.created', data: '{"name":"雪"}' }]);
const resumedRead = await api.fetchEvents('run_test', firstRead.at(-1)?.seq ?? 0);
assert.deepEqual(requests.map(({ after }) => after), [null, '1']);
const recovered = mergeEvents(firstRead, resumedRead);
assert.deepEqual(recovered.map(({ seq }) => seq), [1, 2, 3]);
assert.equal(recovered[1].data, '{"calls":["半"]}');

const splitChunks = encoder.encode(firstFrame);
const chunkedEvents: HarnessEvent[] = [];
Object.defineProperty(globalThis, 'fetch', {
  value: async (_url: string | URL | Request, _options?: RequestInit) => new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      for (const byte of splitChunks) controller.enqueue(Uint8Array.of(byte));
      controller.close();
    },
  }), { status: 200 }), configurable: true,
});
await api.streamEvents('run_test', 0, (event) => chunkedEvents.push(event), new AbortController().signal);
assert.deepEqual(chunkedEvents, [{ seq: 1, type: 'run.created', data: '{"name":"雪"}' }]);

const truncatedEvents: HarnessEvent[] = [];
Object.defineProperty(globalThis, 'fetch', {
  value: async (_url: string | URL | Request, _options?: RequestInit) => new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(encoder.encode(halfSecondFrame));
      controller.close();
    },
  }), { status: 200 }), configurable: true,
});
await api.streamEvents('run_test', 1, (event) => truncatedEvents.push(event), new AbortController().signal);
assert.deepEqual(truncatedEvents, []);

const originalSetTimeout = window.setTimeout;
window.setTimeout = ((callback: TimerHandler) => globalThis.setTimeout(callback, 10)) as typeof window.setTimeout;
Object.defineProperty(globalThis, 'fetch', {
  value: async (_url: string | URL | Request, options?: RequestInit) => new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(encoder.encode(halfSecondFrame));
      options?.signal?.addEventListener('abort', () => controller.error(new DOMException('Aborted', 'AbortError')));
    },
  }), { status: 200 }), configurable: true,
});
assert.deepEqual(await api.fetchEvents('run_test', 1), []);
window.setTimeout = originalSetTimeout;

const abortController = new AbortController();
const callbacks: HarnessEvent[] = [];
const abortBytes = encoder.encode(firstFrame + halfSecondFrame);
Object.defineProperty(globalThis, 'fetch', {
  value: async (_url: string | URL | Request, options?: RequestInit) => new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(abortBytes);
      options?.signal?.addEventListener('abort', () => controller.error(new DOMException('Aborted', 'AbortError')));
    },
  }), { status: 200 }), configurable: true,
});
const timer = globalThis.setTimeout(() => abortController.abort(), 10);
await api.streamEvents('run_test', 0, (event) => callbacks.push(event), abortController.signal).catch((error: unknown) => {
  if (!(error instanceof DOMException && error.name === 'AbortError')) throw error;
});
globalThis.clearTimeout(timer);
assert.deepEqual(callbacks, [{ seq: 1, type: 'run.created', data: '{"name":"雪"}' }]);

console.log('incremental SSE boundaries, UTF-8/CRLF, EOF, abort and reconnect: ok');
