import assert from 'node:assert/strict';
import { test } from 'node:test';
import React, { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import type { SessionMessage } from '../src/api';
import { loadSessionMessages, mergeSessionMessages, syncSessionMessages } from '../src/history';
Object.assign(globalThis, { React });
const { MessageList } = await import('../src/MessageList');

// Oracle: docs/api.md messages contract and the CP-1 public client contract.
// Synthetic durable facts, not model output or browser acceptance evidence.
// Expected order is explicit; no production comparator/encoder is reused.
const session = 'ses_fixture';
const stamp = '2026-11-25T09:00:01.123456789Z';
function fact(id: string, fields: Partial<SessionMessage> = {}): SessionMessage {
  return { id, session_id: session, run_id: null, turn_id: null, role: 'assistant',
    kind: 'response', status: 'recorded', content: '相同正文 🌨️\r\n**原文** <b>保留</b>',
    event: '', created_at: stamp, ...fields };
}

await test('merge: nanoseconds precede ASCII ID ties across interleaved runs', () => {
  const input = [
    fact('msg_response_a', { run_id: 'run_first' }),
    fact('msg_response_Z', { run_id: 'run_second' }),
    fact('msg_response_A_later', { created_at: '2026-11-25T09:00:01.123456790Z' }),
    fact('msg_response_z_earlier', { created_at: '2026-11-25T09:00:01.123456788Z' }),
    fact('msg_response__'), fact('msg_response_A', { run_id: 'run_first' }),
    fact('msg_response_0', { run_id: 'run_second' }),
  ];
  const existing = Object.freeze(input.slice(0, 3).map((row) => Object.freeze(row)));
  const incoming = Object.freeze(input.slice(3).map((row) => Object.freeze(row)));
  const snapshot = JSON.stringify([existing, incoming]);
  assert.deepEqual(mergeSessionMessages(existing, incoming).map((row) => row.id), [
    'msg_response_z_earlier', 'msg_response_0', 'msg_response_A', 'msg_response_Z',
    'msg_response__', 'msg_response_a', 'msg_response_A_later',
  ]);
  assert.equal(JSON.stringify([existing, incoming]), snapshot, 'merge must not mutate inputs');
});

await test('merge: stable IDs deduplicate replay; different IDs with identical text survive', () => {
  const a = fact('msg_response_A');
  const b = fact('msg_response_B');
  const c = fact('msg_answer_run_retry', { run_id: 'run_retry' });
  const rows = mergeSessionMessages([a, b, a], [b, c, a, c]);
  assert.deepEqual(rows.map((row) => row.id), ['msg_answer_run_retry', 'msg_response_A', 'msg_response_B']);
  assert.equal(rows.length, 3);
  assert.ok(rows.every((row) => row.content === a.content));
  assert.deepEqual(mergeSessionMessages(rows, [c, a, b]), rows, 'refresh/replay is idempotent');
  assert.deepEqual(mergeSessionMessages([], []), []);
});

await test('sync resumes a thousand-message transcript and drops stale rows on a mid-page reset', async () => {
  const initial = Array.from({ length: 1000 }, (_, i) => fact('msg_' + i));
  const late = fact('msg_backdated', { created_at: '2025-01-01T00:00:00.000000000Z' });
  const signal = new AbortController().signal;
  await withPages([
    { messages: [late], sync: '0.1001', more: false, reset: false },
    { messages: [fact('deleted')], sync: '0.1002', more: true, reset: false },
    { messages: [late], sync: '1005.1001', more: false, reset: true },
  ], async (seen) => {
    const updated = await syncSessionMessages(session, { cursor: '0.1000', messages: initial }, signal);
    assert.equal(seen.length, 1);
    assert.equal(seen[0].url.searchParams.get('sync'), '0.1000');
    assert.equal(updated.messages.length, 1001);
    assert.equal(updated.messages[0].id, late.id);
    const retained = await syncSessionMessages(session, updated, signal);
    assert.deepEqual(retained.messages, [late]);
    assert.deepEqual(seen.slice(1).map(r => r.url.searchParams.get('sync')), ['0.1001', '0.1002']);
  });
});

const transcript: SessionMessage[] = [
  fact('msg_user_turn', { role: 'user', kind: 'input', turn_id: 'turn_only', content: '原始问题\n第二行' }),
  fact('msg_response_visible', { run_id: 'run_one', turn_id: 'turn_one', content: '完整正文 🌨️\r\n第二行' }),
  fact('msg_answer_run_legacy', { run_id: 'run_legacy', content: '保留的 final answer fallback' }),
  fact('msg_question_inv', { kind: 'question', run_id: 'run_one', content: '选哪个区域？' }),
  fact('msg_answer_inv', { role: 'user', kind: 'answer', run_id: 'run_one', content: '上海' }),
  fact('msg_approval_allow_requested', { role: 'system', kind: 'approval_requested', run_id: 'run_one', content: '写入报告' }),
  fact('msg_approval_allow_decision', { role: 'system', kind: 'approval_decision', run_id: 'run_one', content: 'approve' }),
  fact('msg_approval_deny_requested', { role: 'system', kind: 'approval_requested', run_id: 'run_two', content: '执行命令' }),
  fact('msg_approval_deny_decision', { role: 'system', kind: 'approval_decision', run_id: 'run_two', content: 'deny' }),
  fact('msg_status_run_one', { role: 'system', kind: 'run_status', run_id: 'run_one', content: 'COMPLETED', event: 'run.CoMpLeTeD' }),
  fact('msg_status_run_two', { role: 'system', kind: 'run_status', run_id: 'run_two', status: 'failed', content: 'provider unavailable', event: 'run.failed' }),
  fact('msg_status_run_cancel', { role: 'system', kind: 'run_status', run_id: 'run_cancel', status: 'cancelled', content: 'CANCELLED', event: 'run.CANCELLED' }),
].map((row, index) => ({ ...row, created_at: '2026-11-25T09:00:02.' + String(index).padStart(9, '0') + 'Z' }));

type Captured = { url: URL; signal: AbortSignal | null | undefined; headers: Headers };
await test('approval history: mapped labels replace raw decisions and unknown values remain visible', () => {
  function renderDecision(content: string): string {
    const message = fact('msg_decision', { role: 'system', kind: 'approval_decision', content });
    return renderToStaticMarkup(createElement(MessageList, {
      messages: [message],
    }));
  }

  const approved = renderDecision('approve');
  assert.match(approved, /已批准/);
  assert.doesNotMatch(approved, />approve</);

  const denied = renderDecision('deny');
  assert.match(denied, /已拒绝/);
  assert.doesNotMatch(denied, />deny</);

  const unknown = renderDecision('manual-review');
  assert.match(unknown, /manual-review/);
});
async function withPages(pages: unknown[], exercise: (seen: Captured[]) => Promise<void>): Promise<void> {
  const originalFetch = globalThis.fetch;
  const storage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  const seen: Captured[] = [];
  // Cleared browser store: never seed transcript text or a run snapshot.
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: {
    getItem: () => null,
    setItem: () => { throw new Error('history must not write localStorage'); },
  } });
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    assert.equal(typeof input, 'string', 'public client must use same-origin paths');
    assert.ok(String(input).startsWith('/v1/sessions/'));
    const index = seen.length;
    seen.push({ url: new URL(String(input), 'http://fixture.invalid'), signal: init?.signal, headers: new Headers(init?.headers) });
    assert.ok(index < pages.length, 'fixture has no extra reply');
    assert.equal(init?.method ?? 'GET', 'GET');
    return new Response(JSON.stringify(pages[index]), { headers: { 'Content-Type': 'application/json' } });
  }) as typeof fetch;
  try { await exercise(seen); } finally {
    globalThis.fetch = originalFetch;
    if (storage) Object.defineProperty(globalThis, 'localStorage', storage);
    else Reflect.deleteProperty(globalThis, 'localStorage');
  }
}

await test('load: cleared storage restores every durable kind and nullable association', async () => {
  await withPages([
    { messages: transcript.slice(0, 6), next_cursor: 'opaque_page_two' },
    { messages: transcript.slice(6), next_cursor: '' },
    { messages: transcript.slice(0, 6), next_cursor: 'opaque_page_two' },
    { messages: transcript.slice(6), next_cursor: '' },
  ], async (seen) => {
    const controller = new AbortController();
    const result = await loadSessionMessages(session, { signal: controller.signal });
    assert.deepEqual(result, transcript, 'preserve complete records, original text and event case');
    assert.deepEqual(seen.map(({ url }) => url.searchParams.get('cursor') ?? ''), ['', 'opaque_page_two']);
    assert.ok(seen.every(({ url }) => url.pathname === '/v1/sessions/ses_fixture/messages'));
    assert.ok(seen.every(({ signal }) => signal === controller.signal), 'forward cancellation on every page');
    assert.ok(seen.every(({ headers }) => !headers.has('Authorization')));
    assert.equal(result[0].run_id, null);
    assert.equal(result[0].turn_id, 'turn_only');
    assert.equal(result[2].turn_id, null);
    const refreshed = await loadSessionMessages(session, { signal: controller.signal });
    assert.deepEqual(refreshed, transcript);
    assert.deepEqual(seen.map(({ url }) => url.searchParams.get('cursor') ?? ''),
      ['', 'opaque_page_two', '', 'opaque_page_two'], 'each refresh starts at the first page');
    assert.deepEqual(mergeSessionMessages(result, refreshed), result, 'refresh must not duplicate historical approvals');
  });
});

await test('load: empty session completes with one request', async () => {
  await withPages([{ messages: [], next_cursor: '' }], async (seen) => {
    assert.deepEqual(await loadSessionMessages(session), []);
    assert.equal(seen.length, 1);
  });
});

await test('load: opaque continuation passes through without interpreting its contents', async () => {
  // Transport sentinel, deliberately not a v1 cursor. Serve does not accept
  // these characters; this tests escaping without claiming backend validity.
  const cursor = 'opaque+/%=&?# 雪';
  await withPages([
    { messages: [transcript[0]], next_cursor: cursor },
    { messages: [transcript[1]], next_cursor: '' },
  ], async (seen) => {
    assert.deepEqual(await loadSessionMessages(session), transcript.slice(0, 2));
    assert.deepEqual(seen[1].url.searchParams.getAll('cursor'), [cursor]);
    assert.equal(seen[1].url.hash, '');
  });
});

for (const [label, cursors] of [
  ['immediate repeat', ['again', 'again']],
  ['nonadjacent cycle', ['again', 'other', 'again']],
] as const) {
  await test('load: ' + label + ' fails without partial history or an infinite loop', async () => {
    await withPages(cursors.map((cursor, index) => ({ messages: [transcript[index]], next_cursor: cursor })), async (seen) => {
      await assert.rejects(loadSessionMessages(session));
      assert.equal(seen.length, cursors.length, 'stop before requesting a consumed continuation again');
    });
  });
}

for (const [label, page] of [
  ['null page', null],
  ['bare array', []],
  ['null messages', { messages: null, next_cursor: '' }],
  ['missing continuation', { messages: [] }],
  ['null continuation', { messages: [], next_cursor: null }],
  ['foreign session', { messages: [fact('msg_foreign', { session_id: 'ses_other' })], next_cursor: '' }],
  ['invalid content', { messages: [{ ...transcript[1], content: null }], next_cursor: '' }],
] as const) {
  await test('load: ' + label + ' after a good page rejects the entire traversal', async () => {
    await withPages([{ messages: [transcript[0]], next_cursor: 'next' }, page], async (seen) => {
      await assert.rejects(loadSessionMessages(session));
      assert.equal(seen.length, 2);
    });
  });
}
