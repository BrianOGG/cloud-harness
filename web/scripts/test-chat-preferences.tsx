import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, StrictMode } from 'react';
import type { Root } from 'react-dom/client';
import type { HarnessEvent, RunView, SessionPreferences } from '../src/api';

const dom = new JSDOM('<!doctype html><div id="root"></div>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event', 'MouseEvent']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true, CSS: { escape: (s: string) => s.replace(/[^\w-]/g, '\\$&') } });
Object.assign(window, { matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }) });
let scrolledTo: Element | null = null;
dom.window.HTMLElement.prototype.scrollIntoView = function () { scrolledTo = this; };
const { createRoot } = await import('react-dom/client');
const { default: App } = await import('../src/App');
const { Turn } = await import('../src/Chat');
const { SessionHistory } = await import('../src/SessionHistory');
const { api } = await import('../src/api');
api.streamEvents = async (_id, _after, _cb, signal) => { if (!signal.aborted) await new Promise<void>((resolve) => signal.addEventListener('abort', () => resolve(), { once: true })); };

const now = '2026-11-26T16:00:00Z';
const defaultPrefs: SessionPreferences = { model_id: 'default', approval_mode: 'ask' };
const initialModels = [{ id: 'default', name: 'Default', model: 'base', provider: 'fixture' }, { id: 'profile:fast', name: 'Fast', model: 'fast', provider: 'fixture' }];
const waiting: RunView = { id: 'run_wait', session_id: 'ses_a', turn_id: 'turn_a', status: 'WAITING', wait_reason: 'APPROVAL', final_answer: '', budget: { used_model_calls: 1, max_model_calls: 20, used_tool_calls: 0, max_tool_calls: 20, used_child_runs: 0, max_child_runs: 10, used_script_calls: 0, max_script_calls: 10 }, version: 1, created_at: now, updated_at: now, pending_invocation: { tool: 'write_file', arguments: { path: 'answer.txt' } } };
const container = document.getElementById('root')!;
let root: Root | undefined;
let preferences: Record<string, SessionPreferences>;
let models = [...initialModels];
let providers: { id: string; name: string; base_url: string; model: string; has_api_key: boolean }[] = [];
let seen: { path: string; method: string; body?: Record<string, string> }[] = [];
let linked = false;

globalThis.fetch = async (input, init) => {
  const path = String(input), method = init?.method || 'GET';
  const body = init?.body ? JSON.parse(String(init.body)) as Record<string, string> : undefined;
  seen.push({ path, method, body });
  let result: unknown;
  if (path === '/healthz') result = { status: 'ok', provider: 'fixture' };
  else if (path === '/v1/models') result = models;
  else if (path === '/v1/providers') {
    if (method === 'PUT') {
      const p = { id: body!.id, name: body!.name, base_url: body!.base_url, model: body!.model, has_api_key: true };
      providers = [p]; models = [...initialModels, { id: 'custom:' + p.id, name: p.name, model: p.model, provider: 'openai' }];
      result = p;
    } else result = providers;
  } else if (path === '/v1/sessions') result = method === 'POST' ? { id: 'ses_created' } : ['ses_a', 'ses_b'].map((id) => ({ id, created_at: now, updated_at: now, run_count: linked && id === 'ses_a' ? 1 : 0 }));
  else if (path.endsWith('/preferences')) {
    const id = path.split('/')[3];
    if (method === 'PUT') preferences[id] = body as unknown as SessionPreferences;
    result = preferences[id] || defaultPrefs;
  } else if (path.includes('/messages')) result = { messages: [{ id: 'msg_a', session_id: path.split('/')[3], run_id: null, turn_id: null, role: 'assistant', kind: 'response', status: 'recorded', content: '最终回答始终可见', created_at: now }], next_cursor: '' };
  else if (path.endsWith('/runs?wait=0')) result = { ...waiting, id: 'run_created', session_id: path.split('/')[3], status: 'COMPLETED', wait_reason: null };
  else if (path.endsWith('/runs')) result = linked && path.includes('ses_a') ? [waiting] : [];
  else if (path === '/v1/runs/run_wait') result = waiting;
  else throw new Error('Unexpected request: ' + path);
  return new Response(JSON.stringify(result), { headers: { 'Content-Type': 'application/json' } });
};
async function mount(element = <App />) { root = createRoot(container); await act(async () => { root!.render(<StrictMode>{element}</StrictMode>); }); }
async function reset(query = '') {
  if (root) await act(async () => { root!.unmount(); root = undefined; });
  localStorage.clear(); window.history.replaceState(null, '', '/ui/' + query);
  preferences = { ses_a: { model_id: 'profile:fast', approval_mode: 'safe' }, ses_b: { ...defaultPrefs } };
  models = [...initialModels]; providers = []; seen = []; linked = false; scrolledTo = null;
}
async function choose(label: string, value: string) {
  const select = container.querySelector<HTMLSelectElement>('select[aria-label="' + label + '"]')!;
  assert.ok(select && !select.disabled);
  await act(async () => { select.value = value; select.dispatchEvent(new Event('change', { bubbles: true })); });
}
async function type(selector: string, value: string) {
  const input = container.querySelector<HTMLInputElement | HTMLTextAreaElement>(selector)!;
  await act(async () => {
    const proto = input.tagName === 'TEXTAREA' ? window.HTMLTextAreaElement.prototype : window.HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value')!.set!.call(input, value);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
}
try {
  await test('session preferences load, save, and stay separate across conversations', async () => {
    await reset(); localStorage.setItem('cloud-harness.web-session', 'ses_a'); await mount();
    assert.equal(container.querySelector<HTMLSelectElement>('select[aria-label="审批模式"]')!.value, 'safe');
    assert.equal(container.querySelector<HTMLSelectElement>('select[aria-label="模型"]')!.value, 'profile:fast');
    assert.equal([...container.querySelectorAll('.sidebar-nav button')].some((b) => b.textContent === '审批'), false);
    assert.ok(container.textContent?.includes('观测'));
    await choose('审批模式', 'workspace-write');
    assert.deepEqual(preferences.ses_a, { model_id: 'profile:fast', approval_mode: 'workspace-write' });
    await act(async () => { container.querySelector<HTMLButtonElement>('[data-session-id="ses_b"]')!.click(); });
    assert.equal(container.querySelector<HTMLSelectElement>('select[aria-label="模型"]')!.value, 'default');
    await choose('模型', 'profile:fast');
    assert.deepEqual(preferences.ses_b, { model_id: 'profile:fast', approval_mode: 'ask' });
  });
  await test('new conversation keeps local choices, then persists them and passes createRun options', async () => {
    await reset(); await mount(); await choose('审批模式', 'safe'); await choose('模型', 'profile:fast');
    assert.equal(seen.some((r) => r.method === 'PUT' && r.path.includes('/preferences')), false);
    assert.deepEqual(JSON.parse(localStorage.getItem('cloud-harness.web-draft-preferences')!), { approval_mode: 'safe', model_id: 'profile:fast' });
    await type('textarea', 'hello');
    await act(async () => { container.querySelector<HTMLButtonElement>('button[aria-label="发送"]')!.click(); });
    assert.deepEqual(preferences.ses_created, { approval_mode: 'safe', model_id: 'profile:fast' });
    assert.deepEqual(seen.find((r) => r.path.endsWith('/runs?wait=0'))?.body, { message: 'hello', approval_mode: 'safe', model_id: 'profile:fast' });
  });
  await test('notification run deep link selects its conversation and visible WaitCard', async () => {
    await reset('?run=run_wait'); linked = true; await mount();
    assert.equal(localStorage.getItem('cloud-harness.web-session'), 'ses_a');
    assert.ok(container.querySelector('.wait-card'));
    assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'false');
    assert.equal(scrolledTo, container.querySelector('.wait-card'));
    assert.ok(container.textContent?.includes('最终回答始终可见'));
  });
  await test('running work and live draft default closed, expand independently of transcript and WaitCard', async () => {
    await reset();
    let loaded = 0;
    const events: HarnessEvent[] = [{ seq: 1, type: 'model.completed', data: '{"action":"EXECUTE","input_tokens":1}' }];
    const actions = { respond() {}, addBudget() {}, cancel() {}, inspect() {}, focusAnswer() {}, loadEvents() { loaded++; } };
    await mount(<><SessionHistory messages={[{ id: 'answer', session_id: 'ses_a', run_id: null, turn_id: null, role: 'assistant', kind: 'response', content: '最终回答正文', status: 'recorded', created_at: now }]} loading={false} error="" onRefresh={() => {}} onInspect={() => {}} /><Turn run={{ ...waiting, status: 'RUNNING' }} events={events} live="未提交实时草稿" acting={false} actions={actions} /><Turn run={waiting} events={events} live="" acting={false} actions={actions} /></>);
    assert.equal(container.querySelectorAll('.work-list').length, 0);
    assert.equal(container.querySelector<HTMLDetailsElement>('.live-draft')!.open, false);
    assert.ok(container.querySelector('.wait-card'));
    assert.ok(container.querySelector('.session-history')?.textContent?.includes('最终回答正文'));
    await act(async () => { container.querySelector<HTMLButtonElement>('.work-toggle')!.click(); });
    assert.equal(container.querySelectorAll('.work-list').length, 1); assert.equal(loaded, 1);
    await act(async () => { container.querySelector<HTMLButtonElement>('.work-toggle')!.click(); });
    assert.equal(container.querySelectorAll('.work-list').length, 0);
  });
  await test('provider form hides, clears and preserves saved credentials on blank edits', async () => {
    await reset(); await mount();
    const fields = container.querySelectorAll<HTMLInputElement>('.provider-fields input');
    // Label position provides a stable target without depending on display text.
    const values = ['fixture', 'Fixture', 'http://127.0.0.1:1234', 'custom', 'test-only-form-key'];
    for (let i = 0; i < fields.length; i++) {
      await act(async () => { Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!.call(fields[i], values[i]); fields[i].dispatchEvent(new Event('input', { bubbles: true })); });
    }
    assert.equal(fields[4].type, 'password');
    await act(async () => { container.querySelector<HTMLButtonElement>('.provider-fields button')!.click(); });
    assert.equal(container.querySelector<HTMLSelectElement>('select[aria-label="模型"]')!.value, 'custom:fixture');
    const keyInput = container.querySelector<HTMLInputElement>('input[type="password"]')!;
    assert.equal(keyInput.value, ''); assert.equal(keyInput.placeholder, '已保存；留空保留原值');
    await act(async () => { container.querySelector<HTMLButtonElement>('.provider-fields button')!.click(); });
    const saves = seen.filter((r) => r.path === '/v1/providers' && r.method === 'PUT');
    assert.equal(saves.length, 2); assert.equal(saves[1].body?.api_key, undefined);
  });
} finally {
  if (root) await act(async () => { root!.unmount(); });
  dom.window.close();
}
