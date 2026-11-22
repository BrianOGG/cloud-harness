import assert from 'node:assert/strict';
import { api, parseEventStream } from '../src/api';
import type { HarnessEvent } from '../src/api';
import { budgetExtension, getBudgetCounters } from '../src/budget';

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

// Creating and resuming a run returns at once; the stream reports progress.
const posted: string[] = [];
const bodies: string[] = [];
Object.defineProperty(globalThis, 'fetch', {
  value: async (url: string | URL | Request, options?: RequestInit) => {
    posted.push(String(url));
    bodies.push(String(options?.body ?? ''));
    return new Response('{}', { status: 200 });
  },
  configurable: true,
});
await api.createRun('ses_test', 'hello');
await api.respond('run_test', { decision: 'approve' });
await api.addBudget('run_test', { extra_tokens: 500 });
assert.deepEqual(posted, ['/v1/sessions/ses_test/runs?wait=0', '/v1/runs/run_test/responses?wait=0', '/v1/runs/run_test/budget?wait=0']);
assert.equal(bodies[2], '{"extra_tokens":500}');

const calls = {
  max_model_calls: 10, used_model_calls: 3,
  max_tool_calls: 20, used_tool_calls: 8,
  max_script_calls: 4, used_script_calls: 1,
  max_child_runs: 2, used_child_runs: 1,
};
// A run from a server without token accounting, then one with limits.
assert.deepEqual(getBudgetCounters(calls).map((counter) => `${counter.usedText} / ${counter.maxText}`), ['3 / 10', '8 / 20', '1 / 4', '1 / 2', '0 / 不限', '$0.00 / 不限']);
const limited = { ...calls, max_tokens: 1000, used_input_tokens: 900, used_output_tokens: 300, used_cached_input_tokens: 500, max_cost_usd: 2, used_cost_usd: 0.004 };
assert.deepEqual(getBudgetCounters(limited).slice(4).map((counter) => `${counter.usedText} / ${counter.maxText}`), ['1,200 / 1,000', '$0.0040 / $2.00']);

// The budget wait raises every limit the run has reached, and only those.
assert.deepEqual(budgetExtension({ ...calls, used_model_calls: 10 }), { title: '模型调用预算已用完', label: '增加 10 次模型调用', extension: { extra_model_calls: 10 } });
assert.deepEqual(budgetExtension(limited), { title: 'Token 预算已用完', label: '增加 700 个 token', extension: { extra_tokens: 700 } });
assert.deepEqual(budgetExtension({ ...limited, max_tokens: 0, used_cost_usd: 2.004 }), { title: '费用预算已用完', label: '增加 $1.01 费用上限', extension: { extra_cost_usd: 1.01 } });
assert.deepEqual(budgetExtension({ ...limited, used_model_calls: 10 }), { title: '运行预算已用完', label: '增加 10 次模型调用、700 个 token', extension: { extra_tokens: 700, extra_model_calls: 10 } });

console.log('event continuation, async run requests, budget counters and budget extensions: ok');
