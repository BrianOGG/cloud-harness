import assert from 'node:assert/strict';
import { api, parseEventStream } from '../src/api';
import type { HarnessEvent } from '../src/api';
import { dayGroup } from '../src/format';
import { buildWork, deltaText, mergeEvents } from '../src/timeline';

// The event log of a run that waits for approval, runs two tools, delegates a child, and answers.
const log = parseEventStream([
  ['1', 'run.created', '{"session_id":"ses_1"}'],
  ['2', 'model.completed', '{"action":"EXECUTE","input_tokens":32,"output_tokens":12,"latency_ms":2}'],
  ['4', 'tool.batch', '{"calls":[{"name":"write_file","provider_call_id":"a","status":"WAITING_APPROVAL"},{"name":"list_files","provider_call_id":"b","status":"PENDING"}],"status":"waiting_approval"}'],
  ['5', 'run.WAITING', '{"status":"WAITING","wait_reason":"APPROVAL"}'],
  ['7', 'invocation.RUNNING', '{"provider_call_id":"a","status":"RUNNING","tool":"write_file"}'],
  ['8', 'invocation.SUCCEEDED', '{"provider_call_id":"a","status":"SUCCEEDED","tool":"write_file"}'],
  ['11', 'invocation.SUCCEEDED', '{"provider_call_id":"b","status":"SUCCEEDED","tool":"list_files"}'],
  ['12', 'model.completed', '{"action":"EXECUTE","input_tokens":40,"output_tokens":10,"latency_ms":3}'],
  ['14', 'invocation.RUNNING', '{"provider_call_id":"a","status":"RUNNING","tool":"delegate_task"}'],
  ['16', 'child.started', '{"agent":"explorer","child_run_id":"run_child"}'],
  ['18', 'child.finished', '{"agent":"explorer","child_run_id":"run_child","status":"COMPLETED"}'],
  ['20', 'tool.batch', '{"calls":[{"name":"delegate_task","provider_call_id":"a","status":"SUCCEEDED"}],"status":"complete"}'],
  ['21', 'model.completed', '{"action":"ANSWER","input_tokens":50,"output_tokens":20,"latency_ms":5}'],
  ['22', 'run.completed', '{"status":"COMPLETED"}'],
].map(([id, type, data]) => `id: ${id}\nevent: ${type}\ndata: ${data}\n\n`).join(''));

const work = buildWork(log);
assert.deepEqual(
  work.items.map((item) => (item.kind === 'tool' ? `${item.name}:${item.status}` : item.kind === 'model' ? item.action : item.kind === 'child' ? `child:${item.status}` : item.label)),
  ['EXECUTE', 'write_file:SUCCEEDED', 'list_files:SUCCEEDED', 'EXECUTE', 'delegate_task:SUCCEEDED', 'child:COMPLETED', 'ANSWER'],
);
assert.deepEqual([work.modelCalls, work.toolCalls, work.inputTokens, work.outputTokens, work.latencyMs], [3, 3, 122, 42, 10]);

// Events seen so far while the run waits: the pending calls are listed with their wait states.
assert.deepEqual(
  buildWork(log.slice(0, 4)).items.map((item) => (item.kind === 'tool' ? item.status : item.kind)),
  ['model', 'WAITING_APPROVAL', 'PENDING'],
);
assert.deepEqual(buildWork([{ seq: 1, type: 'model.completed', data: 'not json' }]).items, [
  { kind: 'model', seq: 1, action: '', inputTokens: 0, outputTokens: 0, latencyMs: 0 },
]);

assert.equal(deltaText('{"text":"你好"}'), '你好');
assert.equal(deltaText('{"delta":"x"}'), 'x');
assert.equal(deltaText('oops'), '');

const merged = mergeEvents(log.slice(0, 3), [log[1], log[4], log[3], { seq: 0, type: 'stream.delta', data: '{}' }]);
assert.deepEqual(merged.map((event) => event.seq), [1, 2, 4, 5, 7]);
const same = log.slice(0, 2);
assert.equal(mergeEvents(same, [log[0]]), same);

const now = new Date(2026, 10, 22, 15, 0, 0);
assert.equal(dayGroup(new Date(2026, 10, 22, 0, 5).toISOString(), now), '今天');
assert.equal(dayGroup(new Date(2026, 10, 21, 23, 55).toISOString(), now), '昨天');
assert.equal(dayGroup(new Date(2026, 10, 17, 9, 0).toISOString(), now), '近 7 天');
assert.equal(dayGroup(new Date(2026, 9, 1).toISOString(), now), '更早');
assert.equal(dayGroup(undefined, now), '更早');

// streamEvents hands over each block as it arrives, including deltas split across chunks.
Object.defineProperty(globalThis, 'localStorage', { value: { getItem: () => null }, configurable: true });
const chunks = ['id: 3\nevent: run.resumed\ndata: {}\n\nevent: stream.delta\nda', 'ta: {"text":"he"}\n\n: ping\n\n', 'event: stream.delta\ndata: {"text":"llo"}\n\nid: 4\nevent: run.completed\ndata: {}\n\n'];
let request: { url: string; headers: Headers } | undefined;
Object.defineProperty(globalThis, 'fetch', {
  value: async (url: string, options?: RequestInit) => {
    request = { url, headers: new Headers(options?.headers) };
    const encoder = new TextEncoder();
    return new Response(new ReadableStream({
      start(controller) {
        for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
        controller.close();
      },
    }), { status: 200 });
  },
  configurable: true,
});
const seen: HarnessEvent[] = [];
await api.streamEvents('run_live', 2, (event) => seen.push(event), new AbortController().signal);
assert.equal(request?.url, '/v1/runs/run_live/events');
assert.equal(request?.headers.get('Last-Event-ID'), '2');
assert.equal(request?.headers.has('Authorization'), false);
assert.deepEqual(seen.map((event) => `${event.seq}:${event.type}`), ['3:run.resumed', '0:stream.delta', '0:stream.delta', '4:run.completed']);
assert.equal(seen.filter((event) => event.seq === 0).map((event) => deltaText(event.data)).join(''), 'hello');

console.log('work timeline, event merge, session groups, and live event stream: ok');
