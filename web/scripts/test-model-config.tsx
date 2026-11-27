import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, StrictMode } from 'react';
import type { Root } from 'react-dom/client';
import type { CustomProvider, ModelDetection, SessionPreferences } from '../src/api';

const dom = new JSDOM('<!doctype html><div id="root"></div>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event', 'MouseEvent', 'FocusEvent']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true, CSS: { escape: (s: string) => s.replace(/[^\w-]/g, '\\$&') } });
Object.assign(window, { matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }) });
dom.window.HTMLElement.prototype.scrollIntoView = function () {};
const { createRoot } = await import('react-dom/client');
const { default: App } = await import('../src/App');
const { Settings } = await import('../src/Settings');
const { api } = await import('../src/api');
api.streamEvents = async (_id, _after, _cb, signal) => { if (!signal.aborted) await new Promise<void>((resolve) => signal.addEventListener('abort', () => resolve(), { once: true })); };
const container = document.getElementById('root')!;
const fetchedAt = '2026-11-27T10:00:00Z';
const detection: ModelDetection = { protocol: 'anthropic', models: [{ id: 'sonnet', display_name: 'Sonnet', thinking: 'supported' }, { id: 'haiku', thinking: 'unsupported' }], fetched_at: fetchedAt };
let root: Root | undefined;
let providers: CustomProvider[] = [];
let prefs: SessionPreferences;
let failDetection = false;
let seen: { path: string; method: string; body?: Record<string, unknown> }[] = [];
let thinking: 'supported' | 'unsupported' | 'unknown' = 'unknown';
let deferred: ((value: Response) => void) | undefined;
let holdDetection = false;
let customProviderID = 'provider';
let detectionOverride: ModelDetection | undefined;

globalThis.fetch = async (input, init) => {
  const path = String(input), method = init?.method || 'GET';
  const body = init?.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined;
  seen.push({ path, method, body });
  let result: unknown;
  if (path === '/v1/providers/detect') {
    if (holdDetection) return await new Promise<Response>((resolve) => { deferred = resolve; });
    if (failDetection) return new Response(JSON.stringify({ error: '模型列表获取失败' }), { status: 502 });
    result = detectionOverride || (body?.protocol ? { ...detection, protocol: body.protocol } : detection);
  } else if (path === '/v1/providers') {
    if (method === 'PUT') {
      const value = body!;
      const provider: CustomProvider = { id: String(value.id), name: String(value.name), base_url: String(value.base_url), model: String(value.model), has_api_key: true, protocol: value.protocol as CustomProvider['protocol'], protocol_source: value.protocol_source as CustomProvider['protocol_source'], models: value.models as string[], catalog: value.catalog as CustomProvider['catalog'], catalog_fetched_at: value.catalog_fetched_at as string };
      providers = [...providers.filter((p) => p.id !== provider.id), provider]; result = provider;
    } else result = providers;
  } else if (path.endsWith('/refresh-models')) {
    if (failDetection) return new Response(JSON.stringify({ error: '模型列表获取失败' }), { status: 502 });
    result = { ...providers[0], catalog: detection.models, catalog_fetched_at: fetchedAt };
  }
  else if (path === '/healthz') result = { status: 'ok', provider: 'fixture' };
  else if (path === '/v1/models') result = [{ id: 'default', name: 'Agnes', model: 'agnes-3.0-flash', provider: 'openai', thinking }, { id: 'custom:' + customProviderID + '/sonnet', legacy_id: 'custom:' + customProviderID, name: '服务 · sonnet', model: 'sonnet', provider: 'anthropic', provider_id: customProviderID, provider_name: '服务', thinking: 'supported' }];
  else if (path === '/v1/sessions') result = [{ id: 'session', created_at: fetchedAt, updated_at: fetchedAt, run_count: 0 }];
  else if (path.endsWith('/preferences')) { if (method === 'PUT') prefs = body as unknown as SessionPreferences; result = prefs; }
  else if (path.includes('/messages')) result = { messages: [], next_cursor: '' };
  else if (path.endsWith('/runs')) result = [];
  else throw new Error('Unexpected request: ' + path);
  return new Response(JSON.stringify(result), { headers: { 'Content-Type': 'application/json' } });
};

async function reset() {
  if (root) await act(async () => { root!.unmount(); root = undefined; });
  localStorage.clear(); window.history.replaceState(null, '', '/ui/');
  providers = []; prefs = { approval_mode: 'ask', model_id: 'default', reasoning_effort: 'default' };
  seen = []; failDetection = false; thinking = 'unknown'; holdDetection = false; deferred = undefined; customProviderID = 'provider'; detectionOverride = undefined;
}
async function mount(element: React.ReactNode) { root = createRoot(container); await act(async () => { root!.render(<StrictMode>{element}</StrictMode>); }); }
async function click(text: string) {
  const button = [...container.querySelectorAll<HTMLButtonElement>('button')].find((b) => b.textContent === text);
  assert.ok(button, 'Missing button: ' + text); assert.equal(button.disabled, false);
  await act(async () => { button.click(); });
}
async function type(selector: string, value: string) {
  const input = container.querySelector<HTMLInputElement>(selector)!; assert.ok(input);
  await act(async () => {
    Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!.call(input, value);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
}
async function choose(selector: string, value: string) {
  const select = container.querySelector<HTMLSelectElement>(selector)!; assert.ok(select && !select.disabled);
  await act(async () => { select.value = value; select.dispatchEvent(new Event('change', { bubbles: true })); });
}
async function blur(selector: string) {
  await act(async () => { container.querySelector(selector)!.dispatchEvent(new dom.window.FocusEvent('focusout', { bubbles: true })); });
}
async function submit() { await act(async () => { container.querySelector('.provider-form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); }); }
async function add() {
  await mount(<Settings onModelsChanged={async () => {}} onBack={() => {}} />); await click('添加模型服务');
  await type('#provider-name', '我的服务'); await type('#provider-url', 'https://example.invalid/v1'); await type('#provider-key', 'form-only-fixture-key');
}

try {
  await test('blur detects protocol, fills searchable models, and enables multiple models', async () => {
    await reset(); await add(); await blur('#provider-key');
    assert.equal(container.querySelector<HTMLSelectElement>('#provider-protocol')!.value, 'anthropic');
    assert.ok(container.textContent?.includes('已识别：Anthropic'));
    assert.equal(container.querySelector<HTMLInputElement>('#provider-model')!.value, 'sonnet');
    assert.deepEqual([...container.querySelectorAll('#provider-model-catalog option')].map((item) => item.getAttribute('value')), ['sonnet', 'haiku']);
    await act(async () => { container.querySelectorAll<HTMLInputElement>('.provider-models input')[1].click(); });
    await submit();
    assert.equal(providers[0].protocol, 'anthropic'); assert.equal(providers[0].protocol_source, 'auto'); assert.deepEqual(providers[0].models, ['sonnet', 'haiku']);
    assert.ok(container.querySelector('.provider-heading .chip')?.textContent?.includes('Anthropic · 2 个模型'));
    assert.equal(container.innerHTML.includes('form-only-fixture-key'), false);
    assert.equal(seen.some((r) => r.path.includes('/preferences')), false);
  });
  await test('dual-protocol detection prefers OpenAI and mentions the alternative in the title', async () => {
    detectionOverride = { protocol: 'openai', models: [{ id: 'agnes-3.0-flash' }], fetched_at: fetchedAt, alternatives: ['anthropic'] };
    await add();
    await type('#provider-url', 'https://example.invalid/v1');
    await type('#provider-key', 'secret-key');
    await blur('#provider-key');
    const select = container.querySelector<HTMLSelectElement>('#provider-protocol')!;
    assert.equal(select.value, 'openai');
    assert.equal(select.title, '该服务也兼容 Anthropic；可手动更改');
  });

  await test('manual protocol override only fetches that protocol and survives save', async () => {
    await reset(); await add(); await click('识别');
    await choose('#provider-protocol', 'gemini'); await click('识别');
    assert.equal(seen.filter((r) => r.path === '/v1/providers/detect').at(-1)?.body?.protocol, 'gemini');
    assert.equal(container.querySelector<HTMLSelectElement>('#provider-protocol')!.value, 'gemini');
    await submit(); assert.equal(providers[0].protocol_source, 'manual'); assert.equal(providers[0].protocol, 'gemini');
  });
  await test('failed catalog fetch still allows a manual model and save', async () => {
    await reset(); await add(); failDetection = true; await click('识别');
    assert.ok(container.textContent?.includes('可手填模型名并保存'));
    await type('#provider-model', 'manual-model'); await submit();
    assert.equal(providers[0].model, 'manual-model'); assert.deepEqual(providers[0].models, ['manual-model']);
  });
  await test('editing reuses saved credentials and failed refresh keeps cached models', async () => {
    await reset(); providers = [{ id: 'saved', name: 'Saved', base_url: 'https://example.invalid/v1', model: 'sonnet', has_api_key: true, protocol: 'anthropic', protocol_source: 'manual', models: ['sonnet'], catalog: detection.models }];
    await mount(<Settings onModelsChanged={async () => {}} onBack={() => {}} />); await click('编辑'); await click('识别');
    const body = seen.find((r) => r.path === '/v1/providers/detect')?.body;
    assert.equal(body?.provider_id, 'saved'); assert.equal(body?.api_key, undefined); assert.equal(body?.protocol, 'anthropic');
    await submit(); assert.equal(seen.find((r) => r.method === 'PUT')?.body?.api_key, undefined);
    await click('刷新模型列表'); assert.ok(container.textContent?.includes('已刷新模型列表'));
    failDetection = true; await click('刷新模型列表'); assert.ok(container.querySelector('.provider-card [role="alert"]'));
    await click('编辑'); assert.equal(container.querySelectorAll('#provider-model-catalog option').length, 2);
  });
  await test('late automatic discovery cannot overwrite a manual protocol', async () => {
    await reset(); await add(); holdDetection = true; await blur('#provider-key');
    assert.ok(deferred); await choose('#provider-protocol', 'openai');
    await act(async () => { deferred!(new Response(JSON.stringify(detection))); });
    assert.equal(container.querySelector<HTMLSelectElement>('#provider-protocol')!.value, 'openai');
    assert.equal(container.querySelectorAll('#provider-model-catalog option').length, 0);
  });
  await test('reasoning selection writes session preferences and models group by service', async () => {
    await reset(); localStorage.setItem('cloud-harness.web-session', 'session'); await mount(<App />);
    const select = container.querySelector<HTMLSelectElement>('header select[aria-label="思考强度"]')!;
    assert.equal(select.title, '下一轮生效'); assert.equal(select.value, 'default');
    await choose('select[aria-label="思考强度"]', 'high');
    assert.equal(prefs.reasoning_effort, 'high'); assert.equal(seen.find((r) => r.method === 'PUT')?.body?.reasoning_effort, 'high');
    assert.deepEqual([...container.querySelectorAll('header optgroup')].map((group) => group.getAttribute('label')), ['默认服务', '服务']);
  });
  await test('unsupported reasoning disables the selector while unknown models may choose', async () => {
    await reset(); thinking = 'unsupported'; localStorage.setItem('cloud-harness.web-session', 'session'); await mount(<App />);
    const select = container.querySelector<HTMLSelectElement>('select[aria-label="思考强度"]')!;
    assert.equal(select.disabled, true); assert.ok(select.textContent?.includes('不支持'));
    await choose('select[aria-label="会话模型"]', 'custom:provider/sonnet');
    assert.equal(container.querySelector<HTMLSelectElement>('select[aria-label="思考强度"]')!.disabled, false);
    await choose('select[aria-label="思考强度"]', 'low'); assert.equal(prefs.reasoning_effort, 'low');
  });
  await test('legacy custom selection remains available in the header', async () => {
    await reset(); prefs.model_id = 'custom:provider'; localStorage.setItem('cloud-harness.web-session', 'session'); await mount(<App />);
    assert.equal(container.querySelector<HTMLSelectElement>('select[aria-label="会话模型"]')!.value, 'custom:provider/sonnet');
    assert.equal(container.textContent?.includes('当前模型不可用'), false);
  });
  await test('a service named default still has its own model group', async () => {
    await reset(); customProviderID = 'default'; await mount(<App />);
    assert.deepEqual([...container.querySelectorAll('header optgroup')].map((group) => group.getAttribute('label')), ['默认服务', '服务']);
  });
} finally {
  if (root) await act(async () => { root!.unmount(); }); dom.window.close();
}
