import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, StrictMode, useState } from 'react';
import type { Root } from 'react-dom/client';
import type { ModelOption, ReasoningEffort } from '../src/api';

const dom = new JSDOM('<!doctype html><div id="root"></div><button id="outside">外部</button>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'HTMLElement', 'Event', 'MouseEvent']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
// jsdom has no layout or pointer capture. Dispatch real React pointer events
// against a measured 400px fixture, with the same IDs used by browsers.
const captures = new Map<HTMLElement, number>();
dom.window.HTMLElement.prototype.setPointerCapture = function (id) { captures.set(this, id); };
dom.window.HTMLElement.prototype.releasePointerCapture = function (id) { if (captures.get(this) === id) captures.delete(this); };
const { createRoot } = await import('react-dom/client');
const { ReasoningEffortSlider } = await import('../src/ReasoningEffort');
const { ReasoningEffortPicker } = await import('../src/ReasoningEffort');
const { ModelPicker } = await import('../src/ModelPicker');
const { Composer } = await import('../src/Chat');
const container = document.getElementById('root')!;
const base: ModelOption = { id: 'default', name: 'Default', model: 'base', provider: 'fixture', thinking: 'supported' };
let root: Root | undefined;
const changes: ReasoningEffort[] = [];
const modelChanges: string[] = [];
function SliderFixture({ model = base, initial = 'default', disabled = false }: { model?: ModelOption; initial?: ReasoningEffort; disabled?: boolean }) {
  const [value, setValue] = useState(initial);
  return <ReasoningEffortSlider model={model} value={value} disabled={disabled} onChange={(next) => { changes.push(next); setValue(next); }} />;
}
function ModelFixture({ models, initial = models[0], loading = false, disabled = false }: { models: ModelOption[]; initial?: ModelOption; loading?: boolean; disabled?: boolean }) {
  const [selected, setSelected] = useState(initial);
  return <ModelPicker models={models} selectedModel={selected} loading={loading} disabled={disabled} onChange={(id) => { modelChanges.push(id); setSelected(models.find((model) => model.id === id)); }} />;
}
function EffortPickerFixture({ models, initialModel = models[0], initial = 'default', selectionDisabled = false }: { models: ModelOption[]; initialModel?: ModelOption; initial?: ReasoningEffort; selectionDisabled?: boolean }) {
  const [selected, setSelected] = useState(initialModel);
  const [value, setValue] = useState(initial);
  return <ReasoningEffortPicker models={models} selectedModel={selected} value={value} modelSelectionDisabled={selectionDisabled} onModelChange={(id) => { modelChanges.push(id); setSelected(models.find((model) => model.id === id)); }} onChange={(next) => { changes.push(next); setValue(next); }} />;
}
async function mount(element: React.ReactNode) {
  if (root) await act(async () => { root!.unmount(); });
  changes.length = 0; modelChanges.length = 0; captures.clear();
  root = createRoot(container);
  await act(async () => { root!.render(<StrictMode>{element}</StrictMode>); });
}
const slider = () => container.querySelector<HTMLElement>('[role="slider"]')!;
const text = () => slider().getAttribute('aria-valuetext');
async function key(value: string, target: Element = slider()) {
  await act(async () => { target.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: value, bubbles: true, cancelable: true })); });
}
async function pointer(type: string, at: number, id = 7) {
  const track = container.querySelector<HTMLElement>('.reasoning-track')!;
  track.getBoundingClientRect = () => ({ left: 100, width: 400, top: 0, height: 30, right: 500, bottom: 30, x: 100, y: 0, toJSON() {} });
  const event = new dom.window.MouseEvent(type, { bubbles: true, cancelable: true, clientX: 100 + at / Number(slider().getAttribute('aria-valuemax')) * 400, button: 0 });
  Object.defineProperties(event, { pointerId: { value: id }, isPrimary: { value: true } });
  await act(async () => { track.dispatchEvent(event); });
}
async function tap(at: number) { await pointer('pointerdown', at); await pointer('pointerup', at); }
const trigger = () => container.querySelector<HTMLButtonElement>('button[aria-label="会话模型"]')!;
const effortTrigger = () => document.querySelector<HTMLButtonElement>('.reasoning-trigger')!;
const effortSlider = () => document.querySelector<HTMLElement>('.reasoning-popover [role="slider"]')!;
const options = () => [...container.querySelectorAll<HTMLButtonElement>('[role="option"]')];
async function open() { await act(async () => { trigger().click(); }); }
async function openEffort() { await act(async () => { effortTrigger().click(); }); }
async function searchGlobal(value: string) {
  const input = document.querySelector<HTMLInputElement>('[aria-label="搜索会话模型"]')!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!.call(input, value);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
}
async function search(value: string) {
  const input = container.querySelector<HTMLInputElement>('[aria-label="搜索会话模型"]')!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!.call(input, value);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

try {
  const composer = (overrides: Partial<React.ComponentProps<typeof Composer>> = {}) => <Composer value="hello" onChange={() => {}} onSubmit={() => {}} busy={false} placeholder="发消息…" approvalMode="ask" onApprovalModeChange={() => {}} models={[base]} selectedModel={base} onModelChange={() => {}} onReasoningChange={() => {}} {...overrides} />;
  await test('composer omits empty hints and keyboard tips while retaining useful status text', async () => {
    for (const hint of [undefined, '']) {
      await mount(composer({ hint }));
      assert.equal(container.querySelector('.composer-hint'), null);
      assert.equal(container.querySelector('textarea')?.placeholder, '发消息…');
      const send = container.querySelector<HTMLButtonElement>('[aria-label="发送"]')!;
      assert.equal(send.title, '发送');
      assert.doesNotMatch(container.innerHTML, /Enter|回车/);
    }
    for (const hint of ['未连接', '回复后继续本轮']) {
      await mount(composer({ hint }));
      assert.equal(container.querySelector('.composer-hint')?.textContent, hint);
    }
  });
  await test('Enter sends, Shift+Enter and IME Enter do not; busy or blank drafts cannot send', async () => {
    let submits = 0;
    const onSubmit = () => { submits++; };
    const enter = async (options: KeyboardEventInit = {}) => {
      const event = new dom.window.KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true, ...options });
      await act(async () => { container.querySelector('textarea')!.dispatchEvent(event); });
      return event.defaultPrevented;
    };
    await mount(composer({ onSubmit }));
    assert.equal(await enter({ shiftKey: true }), false);
    assert.equal(submits, 0);
    assert.equal(await enter({ isComposing: true }), false);
    assert.equal(submits, 0);
    assert.equal(await enter(), true);
    assert.equal(submits, 1);
    for (const state of [{ busy: true }, { value: '   ' }]) {
      await mount(composer({ ...state, onSubmit }));
      await enter();
      assert.equal(submits, 1);
      assert.equal(container.querySelector<HTMLButtonElement>('[aria-label="发送"]')!.disabled, true);
    }
  });
  await test('clicks and drags snap only to available notches, and save once on release', async () => {
    await mount(<SliderFixture model={{ ...base, reasoning_type: 'effort', reasoning_values: ['low', 'high'] }} />);
    assert.equal(slider().title, '下一轮生效'); assert.equal(slider().getAttribute('aria-label'), '思考强度');
    assert.equal(container.querySelector('input[type="range"], select, datalist'), null);
    await tap(1); assert.equal(text(), '默认'); assert.deepEqual(changes, []);
    await tap(1.1); assert.equal(text(), '低'); assert.deepEqual(changes, ['low']);
    await pointer('pointerdown', 2); await pointer('pointermove', 3);
    assert.equal(text(), '高'); assert.equal(slider().getAttribute('data-dragging'), 'true'); assert.equal(container.querySelector<HTMLElement>('.reasoning-thumb')!.style.left, '300px');
    await pointer('pointermove', 3.6); assert.equal(text(), '高'); assert.deepEqual(changes, ['low']);
    await pointer('pointerup', 3.6); assert.equal(text(), '高'); assert.deepEqual(changes, ['low', 'high']);
    await tap(2.93); assert.equal(text(), '低'); assert.equal(slider().querySelector('[data-current="true"]')!.getAttribute('data-available'), 'true');
    await tap(-5); assert.equal(text(), '默认'); await tap(9); assert.equal(text(), '高');
    assert.equal(captures.size, 0);
  });
  await test('pointer cancellation and a foreign pointer never commit a draft', async () => {
    await mount(<SliderFixture />);
    await pointer('pointerdown', 4); assert.equal(text(), '高');
    await pointer('pointermove', 1, 99); assert.equal(text(), '高');
    await pointer('pointerup', 1, 99); assert.deepEqual(changes, []);
    await pointer('pointercancel', 4); assert.equal(text(), '默认'); assert.deepEqual(changes, []);
    await pointer('pointerdown', 4); await pointer('lostpointercapture', 4);
    assert.equal(text(), '默认'); assert.deepEqual(changes, []);
  });
  await test('keyboard arrows skip unavailable off and medium levels and stop at boundaries', async () => {
    await mount(<SliderFixture model={{ ...base, reasoning_type: 'effort', reasoning_values: ['minimal', 'high'] }} />);
    await key('ArrowLeft'); assert.deepEqual(changes, []);
    await key('ArrowRight'); assert.equal(text(), '低');
    await key('ArrowRight'); assert.equal(text(), '高');
    await key('ArrowRight'); assert.deepEqual(changes, ['low', 'high']);
    await key('ArrowLeft'); assert.equal(text(), '低');
    await key('Home'); assert.equal(text(), '默认'); await key('End'); assert.equal(text(), '高');
  });
  await test('toggle has exactly off/on, normalizes legacy efforts visually, and stores off/default', async () => {
    await mount(<SliderFixture model={{ ...base, reasoning_type: 'toggle' }} initial="high" />);
    assert.deepEqual([...container.querySelectorAll('.reasoning-notch')].map((item) => item.getAttribute('data-effort')), ['off', 'default']);
    assert.equal(text(), '开'); assert.equal(slider().getAttribute('aria-valuemax'), '1'); assert.deepEqual(changes, []);
    await tap(0); assert.equal(text(), '关'); await key('ArrowRight'); assert.equal(text(), '开');
    assert.deepEqual(changes, ['off', 'default']);
  });
  await test('unsupported and busy sliders disable pointer and keyboard input and cancel drafts', async () => {
    for (const props of [{ model: { ...base, thinking: 'unsupported' as const } }, { disabled: true }]) {
      await mount(<SliderFixture {...props} />);
      assert.equal(slider().getAttribute('aria-disabled'), 'true'); assert.equal(slider().tabIndex, -1);
      await tap(4); await key('End'); assert.deepEqual(changes, []);
      assert.equal(text(), props.model ? '不支持' : '默认');
      assert.equal(container.querySelector('[aria-label="选择思考强度"]'), null);
    }
    await mount(<SliderFixture />); await pointer('pointerdown', 4); assert.equal(text(), '高');
    await act(async () => { root!.render(<StrictMode><SliderFixture disabled /></StrictMode>); });
    await pointer('pointerup', 4); assert.equal(text(), '默认'); assert.deepEqual(changes, []);
  });
  await test('unknown capabilities keep all levels, missing stored efforts fall back without saving', async () => {
    for (const model of [{ ...base, thinking: 'unknown' as const, reasoning_type: 'effort' as const, reasoning_values: ['low'] }, { ...base, thinking: undefined, reasoning_type: 'toggle' as const }, { ...base }]) {
      await mount(<SliderFixture model={model} />);
      assert.equal(container.querySelectorAll('[data-available="true"]').length, 5);
      await key('End'); assert.equal(text(), '高');
    }
    await mount(<SliderFixture model={{ ...base, reasoning_type: 'effort', reasoning_values: ['low', 'high'] }} initial="medium" />);
    assert.equal(text(), '默认'); assert.deepEqual(changes, []);
    await mount(<SliderFixture model={{ ...base, reasoning_type: 'effort', reasoning_values: [] }} />);
    await tap(4); await key('End'); assert.equal(text(), '默认'); assert.deepEqual(changes, []);
  });
  await test('model list uses only supplied enabled models, groups by provider ID, and preserves selection', async () => {
    const models = [base, { ...base, id: 'custom:a/one', name: 'One', provider_id: 'a', provider_name: '服务' }, { ...base, id: 'custom:default/two', name: 'Two', provider_id: 'default', provider_name: '另一个服务' }];
    await mount(<ModelFixture models={models} />); await open();
    assert.equal(trigger().title, '下一轮模型'); assert.equal(trigger().getAttribute('aria-haspopup'), 'listbox');
    assert.equal(container.querySelector('select, datalist, input'), null);
    assert.deepEqual(options().map((option) => option.dataset.modelId), models.map((model) => model.id));
    assert.deepEqual([...container.querySelectorAll('.model-group-label')].map((group) => group.textContent), ['默认服务', '服务', '另一个服务']);
    assert.equal(document.activeElement, options()[0]);
    await key('ArrowDown', options()[0]); assert.equal(document.activeElement, options()[1]);
    await act(async () => { options()[1].click(); });
    assert.equal(trigger().textContent, 'One'); assert.deepEqual(modelChanges, ['custom:a/one']);
    assert.equal(container.querySelector('[role="listbox"]'), null); assert.equal(document.activeElement, trigger());
    await open(); assert.equal(options()[1].getAttribute('aria-selected'), 'true');
    await act(async () => { options()[1].click(); }); assert.equal(modelChanges.length, 1);
  });
  await test('eight models enable case-insensitive local search by name, ID and provider, including empty results', async () => {
    const models = Array.from({ length: 8 }, (_, i) => ({ ...base, id: 'custom:p/model-' + i, name: 'Model ' + i, provider_id: 'p', provider_name: '测试服务' }));
    await mount(<ModelFixture models={models} />); await open();
    const input = container.querySelector<HTMLInputElement>('[aria-label="搜索会话模型"]')!;
    assert.equal(document.activeElement, input);
    await search('MODEL 6'); assert.deepEqual(options().map((option) => option.textContent), ['Model 6']);
    await search('model-3'); assert.equal(options()[0].dataset.modelId, 'custom:p/model-3');
    await search('测试服务'); assert.equal(options().length, 8);
    await search('not-enabled'); assert.equal(options().length, 0); assert.ok(container.textContent?.includes('没有匹配的模型'));
    await key('ArrowDown', input); assert.equal(document.activeElement, input);
    await search('model-4'); await key('ArrowDown', input); assert.equal(document.activeElement, options()[0]);
    await act(async () => { options()[0].click(); }); assert.deepEqual(modelChanges, ['custom:p/model-4']);
    await open(); assert.equal(container.querySelector<HTMLInputElement>('input')!.value, ''); assert.equal(options().length, 8);
  });
  await test('Escape, outside pointer and focus departure close the model list', async () => {
    await mount(<ModelFixture models={[base]} />); await open();
    await key('Escape', document.activeElement!); assert.equal(container.querySelector('[role="listbox"]'), null); assert.equal(document.activeElement, trigger());
    await open();
    await act(async () => { document.getElementById('outside')!.dispatchEvent(new Event('pointerdown', { bubbles: true })); });
    assert.equal(container.querySelector('[role="listbox"]'), null);
    await open(); await act(async () => { document.getElementById('outside')!.focus(); });
    assert.equal(container.querySelector('[role="listbox"]'), null); assert.deepEqual(modelChanges, []);
  });
  await test('model loading, empty and unavailable labels stay consistent and disabled lists cannot open', async () => {
    for (const state of [{ models: [], loading: true, disabled: true, label: '加载中…' }, { models: [], label: '未配置模型' }]) {
      await mount(<ModelFixture {...state} />); assert.equal(trigger().textContent, state.label);
      if (state.disabled) { await open(); assert.equal(container.querySelector('[role="listbox"]'), null); }
    }
    await mount(<ModelPicker models={[base]} onChange={() => {}} />); assert.equal(trigger().textContent, '模型不可用');
    await mount(<ModelFixture models={[base]} />); await open();
    await act(async () => { root!.render(<StrictMode><ModelFixture models={[base]} disabled /></StrictMode>); });
    assert.equal(trigger().disabled, true); assert.equal(trigger().getAttribute('aria-expanded'), 'false');
    assert.equal(container.querySelector('[role="listbox"]'), null);
  });

  await test('the effort pill starts closed, opens on toggle, and closes on Escape or outside pointer', async () => {
    await mount(<EffortPickerFixture models={[base]} />);
    assert.equal(effortTrigger().textContent, 'Default · 默认');
    assert.equal(effortTrigger().getAttribute('aria-expanded'), 'false');
    assert.equal(document.querySelector('.reasoning-popover'), null);
    await openEffort();
    assert.equal(effortTrigger().textContent, 'Default · 默认');
    assert.equal(effortTrigger().getAttribute('aria-expanded'), 'true');
    assert.ok(document.querySelector('.reasoning-popover'));
    await openEffort(); assert.equal(document.querySelector('.reasoning-popover'), null);
    await openEffort();
    await key('Escape', document.activeElement!);
    assert.equal(document.querySelector('.reasoning-popover'), null);
    await openEffort();
    await act(async () => { document.getElementById('outside')!.dispatchEvent(new Event('pointerdown', { bubbles: true })); });
    assert.equal(document.querySelector('.reasoning-popover'), null);
  });

  await test('the trigger reflects the active model and effort before and after a reset', async () => {
    await mount(<EffortPickerFixture models={[base]} />);
    await openEffort(); await key('ArrowRight', effortSlider());
    assert.equal(effortSlider().getAttribute('aria-valuetext'), '关');
    await openEffort();
    assert.equal(effortTrigger().textContent, 'Default · 关');
    await openEffort(); assert.equal(effortTrigger().textContent, 'Default · 关');
    await act(async () => { document.querySelector<HTMLButtonElement>('[aria-label="重置思考强度"]')!.click(); });
    assert.equal(effortSlider().getAttribute('aria-valuetext'), '默认');
    await openEffort(); assert.equal(effortTrigger().textContent, 'Default · 默认');
  });

  await test('Agnes uses off/on, treats legacy levels as on, and reset stores the official on default', async () => {
    const agnes = { ...base, id: 'agnes', name: 'Agnes', reasoning_type: 'toggle' as const };
    for (const old of ['low', 'medium', 'high'] as const) {
      await mount(<EffortPickerFixture models={[agnes]} initial={old} />);
      await openEffort();
      assert.equal(effortSlider().getAttribute('aria-valuetext'), '开');
      assert.deepEqual([...document.querySelectorAll('.reasoning-notch')].map((item) => item.getAttribute('data-effort')), ['off', 'default']);
      await act(async () => { document.querySelector<HTMLButtonElement>('[aria-label="重置思考强度"]')!.click(); });
      assert.deepEqual(changes, ['default']);
      assert.equal(effortSlider().getAttribute('aria-valuetext'), '开');
      await openEffort(); assert.equal(effortTrigger().textContent, 'Agnes · 开');
    }
  });

  await test('the model sub-view groups and searches enabled models, then refreshes slider capability', async () => {
    const models = Array.from({ length: 8 }, (_, i) => ({
      ...base,
      id: 'custom:p/model-' + i,
      name: 'Model ' + i,
      provider_id: i < 4 ? 'p' : 'q',
      provider_name: i < 4 ? '提供方 P' : '提供方 Q',
      ...(i === 7 ? { reasoning_type: 'toggle' as const } : {}),
    }));
    await mount(<EffortPickerFixture models={models} />);
    await openEffort();
    await act(async () => { document.querySelector<HTMLButtonElement>('.reasoning-model-link')!.click(); });
    assert.equal(document.querySelector('.reasoning-model-heading strong')?.textContent, '选择模型');
    assert.deepEqual([...document.querySelectorAll('.reasoning-popover .model-group-label')].map((item) => item.textContent), ['提供方 P', '提供方 Q']);
    await searchGlobal('MODEL 7');
    assert.deepEqual([...document.querySelectorAll('.reasoning-popover [role="option"]')].map((option) => option.textContent?.trim()), ['Model 7']);
    assert.equal(document.querySelector<HTMLInputElement>('.reasoning-popover [role="option"]')?.getAttribute('aria-selected'), 'false');
    await act(async () => { document.querySelector<HTMLButtonElement>('.reasoning-popover [role="option"]')!.click(); });
    assert.deepEqual(modelChanges, ['custom:p/model-7']);
    assert.equal(effortSlider().getAttribute('aria-valuemax'), '1');
    assert.equal(effortSlider().getAttribute('aria-valuetext'), '开');
    assert.equal(document.activeElement, document.querySelector('.reasoning-model-link'));
    await openEffort(); assert.equal(effortTrigger().textContent, 'Model 7 · 开');
  });

  await test('Escape and outside clicks from the model sub-view close the whole popover', async () => {
    await mount(<EffortPickerFixture models={[base]} />);
    await openEffort();
    await act(async () => { document.querySelector<HTMLButtonElement>('.reasoning-model-link')!.click(); });
    await key('Escape', document.activeElement!);
    assert.equal(document.querySelector('.reasoning-popover'), null);
    await openEffort(); await act(async () => { document.querySelector<HTMLButtonElement>('.reasoning-model-link')!.click(); });
    await act(async () => { document.getElementById('outside')!.dispatchEvent(new Event('pointerdown', { bubbles: true })); });
    assert.equal(document.querySelector('.reasoning-popover'), null);
  });
} finally {
  if (root) await act(async () => { root!.unmount(); });
  dom.window.close();
}
