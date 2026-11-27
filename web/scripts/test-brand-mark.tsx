import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, StrictMode } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import type { Root } from 'react-dom/client';
import { BrandMark } from '../src/BrandMark';

const dom = new JSDOM('<!doctype html><div id="root"></div>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event', 'MouseEvent']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
Object.assign(window, { matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }) });
dom.window.HTMLElement.prototype.scrollIntoView = () => {};
const { createRoot } = await import('react-dom/client');
const { default: App } = await import('../src/App');
const { api } = await import('../src/api');
api.streamEvents = async (_id, _after, _cb, signal) => {
  if (!signal.aborted) await new Promise<void>((resolve) => signal.addEventListener('abort', () => resolve(), { once: true }));
};

const session = { id: 'ses_brand', created_at: '2026-11-27T00:00:00Z', updated_at: '2026-11-27T00:00:00Z', run_count: 0 };
const container = document.getElementById('root')!;
let root: Root | undefined;
let waitingForAnswer = false;
globalThis.fetch = async (input) => {
  const path = String(input);
  let body: unknown;
  if (path === '/healthz') body = { status: 'ok', provider: 'fixture' };
  else if (path === '/v1/models') body = [{ id: 'default', name: 'Default', model: 'fixture', provider: 'fixture' }];
  else if (path === '/v1/approvals' || path === '/v1/providers') body = [];
  else if (path === '/v1/sessions') body = [session];
  else if (path.endsWith('/preferences')) body = { model_id: 'default', approval_mode: 'ask' };
  else if (path.includes('/messages')) body = { messages: [], next_cursor: '' };
  else if (path.endsWith('/runs')) body = waitingForAnswer ? [{ id: 'run_question', session_id: session.id, turn_id: 'turn_question', status: 'WAITING', wait_reason: 'USER', version: 1, budget: {} }] : [];
  else throw new Error('Unexpected request: ' + path);
  return new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
};
async function unmount() {
  if (root) await act(async () => { root!.unmount(); root = undefined; });
}
async function mount({ closed = false, answering = false } = {}) {
  await unmount();
  localStorage.clear();
  window.history.replaceState(null, '', '/ui/');
  if (closed) localStorage.setItem('cloud-harness.web-sidebar', 'closed');
  if (answering) localStorage.setItem('cloud-harness.web-session', session.id);
  waitingForAnswer = answering;
  root = createRoot(container);
  await act(async () => { root!.render(<StrictMode><App /></StrictMode>); });
}
async function click(selector: string) {
  const button = container.querySelector<HTMLButtonElement>(selector);
  assert.ok(button, 'Missing button: ' + selector);
  await act(async () => { button.click(); });
}
function checkMark(mark: Element | null, size: number) {
  assert.ok(mark);
  assert.equal(mark.tagName.toLowerCase(), 'svg');
  assert.equal(mark.getAttribute('viewBox'), '0 0 32 32');
  assert.equal(mark.getAttribute('width'), String(size));
  assert.equal(mark.getAttribute('height'), String(size));
  assert.equal(mark.getAttribute('aria-hidden'), 'true');
  assert.equal(mark.getAttribute('focusable'), 'false');
  assert.equal(mark.querySelectorAll('path').length, 1);
  assert.equal(mark.querySelector('path')!.getAttribute('fill'), 'currentColor');
  assert.equal(mark.querySelector('path')!.getAttribute('fill-rule'), 'evenodd');
  assert.equal(mark.querySelector('mask, [id], image'), null);
}

try {
  await test('reusable marks share the favicon geometry, inherit color, and need no IDs', () => {
    const marks = new JSDOM(renderToStaticMarkup(<><BrandMark /><BrandMark large /></>));
    const favicon = new JSDOM(readFileSync(new URL('../public/favicon.svg', import.meta.url), 'utf8'), { contentType: 'image/svg+xml' });
    try {
      const [small, large] = marks.window.document.querySelectorAll('svg');
      checkMark(small, 20); checkMark(large, 40);
      assert.equal(large.classList.contains('large'), true);
      const path = favicon.window.document.querySelector('path')!;
      assert.equal(path.getAttribute('fill'), '#a3adff');
      assert.equal(path.getAttribute('fill-rule'), 'evenodd');
      assert.equal(small.querySelector('path')!.getAttribute('d'), path.getAttribute('d'));
      assert.equal(large.querySelector('path')!.getAttribute('d'), path.getAttribute('d'));
      assert.equal(favicon.window.document.querySelector('mask, [id], rect, image'), null);
    } finally { marks.window.close(); favicon.window.close(); }
  });

  await test('sidebar brand keeps its label and new-conversation action; welcome uses the large mark', async () => {
    await mount();
    const brand = container.querySelector<HTMLButtonElement>('button.brand')!;
    assert.equal(brand.textContent, 'Cloud Harness');
    checkMark(brand.querySelector('svg.brand-mark'), 20);
    checkMark(container.querySelector('.welcome-inner > svg.brand-mark.large'), 40);
    assert.equal(container.querySelector('.welcome h2')?.textContent, '今天想做什么？');
    assert.equal(container.querySelector('.topbar > .brand-mark'), null);
    await click('[data-session-id="ses_brand"]');
    assert.equal(localStorage.getItem('cloud-harness.web-session'), session.id);
    assert.equal(container.querySelector('.welcome'), null);
    await click('button.brand');
    assert.equal(localStorage.getItem('cloud-harness.web-session'), null);
    checkMark(container.querySelector('.welcome-inner > svg.brand-mark.large'), 40);
  });

  await test('collapsed sidebar places one quiet mark after the actions and before the title', async () => {
    await mount();
    await click('[aria-label="收起侧栏"]');
    const topbar = container.querySelector('.topbar')!;
    assert.deepEqual([...topbar.children].map((child) => child.tagName.toLowerCase()), ['button', 'button', 'svg', 'h1']);
    assert.deepEqual([...topbar.querySelectorAll('button')].map((button) => button.getAttribute('aria-label')), ['展开侧栏', '新对话']);
    checkMark(topbar.querySelector('.brand-mark'), 20);
    assert.equal(topbar.querySelectorAll('.brand-mark').length, 1);
    assert.equal(topbar.textContent, '新对话');
    await click('[aria-label="展开侧栏"]');
    assert.equal(topbar.querySelector('.brand-mark'), null);
    await mount({ closed: true });
    checkMark(container.querySelector('.topbar > .brand-mark'), 20);
  });

  await test('new and existing conversations use the short message placeholder', async () => {
    await mount();
    assert.equal(container.querySelector('textarea')?.placeholder, '发消息…');
    await click('[data-session-id="ses_brand"]');
    assert.equal(container.querySelector('textarea')?.placeholder, '发消息…');
    assert.equal(container.innerHTML.includes('给 Cloud Harness 发消息'), false);
  });

  await test('answering a question retains its existing placeholder', async () => {
    await mount({ answering: true });
    assert.equal(container.querySelector('textarea')?.placeholder, '回答问题…');
  });
} finally {
  await unmount();
  dom.window.close();
}
