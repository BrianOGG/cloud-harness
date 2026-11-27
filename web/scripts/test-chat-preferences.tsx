import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, StrictMode } from 'react';
import type { Root } from 'react-dom/client';
import type { RunView, SessionPreferences } from '../src/api';

const dom = new JSDOM('<!doctype html><div id="root"></div>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event', 'MouseEvent']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true, CSS: { escape: (s: string) => s.replace(/[^\w-]/g, '\\$&') } });
Object.assign(window, { matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }) });
dom.window.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
dom.window.HTMLDialogElement.prototype.close = function () { this.open = false; };
let scrolledTo: Element | null = null;
dom.window.HTMLElement.prototype.scrollIntoView = function () { scrolledTo = this; };
const { createRoot } = await import('react-dom/client');
const { default: App } = await import('../src/App');
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
let connectionFailure = false;
let saveFailure = false;

globalThis.fetch = async (input, init) => {
  const path = String(input), method = init?.method || 'GET';
  const body = init?.body ? JSON.parse(String(init.body)) as Record<string, string> : undefined;
  seen.push({ path, method, body });
  let result: unknown;
  if (path === '/healthz') result = { status: 'ok', provider: 'fixture' };
  else if (path === '/v1/models') result = models;
  else if (path === '/v1/providers') {
    if (method === 'PUT') {
      if (saveFailure) return new Response(JSON.stringify({ error: '保存失败，请重试' }), { status: 500 });
      const p = { id: body!.id, name: body!.name, base_url: body!.base_url, model: body!.model, has_api_key: true };
      providers = [...providers.filter((item) => item.id !== p.id), p]; models = [...initialModels, ...providers.map((item) => ({ id: 'custom:' + item.id, name: item.name, model: item.model, provider: 'openai' }))];
      result = p;
    } else result = providers;
  } else if (path.startsWith('/v1/providers/')) {
    if (method === 'DELETE') {
      providers = providers.filter((p) => p.id !== path.split('/')[3]);
      models = models.filter((m) => m.id !== 'custom:' + path.split('/')[3]);
      return new Response(null, { status: 204 });
    }
    if (connectionFailure) return new Response(JSON.stringify({ error: '连接失败' }), { status: 502 });
    result = { ...providers.find((p) => p.id === path.split('/')[3]), catalog: [{ id: 'custom', thinking: 'unknown' }], catalog_fetched_at: now };
  } else if (path === '/v1/sessions') result = method === 'POST' ? { id: 'ses_created' } : ['ses_a', 'ses_b'].map((id) => ({ id, created_at: now, updated_at: now, run_count: linked && id === 'ses_a' ? 1 : 0 }));
  else if (path.endsWith('/preferences')) {
    const id = path.split('/')[3];
    if (method === 'PUT') preferences[id] = body as unknown as SessionPreferences;
    result = preferences[id] || defaultPrefs;
  } else if (path.includes('/messages')) result = { messages: [{ id: 'msg_a', session_id: path.split('/')[3], run_id: 'run_done', turn_id: 'turn_done', role: 'assistant', kind: 'response', status: 'recorded', content: '最终回答始终可见', created_at: now }], next_cursor: '' };
  else if (path.endsWith('/runs?wait=0')) result = { ...waiting, id: 'run_created', session_id: path.split('/')[3], status: 'COMPLETED', wait_reason: null };
  else if (path.endsWith('/runs')) result = linked && path.includes('ses_a') ? [{ ...waiting, id: 'run_done', turn_id: 'turn_done', status: 'COMPLETED', final_answer: '最终回答始终可见' }, waiting] : [];
  else if (path === '/v1/runs/run_wait') result = waiting;
  else throw new Error('Unexpected request: ' + path);
  return new Response(JSON.stringify(result), { headers: { 'Content-Type': 'application/json' } });
};
async function mount(element = <App />) { root = createRoot(container); await act(async () => { root!.render(<StrictMode>{element}</StrictMode>); }); }
async function reset(query = '') {
  if (root) await act(async () => { root!.unmount(); root = undefined; });
  localStorage.clear(); window.history.replaceState(null, '', '/ui/' + query);
  preferences = { ses_a: { model_id: 'profile:fast', approval_mode: 'safe' }, ses_b: { ...defaultPrefs } };
  models = [...initialModels]; providers = []; seen = []; linked = false; scrolledTo = null; connectionFailure = false; saveFailure = false;
}
async function choose(label: string, value: string) {
  if (label === '审批模式') {
    await act(async () => { container.querySelector<HTMLButtonElement>('button[aria-label="审批模式"]')!.click(); });
    const index = ['ask', 'safe', 'workspace-write', 'full-auto'].indexOf(value);
    await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[index].click(); });
    if (value === 'full-auto') {
      assert.equal(container.querySelector<HTMLDialogElement>('dialog')?.open, true);
      assert.equal(container.querySelector<HTMLButtonElement>('dialog .primary')!.disabled, true);
      await act(async () => { container.querySelector<HTMLInputElement>('dialog input[type="checkbox"]')!.click(); });
      await act(async () => { container.querySelector<HTMLButtonElement>('dialog .primary')!.click(); });
    }
    return;
  }
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
async function clickText(text: string) {
  const button = [...container.querySelectorAll<HTMLButtonElement>('button')].find((b) => b.textContent === text);
  assert.ok(button, 'Missing button: ' + text); assert.equal(button.disabled, false);
  await act(async () => { button.click(); });
}
async function submitProvider() {
  await act(async () => { container.querySelector('.provider-form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); });
}
try {
  await test('session preferences load, save, and stay separate across conversations', async () => {
    await reset(); localStorage.setItem('cloud-harness.web-session', 'ses_a'); await mount();
    assert.equal(container.querySelector('button[aria-label="审批模式"]')!.textContent, '下一轮 · 文件读取与 Git 查看自动');
    assert.equal(container.querySelector<HTMLSelectElement>('select[aria-label="会话模型"]')!.value, 'profile:fast');
    assert.equal([...container.querySelectorAll('.sidebar-nav button')].some((b) => b.textContent === '审批'), false);
    assert.ok(container.textContent?.includes('观测'));
    await choose('审批模式', 'workspace-write');
    assert.deepEqual(preferences.ses_a, { model_id: 'profile:fast', approval_mode: 'workspace-write' });
    await act(async () => { container.querySelector<HTMLButtonElement>('[data-session-id="ses_b"]')!.click(); });
    assert.equal(container.querySelector<HTMLSelectElement>('select[aria-label="会话模型"]')!.value, 'default');
    await choose('会话模型', 'profile:fast');
    assert.deepEqual(preferences.ses_b, { model_id: 'profile:fast', approval_mode: 'ask' });
  });
  await test('new conversation keeps local choices, then persists them and passes createRun options', async () => {
    await reset(); await mount(); await choose('审批模式', 'full-auto'); await choose('会话模型', 'profile:fast');
    assert.equal(seen.some((r) => r.method === 'PUT' && r.path.includes('/preferences')), false);
    assert.deepEqual(JSON.parse(localStorage.getItem('cloud-harness.web-draft-preferences')!), { approval_mode: 'full-auto', model_id: 'profile:fast' });
    await type('textarea', 'hello');
    await act(async () => { container.querySelector<HTMLButtonElement>('button[aria-label="发送"]')!.click(); });
    assert.deepEqual(preferences.ses_created, { approval_mode: 'full-auto', model_id: 'profile:fast' });
    assert.deepEqual(seen.find((r) => r.path.endsWith('/runs?wait=0'))?.body, { message: 'hello', approval_mode: 'full-auto', model_id: 'profile:fast' });
  });
  await test('an existing session confirms full-auto before persisting the next-turn preference', async () => {
    await reset(); localStorage.setItem('cloud-harness.web-session', 'ses_a'); await mount();
    const mode = container.querySelector<HTMLButtonElement>('button[aria-label="审批模式"]')!;
    await act(async () => { mode.click(); });
    await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[3].click(); });
    assert.equal(seen.some((r) => r.method === 'PUT' && r.path.includes('/preferences')), false);
    assert.deepEqual(preferences.ses_a, { model_id: 'profile:fast', approval_mode: 'safe' });
    assert.equal(container.querySelector<HTMLButtonElement>('dialog .primary')!.disabled, true);
    await act(async () => { container.querySelector<HTMLInputElement>('dialog input[type="checkbox"]')!.click(); });
    await act(async () => { container.querySelector<HTMLButtonElement>('dialog .primary')!.click(); });
    assert.deepEqual(preferences.ses_a, { model_id: 'profile:fast', approval_mode: 'full-auto' });
    assert.ok(seen.some((r) => r.method === 'PUT' && r.path === '/v1/sessions/ses_a/preferences' && r.body?.approval_mode === 'full-auto'));
    assert.equal(container.querySelector('dialog'), null);
  });
  await test('notification run deep link selects its conversation and visible WaitCard', async () => {
    await reset('?run=run_wait'); linked = true; await mount();
    assert.equal(localStorage.getItem('cloud-harness.web-session'), 'ses_a');
    assert.ok(container.querySelector('.wait-card'));
    assert.equal(container.querySelector('.work-toggle')?.getAttribute('aria-expanded'), 'false');
    assert.equal(scrolledTo, container.querySelector('.wait-card'));
    assert.ok(container.textContent?.includes('最终回答始终可见'));
  });
  await test('chat header owns model selection and composer only offers the current approval chip', async () => {
    await reset(); await mount();
    assert.ok(container.querySelector('header select[aria-label="会话模型"]'));
    assert.equal(container.querySelector('header label'), null);
    const composer = container.querySelector('.composer')!;
    assert.equal(composer.querySelector('select, input, form, .provider-editor'), null);
    assert.equal(composer.querySelectorAll('textarea').length, 1);
    assert.equal(composer.querySelector('button[aria-label="审批模式"]')!.textContent, '下一轮 · 按策略确认');
    assert.equal(composer.querySelector('.approval-mode-popover'), null);
    await act(async () => { composer.querySelector<HTMLButtonElement>('button[aria-label="审批模式"]')!.click(); });
    assert.ok(composer.textContent?.includes('仅下一轮生效'));
    await act(async () => { document.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true })); });
    assert.equal(composer.querySelector('.approval-mode-popover'), null);
    assert.equal(seen.some((r) => r.path === '/v1/providers'), false);
  });
  await test('settings CRUD, blank key edits and connection checks never auto-select models or lose the draft', async () => {
    await reset(); localStorage.setItem('cloud-harness.web-session', 'ses_a'); await mount();
    await type('textarea', '保留我的草稿');
    await clickText('设置');
    assert.ok(container.querySelector('[aria-label="模型服务设置"]'));
    assert.equal(container.querySelector('textarea'), null);
    assert.ok(container.querySelector<HTMLInputElement>('[aria-label="搜索模型服务"]'));
    await clickText('添加模型服务');
    await type('#provider-name', 'Fixture'); await type('#provider-url', 'http://127.0.0.1:1234/v1'); await type('#provider-model', 'custom'); await type('#provider-key', 'test-only-form-key');
    assert.equal(container.querySelector<HTMLInputElement>('#provider-key')!.type, 'password');
    await submitProvider();
    assert.equal(providers.length, 1);
    const id = providers[0].id;
    assert.equal(container.querySelector('input[type="password"]'), null);
    assert.ok(container.textContent?.includes('密钥已配置'));
    assert.equal(container.innerHTML.includes('test-only-form-key'), false);
    assert.deepEqual(preferences.ses_a, { model_id: 'profile:fast', approval_mode: 'safe' });
    await clickText('刷新模型列表'); assert.ok(container.textContent?.includes('已刷新模型列表'));
    connectionFailure = true; await clickText('刷新模型列表'); assert.ok(container.querySelector('.provider-card [role="alert"]'));
    await clickText('编辑');
    assert.equal(container.querySelector<HTMLInputElement>('#provider-key')!.value, '');
    assert.equal(container.querySelector<HTMLInputElement>('#provider-key')!.placeholder, '已保存；留空保留原值');
    await type('#provider-name', 'Renamed'); await submitProvider();
    const saves = seen.filter((r) => r.path === '/v1/providers' && r.method === 'PUT');
    assert.equal(saves.length, 2); assert.equal(saves[1].body?.api_key, undefined); assert.equal(saves[1].body?.id, id);
    await clickText('返回会话');
    assert.equal(container.querySelector<HTMLTextAreaElement>('textarea')!.value, '保留我的草稿');
    assert.equal(container.querySelector<HTMLSelectElement>('.header-model')!.value, 'profile:fast');
    await choose('会话模型', 'custom:' + id);
    await clickText('设置'); await clickText('删除');
    assert.ok(container.textContent?.includes('当前模型可能不可用'));
    assert.equal(seen.some((r) => r.method === 'DELETE'), false);
    await clickText('取消'); assert.equal(providers.length, 1);
    await clickText('删除'); await clickText('确认删除'); assert.equal(providers.length, 0);
    assert.equal(preferences.ses_a.model_id, 'custom:' + id);
    await clickText('返回会话');
    assert.ok(container.querySelector('.header-model')?.textContent?.includes('模型不可用'));
    assert.ok(container.textContent?.includes('当前模型不可用，请重新选择'));
    assert.equal(container.querySelector<HTMLButtonElement>('[aria-label="发送"]')!.disabled, true);
    assert.equal(container.querySelector<HTMLTextAreaElement>('textarea')!.value, '保留我的草稿');
    await choose('会话模型', 'default');
    assert.equal(container.querySelector<HTMLButtonElement>('[aria-label="发送"]')!.disabled, false);
  });
  await test('no configured models has a settings action and preserves the unsent draft', async () => {
    await reset(); models = []; await mount(); await type('textarea', '尚未发送');
    assert.ok(container.textContent?.includes('还没有已配置模型'));
    assert.equal(container.querySelector<HTMLButtonElement>('[aria-label="发送"]')!.disabled, true);
    await clickText('去配置模型服务'); await clickText('返回会话');
    assert.equal(container.querySelector<HTMLTextAreaElement>('textarea')!.value, '尚未发送');
    assert.equal(seen.some((r) => r.path.endsWith('/runs?wait=0')), false);
  });
  await test('provider save failure stays in settings without losing editable values', async () => {
    await reset(); await mount(); await clickText('设置'); await clickText('添加模型服务');
    await type('#provider-name', 'Fixture'); await type('#provider-url', 'https://example.invalid/v1'); await type('#provider-model', 'fixture'); await type('#provider-key', 'test-only-form-key');
    saveFailure = true; await submitProvider();
    assert.ok(container.querySelector('.provider-form [role="alert"]'));
    assert.equal(container.querySelector<HTMLInputElement>('#provider-name')!.value, 'Fixture');
    assert.equal(providers.length, 0);
    assert.equal(seen.some((r) => r.method === 'PUT' && r.path.includes('/preferences')), false);
  });
} finally {
  if (root) await act(async () => { root!.unmount(); });
  dom.window.close();
}
