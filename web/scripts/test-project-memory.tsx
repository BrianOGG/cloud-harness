import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act } from 'react';
import type { ProjectMemory } from '../src/projectMemoryApi';

const dom = new JSDOM('<div id="root"></div>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
const { createRoot } = await import('react-dom/client');
const { ConfirmMemory, ProjectMemories } = await import('../src/ProjectMemory');

await test('saving a conversation memory is explicit and its source survives a correction', async () => {
  const container = document.getElementById('root')!;
  const root = createRoot(container);
  const click = async (label: string) => {
    const button = [...container.querySelectorAll('button')].find(b => b.textContent?.startsWith(label));
    assert.ok(button && !button.disabled, label);
    await act(async () => button.click());
  };
  const content = async (value: string) => {
    const field = container.querySelector('textarea')!;
    await act(async () => {
      Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value')!.set!.call(field, value);
      field.dispatchEvent(new Event('input', { bubbles: true }));
    });
  };
  let saved: ProjectMemory | undefined;
  const calls: string[] = [];
  globalThis.fetch = async (input, init) => {
    const method = init?.method || 'GET';
    calls.push(method);
    assert.equal(String(input), '/v1/project-memories' + (['PUT', 'DELETE'].includes(method) ? '/mem' : ''));
    if (method === 'GET') return Response.json(saved ? [saved] : []);
    const body = init?.body && JSON.parse(String(init.body));
    if (method === 'POST') {
      assert.deepEqual(body, { kind: 'fact', content: 'manual fact', source_session_id: 'session', source_run_id: 'run' });
      saved = { id: 'mem', ...body, source_available: true, updated_at: '2026-01-01T00:00:00Z' };
      return Response.json(saved, { status: 201 });
    }
    if (method === 'PUT') {
      assert.deepEqual(body, { kind: 'fact', content: 'corrected fact' });
      saved = { ...saved, ...body };
    } else if (method === 'DELETE') saved = undefined;
    else assert.fail(method);
    return new Response(null, { status: 204 });
  };
  try {
    await act(async () => root.render(<><article><div className="turn-final">selected passage · private fixture</div>
      <ConfirmMemory sessionId="session" runId="run" /></article><ProjectMemories /></>));
    await click('存为项目记忆');
    assert.equal(container.querySelector('textarea')!.value, '');
    await click('取消');
    const text = container.querySelector('.turn-final')!.firstChild!;
    const range = document.createRange();
    range.setStart(text, 0);
    range.setEnd(text, 'selected passage'.length);
    window.getSelection()!.removeAllRanges();
    window.getSelection()!.addRange(range);
    assert.equal(window.getSelection()!.toString(), 'selected passage');
    await click('存为项目记忆');
    assert.equal(container.querySelector('textarea')!.value, 'selected passage');
    await content('manual fact');
    assert.deepEqual(calls, []);
    await click('确认保存');
    await click('项目记忆');
    const link = () => new URL(container.querySelector('a')!.href);
    assert.equal(link().searchParams.get('run'), 'run');
    assert.equal(link().searchParams.get('session'), 'session');
    await click('修改');
    await content('corrected fact');
    await click('确认保存');
    assert.equal(container.querySelector('.memory-text')!.textContent, 'corrected fact');
    assert.equal(link().searchParams.get('run'), 'run');
    await click('删除');
    await click('确认删除');
    assert.deepEqual(calls, ['POST', 'GET', 'PUT', 'DELETE']);
    assert.ok(container.textContent?.includes('还没有项目记忆'));
  } finally {
    await act(async () => root.unmount());
    dom.window.close();
  }
});
