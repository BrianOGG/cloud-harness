import assert from 'node:assert/strict';
import { api, parseEventStream } from '../src/api';
import type { HarnessEvent } from '../src/api';
import { getBudgetCounters } from '../src/budget';

const parsed = parseEventStream('id: 7\r\nevent: run.created\r\ndata: {"source":"test"}\r\n\r\nid: 8\nevent: tool.batch\ndata: line one\ndata: line two\n\n');
assert.deepEqual(parsed, [
  { seq: 7, type: 'run.created', data: '{"source":"test"}' },
  { seq: 8, type: 'tool.batch', data: 'line one\nline two' },
]);

Object.defineProperty(globalThis, 'localStorage', { value: { getItem: () => 'test-token' }, configurable: true });
Object.defineProperty(globalThis, 'window', { value: globalThis, configurable: true });
let captured: { url: string; options: RequestInit } | undefined;
Object.defineProperty(globalThis, 'fetch', {
  value: async (url: string | URL | Request, options?: RequestInit) => {
    captured = { url: String(url), options: options || {} };
    return new Response('id: 9\nevent: run.completed\ndata: {"ok":true}\n\n', { status: 200 });
  },
  configurable: true,
});
assert.deepEqual(await api.fetchEvents('run_test', 8), [
  { seq: 9, type: 'run.completed', data: '{"ok":true}' },
]);
assert.equal(captured?.url, '/v1/runs/run_test/events');
assert.equal(new Headers(captured?.options.headers).get('Last-Event-ID'), '8');
assert.equal(new Headers(captured?.options.headers).get('Authorization'), 'Bearer test-token');

// A held-open stream delivers persisted events and live text in arrival
// order, however the bytes are split across reads.
const wire = [
  'id: 1\nevent: run.created\ndata: {}\n\n',
  'event: stream.delta\ndata: {"text":"Hel"}\n\n',
  ': ping\n\n',
  'event: stream.delta\r\ndata: {"text":"lo"}\r\n\r\n',
  'event: stream.reset\ndata: null\n\n',
  'event: stream.delta\ndata: {"text":"Hi 世界"}\n\n',
  'id: 2\nevent: model.completed\ndata: {}\n\nid: 3\nevent: run.completed\ndata: {}\n\n',
].join('');
const wireBytes = new TextEncoder().encode(wire);
for (const size of [1, 7, wireBytes.length]) {
  Object.defineProperty(globalThis, 'fetch', {
    value: async (url: string | URL | Request, options?: RequestInit) => {
      captured = { url: String(url), options: options || {} };
      return new Response(new ReadableStream<Uint8Array>({
        start(controller) {
          for (let at = 0; at < wireBytes.length; at += size) controller.enqueue(wireBytes.slice(at, at + size));
          controller.close();
        },
      }), { status: 200 });
    },
    configurable: true,
  });
  const seen: string[] = [];
  await api.streamRun('run_test', 0, {
    onEvents: (events: HarnessEvent[]) => seen.push(...events.map((event) => `${event.seq}:${event.type}`)),
    onDelta: (text) => seen.push(`delta:${text}`),
    onReset: () => seen.push('reset'),
  }, new AbortController().signal);
  assert.deepEqual(seen, ['1:run.created', 'delta:Hel', 'delta:lo', 'reset', 'delta:Hi 世界', '2:model.completed', '3:run.completed'], `chunk size ${size}`);
  assert.equal(captured?.url, '/v1/runs/run_test/events');
  assert.equal(new Headers(captured?.options.headers).get('Last-Event-ID'), null);
}

// Creating and resuming a run returns at once; the stream reports progress.
const posted: string[] = [];
Object.defineProperty(globalThis, 'fetch', {
  value: async (url: string | URL | Request) => {
    posted.push(String(url));
    return new Response('{}', { status: 200 });
  },
  configurable: true,
});
await api.createRun('ses_test', 'hello');
await api.respond('run_test', { decision: 'approve' });
await api.addBudget('run_test', 10);
assert.deepEqual(posted, ['/v1/sessions/ses_test/runs?wait=0', '/v1/runs/run_test/responses?wait=0', '/v1/runs/run_test/budget?wait=0']);

const counters = getBudgetCounters({
  max_model_calls: 10, used_model_calls: 3,
  max_tool_calls: 20, used_tool_calls: 8,
  max_script_calls: 4, used_script_calls: 1,
  max_child_runs: 2, used_child_runs: 1,
});
assert.deepEqual(counters.map((counter) => `${counter.used} / ${counter.max}`), ['3 / 10', '8 / 20', '1 / 4', '1 / 2']);

console.log('event continuation, live stream and four budget counters: ok');
