import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, StrictMode } from 'react';
import type { Root } from 'react-dom/client';
import type { CustomProvider, ModelDetection, ModelOption, SessionPreferences } from '../src/api';

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
let modelOverrides: Partial<ModelOption> = {};

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
  else if (path === '/v1/models') result = [{ id: 'default', name: 'Agnes', model: 'agnes-3.0-flash', provider: 'openai', thinking, ...modelOverrides }, { id: 'custom:' + customProviderID + '/sonnet', legacy_id: 'custom:' + customProviderID, name: '服务 · sonnet', model: 'sonnet', provider: 'anthropic', provider_id: customProviderID, provider_name: '服务', thinking: 'supported' }];
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
  seen = []; failDetection = false; thinking = 'unknown'; holdDetection = false; deferred = undefined; customProviderID = 'provider'; detectionOverride = undefined; modelOverrides = {};
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
async function toggleModel(id: string) {
  const input = container.querySelector<HTMLInputElement>('[aria-label="启用 ' + id + '"]')!;
  assert.ok(input); assert.equal(input.disabled, false);
  await act(async () => { input.click(); });
}
async function setDefault(id: string) {
  const input = container.querySelector<HTMLInputElement>('[aria-label="默认模型 ' + id + '"]')!;
  assert.ok(input); assert.equal(input.disabled, false);
  await act(async () => { input.click(); });
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
    assert.equal(container.querySelector('datalist'), null);
    assert.equal(container.querySelector<HTMLInputElement>('[aria-label="默认模型 sonnet"]')!.checked, true);
    assert.deepEqual([...container.querySelectorAll('.provider-model-row')].map((item) => item.getAttribute('data-model-id')), ['sonnet', 'haiku']);
    assert.ok(container.querySelector('[data-model-id="sonnet"]')?.textContent?.includes('Sonnet'));
    await toggleModel('haiku');
    await submit();
    assert.equal(providers[0].protocol, 'anthropic'); assert.equal(providers[0].protocol_source, 'auto'); assert.deepEqual(providers[0].models, ['sonnet', 'haiku']);
    assert.ok(container.querySelector('.provider-heading .chip')?.textContent?.includes('Anthropic · 2 个模型'));
    assert.equal(container.innerHTML.includes('form-only-fixture-key'), false);
    assert.equal(seen.some((r) => r.path.includes('/preferences')), false);
  });
  await test('dual-protocol detection prefers OpenAI and mentions the alternative in the title', async () => {
    await reset(); detectionOverride = { protocol: 'openai', models: [{ id: 'agnes-3.0-flash' }], fetched_at: fetchedAt, alternatives: ['anthropic'] };
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
    await type('#provider-model-search', 'manual-model'); await click('添加并勾选'); await submit();
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
    await click('编辑'); assert.equal(container.querySelectorAll('.provider-model-row').length, 2);
  });
  await test('late automatic discovery cannot overwrite a manual protocol', async () => {
    await reset(); await add(); holdDetection = true; await blur('#provider-key');
    assert.ok(deferred); await choose('#provider-protocol', 'openai');
    await act(async () => { deferred!(new Response(JSON.stringify(detection))); });
    assert.equal(container.querySelector<HTMLSelectElement>('#provider-protocol')!.value, 'openai');
    assert.equal(container.querySelectorAll('.provider-model-row').length, 0);
  });
  await test('single list filters names and IDs, adds manual IDs, and protects the selected default', async () => {
    await reset(); await add(); await click('识别');
    const defaultCheckbox = () => container.querySelector<HTMLInputElement>('[aria-label="启用 sonnet"]')!;
    assert.equal(defaultCheckbox().checked, true); assert.equal(defaultCheckbox().disabled, true);
    assert.equal(container.querySelector<HTMLInputElement>('[aria-label="默认模型 haiku"]')!.disabled, true);
    await type('#provider-model-search', 'HAI');
    assert.deepEqual([...container.querySelectorAll('.provider-model-row')].map((row) => row.getAttribute('data-model-id')), ['haiku']);
    await toggleModel('haiku'); await setDefault('haiku');
    await type('#provider-model-search', ''); await toggleModel('sonnet');
    assert.equal(defaultCheckbox().checked, false);
    await type('#provider-model-search', 'private-new-id'); await click('添加并勾选');
    assert.equal(container.querySelector<HTMLInputElement>('[aria-label="启用 private-new-id"]')!.checked, true);
    await setDefault('private-new-id'); await toggleModel('haiku');
    const manual = container.querySelector<HTMLInputElement>('[aria-label="启用 private-new-id"]')!;
    assert.equal(manual.disabled, true); await act(async () => { manual.click(); }); assert.equal(manual.checked, true);
    await submit();
    assert.equal(providers[0].model, 'private-new-id'); assert.deepEqual(providers[0].models, ['private-new-id']);
  });
  await test('other models start collapsed and selected or default other models stay visible', async () => {
    await reset(); detectionOverride = { protocol: 'openai', fetched_at: fetchedAt, models: [
      { id: 'stepaudio-2.5-tts', category: 'other' },
      { id: 'step-5-preview', display_name: 'Step 5 Preview', category: 'chat' },
      { id: 'stepaudio-2.5-asr', category: 'other' }, { id: 'stepaudio-2.5-chat', category: 'other' },
      { id: 'step-router-v1' }, { id: 'stepaudio-2.5-realtime' },
    ] };
    await add(); await click('识别');
    assert.equal(container.querySelector<HTMLInputElement>('[aria-label="默认模型 step-5-preview"]')!.checked, true);
    const details = container.querySelector<HTMLDetailsElement>('.provider-other-models')!;
    assert.equal(details.open, false); assert.equal(details.querySelectorAll('.provider-model-row').length, 5);
    assert.equal(container.querySelector('[data-model-id="step-5-preview"]')!.closest('details'), null);
    await act(async () => { details.open = true; }); await toggleModel('stepaudio-2.5-tts');
    const tts = container.querySelector('[data-model-id="stepaudio-2.5-tts"]')!;
    assert.equal(tts.closest('details'), null); await setDefault('stepaudio-2.5-tts');
    await act(async () => { container.querySelector<HTMLDetailsElement>('details')!.open = false; });
    assert.equal(container.querySelector('[data-model-id="stepaudio-2.5-tts"]')!.closest('details'), null);
    await submit(); assert.equal(providers[0].model, 'stepaudio-2.5-tts'); assert.ok(providers[0].models?.includes('stepaudio-2.5-tts'));
    await click('编辑');
    assert.equal(container.querySelector('[data-model-id="stepaudio-2.5-tts"]')!.closest('details'), null);
    assert.equal(container.querySelector<HTMLInputElement>('[aria-label="启用 stepaudio-2.5-tts"]')!.disabled, true);
  });
  await test('manual IDs can be saved without a successful catalog or reasoning choice', async () => {
    await reset(); await add(); await submit(); assert.equal(providers.length, 0);
    assert.ok(container.querySelector('[role="alert"]')?.textContent?.includes('默认'));
    await type('#provider-model-search', 'unlisted-router'); await click('添加并勾选');
    assert.equal(container.querySelector('[data-model-id="unlisted-router"]')!.closest('details'), null);
    await submit(); assert.equal(providers[0].model, 'unlisted-router');
  });
  await test('toggle reasoning offers only off and on and displays stored effort as on', async () => {
    await reset(); modelOverrides = { reasoning_type: 'toggle', thinking: 'supported' }; prefs.reasoning_effort = 'high';
    localStorage.setItem('cloud-harness.web-session', 'session'); await mount(<App />);
    const trigger = container.querySelector<HTMLButtonElement>('header [aria-label="思考强度"]')!;
    assert.equal(trigger.textContent, '思考：开'); await act(async () => { trigger.click(); });
    const menu = container.querySelector('[aria-label="选择思考强度"]')!;
    assert.deepEqual([...menu.querySelectorAll('button')].map((option) => option.textContent), ['关', '开']);
    await click('关'); assert.equal(prefs.reasoning_effort, 'off'); assert.equal(trigger.textContent, '思考：关');
    await act(async () => { trigger.click(); }); await click('开'); assert.equal(prefs.reasoning_effort, 'default');
  });
  await test('effort reasoning disables missing levels without expanding max to high', async () => {
    for (const values of [['low', 'high'], ['low', 'medium', 'high'], ['minimal', 'max', 'none']]) {
      await reset(); modelOverrides = { reasoning_type: 'effort', reasoning_values: values, thinking: 'supported' };
      localStorage.setItem('cloud-harness.web-session', 'session'); await mount(<App />);
      await act(async () => { container.querySelector<HTMLButtonElement>('header [aria-label="思考强度"]')!.click(); });
      const options = [...container.querySelectorAll<HTMLButtonElement>('[aria-label="选择思考强度"] button')];
      const disabled = (label: string) => options.find((item) => item.textContent === label)!.disabled;
      assert.equal(disabled('默认'), false); assert.equal(disabled('低'), false);
      assert.equal(disabled('中'), !values.includes('medium')); assert.equal(disabled('高'), !values.includes('high'));
      assert.equal(disabled('关'), !values.includes('none'));
      if (disabled('中')) { const medium = options.find((item) => item.textContent === '中')!; await act(async () => { medium.click(); }); assert.equal(seen.some((r) => r.method === 'PUT'), false); }
    }
  });
  await test('a late discovery cannot replace a manually added default', async () => {
    await reset(); await add(); holdDetection = true; await blur('#provider-key'); assert.ok(deferred);
    await type('#provider-model-search', 'manual-first'); await click('添加并勾选');
    await act(async () => { deferred!(new Response(JSON.stringify(detection))); });
    assert.equal(container.querySelector<HTMLInputElement>('[aria-label="默认模型 manual-first"]')!.checked, true);
    assert.equal(container.querySelector('[data-model-id="sonnet"]'), null);
    await submit(); assert.equal(providers[0].model, 'manual-first');
  });
  await test('a legacy missing effort is displayed as the service default', async () => {
    await reset(); modelOverrides = { reasoning_type: 'effort', reasoning_values: ['low', 'high'], thinking: 'supported' };
    prefs.reasoning_effort = 'medium'; localStorage.setItem('cloud-harness.web-session', 'session'); await mount(<App />);
    assert.equal(container.querySelector<HTMLButtonElement>('header [aria-label="思考强度"]')!.textContent, '思考：默认');
    assert.equal(seen.some((r) => r.method === 'PUT'), false);
  });
  await test('reasoning selection writes session preferences and models group by service', async () => {
    await reset(); localStorage.setItem('cloud-harness.web-session', 'session'); await mount(<App />);
    const trigger = container.querySelector<HTMLButtonElement>('header [aria-label="思考强度"]')!;
    assert.equal(trigger.tagName, 'BUTTON'); assert.equal(trigger.title, '下一轮生效'); assert.equal(trigger.textContent, '思考：默认');
    await act(async () => { trigger.click(); });
    const menu = container.querySelector('header [aria-label="选择思考强度"]')!;
    const options = [...menu.querySelectorAll<HTMLButtonElement>('button')];
    assert.deepEqual(options.map((option) => option.textContent), ['默认', '关', '低', '中', '高']);
    assert.equal(menu.textContent?.includes('思考：'), false);
    assert.equal(menu.querySelector('[aria-pressed="true"]')?.textContent, '默认');
    assert.ok(options[0].querySelector('svg'));
    await click('默认');
    assert.equal(trigger.getAttribute('aria-expanded'), 'false'); assert.equal(container.querySelector('[aria-label="选择思考强度"]'), null);
    assert.equal(seen.some((r) => r.method === 'PUT'), false);
    await act(async () => { trigger.click(); });
    await act(async () => { document.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true })); });
    assert.equal(container.querySelector('[aria-label="选择思考强度"]'), null); assert.equal(document.activeElement, trigger);
    await act(async () => { trigger.click(); });
    await act(async () => { container.dispatchEvent(new dom.window.Event('pointerdown', { bubbles: true })); });
    assert.equal(container.querySelector('[aria-label="选择思考强度"]'), null);
    await act(async () => { trigger.click(); });
    await click('高');
    assert.equal(prefs.reasoning_effort, 'high'); assert.equal(seen.find((r) => r.method === 'PUT')?.body?.reasoning_effort, 'high');
    assert.equal(trigger.textContent, '思考：高'); assert.equal(container.querySelector('[aria-label="选择思考强度"]'), null);
    await act(async () => { trigger.click(); });
    assert.equal(container.querySelector('[aria-label="选择思考强度"] [aria-pressed="true"]')?.textContent, '高');
    assert.deepEqual([...container.querySelectorAll('header optgroup')].map((group) => group.getAttribute('label')), ['默认服务', '服务']);
  });
  await test('unsupported reasoning disables the selector while unknown models may choose', async () => {
    await reset(); thinking = 'unsupported'; localStorage.setItem('cloud-harness.web-session', 'session');
    const listModels = api.listModels;
    api.listModels = async () => (await listModels()).map((model) => model.id === 'custom:provider/sonnet' ? { ...model, thinking: 'unknown' } : model);
    try { await mount(<App />); } finally { api.listModels = listModels; }
    const trigger = container.querySelector<HTMLButtonElement>('header [aria-label="思考强度"]')!;
    assert.equal(trigger.disabled, true); assert.ok(trigger.textContent?.includes('思考：不支持'));
    await act(async () => { trigger.click(); }); assert.equal(container.querySelector('[aria-label="选择思考强度"]'), null);
    await choose('select[aria-label="会话模型"]', 'custom:provider/sonnet');
    assert.equal(trigger.disabled, false); assert.equal(trigger.textContent, '思考：默认');
    await act(async () => { trigger.click(); });
    await click('低'); assert.equal(prefs.reasoning_effort, 'low'); assert.equal(trigger.textContent, '思考：低');
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
