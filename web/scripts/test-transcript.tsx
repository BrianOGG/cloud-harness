import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, StrictMode } from 'react';
import type { Root } from 'react-dom/client';
import type { HarnessEvent, RunView, SessionMessage } from '../src/api';
import { projectTranscript, turnSummary } from '../src/transcript';

const dom = new JSDOM('<!doctype html><div id="root"></div>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event', 'MouseEvent']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true, CSS: { escape: (value: string) => value } });
Object.assign(window, { matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }) });
dom.window.HTMLElement.prototype.scrollIntoView = () => {};
const { createRoot } = await import('react-dom/client');
const { SessionHistory } = await import('../src/SessionHistory');
const { default: App } = await import('../src/App');
const { api } = await import('../src/api');
const container = document.getElementById('root')!;
let root: Root | undefined;
const now = '2026-11-26T20:00:00.000000000Z';
const base: RunView = { id: 'run_root', session_id: 'ses_fixture', turn_id: 'turn_root', status: 'COMPLETED', final_answer: '权威根运行最终答复', approval_mode: 'workspace-write', workspace: '/fixture', budget: { max_model_calls: 20, used_model_calls: 3, max_tool_calls: 20, used_tool_calls: 2, max_script_calls: 10, used_script_calls: 0, max_child_runs: 4, used_child_runs: 0 }, version: 1 };
function fact(id: string, kind: SessionMessage['kind'], content: string, fields: Partial<SessionMessage> = {}): SessionMessage {
  return { id, session_id: base.session_id, run_id: base.id, turn_id: base.turn_id!, kind, role: kind === 'input' || kind === 'answer' ? 'user' : kind === 'question' || kind === 'response' ? 'assistant' : 'system', status: 'recorded', event: '', content, created_at: now.slice(0, 20) + String(sequence++).padStart(9, '0') + 'Z', ...fields };
}
let sequence = 0;
const input = fact('input', 'input', '请诊断系统');
const intermediate = fact('intermediate', 'response', '还在检查，请稍候');
const request = fact('msg_approval_apr_one_requested', 'approval_requested', 'uname -a');
const decision = fact('msg_approval_apr_one_decision', 'approval_decision', 'approve');
const finalRecord = fact('stored_final', 'response', base.final_answer!);
const audit = [input, intermediate, request, decision, finalRecord];
const child: RunView = { ...base, id: 'run_child', parent_id: base.id, turn_id: 'turn_child', final_answer: '子运行内部答复' };
const childRecord = fact('child_response', 'response', child.final_answer!, { run_id: child.id, turn_id: child.turn_id! });
const pending: RunView = { ...base, status: 'WAITING', wait_reason: 'APPROVAL', final_answer: '', pending_invocation: { invocation_id: 'inv_current', tool: 'run_command', arguments: { command: 'cat /proc/meminfo', cwd: 'system' }, resources: ['/fixture/system', 'cat /proc/meminfo'] } };
const decisions: string[] = [];
const opened: string[] = [];
const actions = { respond(id: string, body: { decision?: string }) { decisions.push(id + ':' + body.decision); }, addBudget() {}, cancel() {}, inspect() {}, focusAnswer() {}, loadEvents(run: RunView) { opened.push(run.id); } };
const log: HarnessEvent[] = [
  { seq: 1, type: 'policy.decided', data: '{"tool":"run_command","effect":"ask","source":"default","rule":"default"}' },
  { seq: 2, type: 'invocation.SUCCEEDED', data: '{"status":"SUCCEEDED","tool":"run_command","provider_call_id":"diagnostic"}' },
];
// Same fields as the read-only trace contract; persistent invocation events
// deliberately carry only status metadata, not command arguments or results.
api.getTrace = async (id) => ({ session_id: base.session_id, run: { id, status: 'COMPLETED', steps: [{ id: 'step', action: 'EXECUTE', batches: [{ id: 'batch', status: 'COMPLETED', action: 'EXECUTE', calls: [{ id: 'inv_current', name: 'run_command', status: 'SUCCEEDED', ordinal: 0, arguments: { command: 'uname -a' }, result: { ok: true, data: { stdout: 'Linux fixture' } } }] }] }] } });
function history(runs: RunView[], messages = audit, extra: Record<string, unknown> = {}) {
  return <SessionHistory messages={messages} runs={runs} events={{ [base.id]: log }} live={{}} acting={{}} actions={actions} loading={false} error="" onRefresh={() => {}} onInspect={() => {}} {...extra} />;
}
async function render(element: React.ReactNode) {
  if (!root) root = createRoot(container);
  await act(async () => { root!.render(<StrictMode>{element}</StrictMode>); });
}
async function reset() {
  if (root) await act(async () => { root!.unmount(); root = undefined; });
  decisions.length = opened.length = 0;
  localStorage.clear();
}
async function click(selector: string) {
  const button = container.querySelector<HTMLButtonElement>(selector)!;
  assert.ok(button, 'missing ' + selector);
  await act(async () => { button.click(); });
}

try {
  await test('projection joins distinct child turn IDs, preserves audit order and uses only completed root final_answer', () => {
    const snapshot = JSON.stringify([audit, childRecord, base, child]);
    const turns = projectTranscript([...audit, childRecord], [child, base]);
    assert.equal(turns.length, 1);
    assert.equal(turns[0].root?.id, base.id);
    assert.equal(turns[0].finalAnswer, base.final_answer);
    assert.deepEqual(turns[0].messages.map((row) => row.id), [...audit, childRecord].map((row) => row.id));
    assert.equal(turns[0].approvals, 1, 'decision does not count as another approval');
    assert.equal(JSON.stringify([audit, childRecord, base, child]), snapshot);
    for (const status of ['FAILED', 'CANCELLED', 'RUNNING', 'WAITING']) assert.equal(projectTranscript(audit, [{ ...base, status }])[0].finalAnswer, undefined);
    assert.equal(projectTranscript([childRecord], [child])[0].finalAnswer, undefined, 'missing parent never promotes child');
    assert.equal(projectTranscript(audit, [{ ...base, final_answer: '' }])[0].finalAnswer, undefined);
    const noFinal = projectTranscript(audit, [{ ...base, final_answer: '' }])[0];
    assert.match(turnSummary(noFinal), /无最终答复/);
  });

  await test('completed default body has input, authoritative final and one expandable process, no approval wall or child final', async () => {
    await reset(); await render(history([base, child], [...audit, childRecord]));
    assert.equal(container.querySelectorAll('.turn').length, 1);
    assert.equal(container.querySelectorAll('.work-toggle').length, 1);
    assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'false');
    assert.equal(container.querySelector('.turn-final')?.textContent?.includes(base.final_answer!), true);
    assert.equal(container.querySelectorAll('.history-fact, .turn-process, .wait-card, .past-runs').length, 0);
    assert.equal(container.textContent?.includes(intermediate.content), false);
    assert.equal(container.textContent?.includes(child.final_answer!), false);
    assert.match(container.textContent!, /4 次工具调用 · 1 次审批/);
    await click('.work-toggle');
    assert.deepEqual([...container.querySelectorAll('.turn-process [data-message-id]')].map((node) => node.getAttribute('data-message-id')), [...audit.slice(1), childRecord].map((row) => row.id));
    assert.match(container.textContent!, /policy.decided/);
    assert.match(container.textContent!, /Linux fixture/);
    assert.ok(opened.includes(child.id) && opened.includes(base.id));
    await render(history([{ ...base, version: 2 }, child], [...audit, childRecord]));
    assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'true', 'refresh must retain manual expansion');
  });

  await test('only authoritative current invocation creates outer keyboard-operable wait actions; acting disables both', async () => {
    await reset(); await render(history([pending]));
    const card = container.querySelector('.wait-card')!;
    assert.ok(card && !card.closest('.work'));
    assert.ok(card.textContent?.includes('cat /proc/meminfo'));
    assert.ok(card.textContent?.includes('system') && card.textContent?.includes('/fixture'));
    assert.ok(card.textContent?.includes('命令不在模式自动执行范围'));
    assert.equal(container.querySelector('.history-fact'), null);
    const buttons = [...card.querySelectorAll<HTMLButtonElement>('button')];
    assert.equal(buttons.length, 2);
    for (const button of buttons) { assert.equal(button.type, 'button'); assert.equal(button.tabIndex, 0); button.focus(); assert.equal(document.activeElement, button); }
    await click('.wait-actions .primary'); await click('.wait-actions .button');
    assert.deepEqual(decisions, [base.id + ':approve', base.id + ':deny']);
    await render(history([pending], audit, { acting: { [base.id]: true } }));
    assert.ok([...container.querySelectorAll<HTMLButtonElement>('.wait-actions button')].every((button) => button.disabled));
    await render(history([{ ...pending, status: 'RUNNING', wait_reason: '' }]));
    assert.equal(container.querySelector('.wait-card'), null, 'stale requests are audit only, not pending');
  });

  await test('approval resolution returns to same collapsed turn; new refresh/remount uses identical projection', async () => {
    await reset(); await render(history([pending], audit.slice(0, 3)));
    const turn = container.querySelector('.turn');
    await render(history([base]));
    assert.equal(container.querySelector('.turn'), turn, 'same invocation updates its existing turn');
    assert.equal(container.querySelectorAll('.history-fact, .wait-card').length, 0);
    assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'false');
    const before = container.textContent;
    await reset(); await render(history([base], [...audit]));
    assert.equal(container.textContent, before);
    await click('.work-toggle');
    assert.match(container.textContent!, /已批准/);
    assert.equal(container.querySelectorAll('.turn-process [data-kind="approval_requested"]').length, 1);
    assert.equal(container.querySelectorAll('.turn-process [data-kind="approval_decision"]').length, 1);
  });

  await test('manual expansion survives live updates and a denial while drafts stay inside process', async () => {
    await reset();
    const running = { ...pending, status: 'RUNNING', wait_reason: '' };
    await render(history([running], audit.slice(0, 3), { live: { [base.id]: '尚未提交的流式草稿' } }));
    assert.equal(container.textContent?.includes('尚未提交的流式草稿'), false);
    await click('.work-toggle');
    assert.ok(container.querySelector('.turn-process .live-draft')?.textContent?.includes('尚未提交的流式草稿'));
    await render(history([{ ...running, version: 2 }], audit.slice(0, 3), { live: { [base.id]: '更新中的草稿' } }));
    assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'true');
    const denied = { ...decision, content: 'deny' };
    await render(history([base], [input, request, denied, finalRecord]));
    assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'true');
    assert.equal(container.querySelector('.wait-card'), null);
    assert.equal(container.querySelectorAll('[data-kind="approval_decision"]').length, 1);
    assert.ok(container.querySelector('.turn-process')?.textContent?.includes('已拒绝'));
    await click('.work-toggle');
    assert.equal(container.querySelector('.history-fact'), null);
  });

  await test('human questions/answers, child pending, budget pause and reconciliation remain outside process', async () => {
    await reset();
    const question = fact('question', 'question', '选哪个区域？', { run_id: child.id, turn_id: child.turn_id! });
    const answer = fact('answer', 'answer', '上海', { run_id: child.id, turn_id: child.turn_id! });
    const childPending = { ...pending, id: child.id, parent_id: base.id, turn_id: child.turn_id };
    await render(history([{ ...base, status: 'WAITING', wait_reason: 'CHILD' }, childPending], [...audit, question, answer]));
    assert.equal(container.querySelectorAll('.turn').length, 1);
    assert.ok(container.querySelector('[data-kind="question"]') && container.querySelector('[data-kind="answer"]'));
    assert.equal(container.querySelector('.wait-card')?.closest('.work'), null);
    await render(history([{ ...pending, wait_reason: 'BUDGET' }]));
    assert.ok(container.querySelector('[aria-label="等待补充预算"]'));
    await render(history([{ ...pending, wait_reason: 'RECONCILIATION' }]));
    assert.ok(container.querySelector('[aria-label="等待确认工具结果"]'));
    await render(history([{ ...pending, wait_reason: 'USER', question: '当前新问题' }], [input]));
    assert.ok(container.querySelector('.current-question')?.textContent?.includes('当前新问题'));
    assert.equal(container.querySelector('.current-question')?.closest('.work'), null);
  });

  await test('no answer fallback from intermediate text, orphan response or child; failures stay honest', async () => {
    await reset();
    await render(history([{ ...base, status: 'FAILED', error: { message: 'fixture provider unavailable' } }], audit));
    assert.equal(container.querySelector('.turn-final'), null);
    assert.match(container.querySelector('[role="alert"]')!.textContent!, /fixture provider unavailable/);
    await render(history([{ ...base, status: 'CANCELLED' }], audit));
    assert.equal(container.querySelector('.turn-final'), null);
    assert.match(container.textContent!, /已取消/);
    await render(history([], [fact('orphan', 'response', '这不是可确认的最终答复', { run_id: null, turn_id: null })]));
    assert.equal(container.querySelector('.turn-final'), null);
    assert.equal(container.textContent?.includes('这不是可确认的最终答复'), false);
    await click('.work-toggle');
    assert.ok(container.textContent?.includes('这不是可确认的最终答复'));
  });

  await test('App failed approval preserves pending, prevents duplicate submit, separates next-turn preference and survives restore', async () => {
    await reset();
    let current = { ...pending };
    let rows = audit.slice(0, 3);
    let prefs = { approval_mode: 'safe', model_id: 'default' };
    let submits = 0;
    let release: (() => void) | undefined;
    let held: Promise<void> | undefined;
    let fail = true;
    const originalFetch = globalThis.fetch;
    const originalStream = api.streamEvents;
    api.streamEvents = async (_id, _after, _cb, signal) => { if (!signal.aborted) await new Promise<void>((resolve) => signal.addEventListener('abort', () => resolve(), { once: true })); };
    globalThis.fetch = async (input, init) => {
      const path = String(input);
      let body: unknown;
      let status = 200;
      if (path === '/healthz') body = { provider: 'fixture' };
      else if (path === '/v1/models') body = [{ id: 'default', name: 'Default', model: 'fixture', provider: 'fixture' }];
      else if (path === '/v1/sessions') body = [{ id: base.session_id, created_at: now, updated_at: now, run_count: 1 }];
      else if (path.endsWith('/preferences')) {
        if (init?.method === 'PUT') prefs = JSON.parse(String(init.body));
        body = prefs;
      } else if (path.endsWith('/messages')) body = { messages: rows, next_cursor: '' };
      else if (path.endsWith('/runs')) body = [current];
      else if (path === '/v1/runs/' + base.id) body = current;
      else if (path.endsWith('/responses?wait=0')) {
        submits++;
        await held;
        if (fail) { status = 503; body = { error: { message: 'fixture submit failed' } }; }
        else { current = { ...base, version: current.version + 1 }; rows = audit; body = current; }
      } else throw new Error('Unexpected fixture route ' + path);
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
    };
    try {
      localStorage.setItem('cloud-harness.web-session', base.session_id);
      await render(<App />);
      assert.match(container.querySelector('.run-snapshot')!.textContent!, /工作区文件编辑自动/);
      assert.match(container.querySelector('.composer [aria-label="审批模式"]')!.textContent!, /下一轮 · 文件读取与 Git 查看自动/);
      await click('.composer [aria-label="审批模式"]');
      await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[0].click(); });
      assert.equal(submits, 0, 'preference changes must not approve current command');
      assert.match(container.querySelector('.run-snapshot')!.textContent!, /工作区文件编辑自动/);
      held = new Promise<void>((resolve) => { release = resolve; });
      await act(async () => { const button = container.querySelector<HTMLButtonElement>('.wait-actions .primary')!; button.click(); button.click(); });
      assert.equal(submits, 1);
      assert.ok([...container.querySelectorAll<HTMLButtonElement>('.wait-actions button')].every((button) => button.disabled));
      await act(async () => { release!(); });
      assert.match(container.querySelector('.flash[role="alert"]')!.textContent!, /fixture submit failed/);
      assert.ok(container.querySelector('.wait-card'));
      assert.equal(container.querySelector('.turn-final'), null);
      fail = false; held = undefined;
      await click('.wait-actions .primary');
      assert.equal(submits, 2);
      assert.equal(container.querySelectorAll('.wait-card, .history-fact, .past-runs').length, 0);
      assert.equal(container.querySelectorAll('.turn-final').length, 1);
      await click('[aria-label="刷新会话历史"]');
      assert.equal(container.querySelectorAll('.turn-final').length, 1);
      await click('.work-toggle');
      await click('[aria-label="刷新会话历史"]');
      assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'true');
      await act(async () => { root!.unmount(); root = undefined; });
      await render(<App />);
      assert.equal(container.querySelectorAll('.turn-final').length, 1);
      assert.equal(container.querySelectorAll('.history-fact, .wait-card').length, 0);
      assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'false');
    } finally {
      release?.();
      await reset();
      globalThis.fetch = originalFetch;
      api.streamEvents = originalStream;
    }
  });
} finally { await reset(); dom.window.close(); }
