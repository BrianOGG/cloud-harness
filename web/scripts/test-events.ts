import assert from 'node:assert/strict';
import { api, parseEventStream } from '../src/api';
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

const counters = getBudgetCounters({
  max_model_calls: 10, used_model_calls: 3,
  max_tool_calls: 20, used_tool_calls: 8,
  max_script_calls: 4, used_script_calls: 1,
  max_child_runs: 2, used_child_runs: 1,
});
assert.deepEqual(counters.map((counter) => `${counter.used} / ${counter.max}`), ['3 / 10', '8 / 20', '1 / 4', '1 / 2']);

console.log('event continuation and four budget counters: ok');
