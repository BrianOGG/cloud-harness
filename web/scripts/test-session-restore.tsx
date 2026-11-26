import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, StrictMode } from 'react';
import type { Root } from 'react-dom/client';

// Mount the actual App and effects, without a browser or the acceptance DB.
const dom = new JSDOM('<!doctype html><div id="root"></div>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event', 'MouseEvent']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
Object.assign(window, { matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }) });
dom.window.HTMLElement.prototype.scrollIntoView = () => {};
const { createRoot } = await import('react-dom/client');
const { flushSync } = await import('react-dom');
const { default: App } = await import('../src/App');

const key = 'cloud-harness.web-session';
const id = 'ses_saved';
const session = { id, created_at: '2026-11-26T01:00:00Z', updated_at: '2026-11-26T01:00:00Z', run_count: 0 };
const message = { id: 'msg_saved', session_id: id, run_id: null, turn_id: null, role: 'assistant', kind: 'response', status: 'recorded', content: '重新挂载后自动加载的历史', event: '', created_at: session.created_at };
const container = document.getElementById('root')!;
let root: Root | undefined;
const seen: string[] = [];
let runsStatus = 200;
let messagesStatus = 200;
let runError = 'record not found';
let releaseSessions: (() => void) | undefined;
let holdSessions: Promise<void> | undefined;

globalThis.fetch = async (input) => {
  const path = String(input);
  seen.push(path);
  let status = 200;
  let body: unknown;
  if (path === '/healthz') body = { status: 'ok', provider: 'fixture' };
  else if (path === '/v1/approvals' || path === '/v1/providers') body = [];
  else if (path === '/v1/models') body = [{ id: 'default', name: '默认模型', model: 'fixture', provider: 'fixture' }];
  else if (path.endsWith('/preferences')) body = { model_id: 'default', approval_mode: 'ask' };
  else if (path === '/v1/sessions') { await holdSessions; body = [session]; }
  else if (path.endsWith('/runs')) { status = runsStatus; body = status === 200 ? [] : { error: { message: runError } }; }
  else if (path.includes('/messages')) { status = messagesStatus; body = status === 200 ? { messages: [message], next_cursor: '' } : { error: { message: 'record not found' } }; }
  else throw new Error('Unexpected request: ' + path);
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
};

async function mount() {
  root = createRoot(container);
  await act(async () => { root!.render(<StrictMode><App /></StrictMode>); });
}
async function unmount() {
  if (root) await act(async () => { root!.unmount(); root = undefined; });
}
async function reset() {
  await unmount();
  localStorage.clear();
  seen.length = 0;
  runsStatus = messagesStatus = 200;
  runError = 'record not found';
  holdSessions = undefined;
  releaseSessions = undefined;
}

try {
  await test('App saves the selected session during the click, before a refresh can outrun effects', async () => {
    await reset();
    await mount();
    await act(async () => {
      flushSync(() => {
        container.querySelector<HTMLButtonElement>('[data-session-id="ses_saved"]')!.click();
        assert.equal(localStorage.getItem(key), id, 'selection must be durable before passive effects run');
      });
    });
    await unmount();
    seen.length = 0;
    holdSessions = new Promise<void>((resolve) => { releaseSessions = resolve; });
    await mount();
    assert.ok(container.textContent?.includes(message.content), 'history loads before the sidebar list completes');
    assert.ok(seen.includes('/v1/sessions/ses_saved/runs'));
    assert.ok(seen.some((path) => path.startsWith('/v1/sessions/ses_saved/messages')));
    assert.equal(container.querySelector('.welcome'), null);
    await act(async () => { releaseSessions!(); });
    assert.ok(container.querySelector('[data-session-id="ses_saved"]')?.classList.contains('current'));
    assert.ok(container.textContent?.includes(message.content));
    await unmount();
  });

  await test('App retains a restored session on a non-404 failure containing not found or 404', async () => {
    await reset();
    localStorage.setItem(key, id);
    runsStatus = 503;
    runError = 'upstream metadata not found (ref 404)';
    await mount();
    assert.equal(localStorage.getItem(key), id);
    assert.ok(container.querySelector('[data-session-id="ses_saved"]')?.classList.contains('current'));
    assert.ok(container.textContent?.includes(message.content));
    assert.ok(container.querySelector('[role="alert"]')?.textContent?.includes(runError));
    await unmount();
  });

  for (const route of ['runs', 'messages']) {
    await test('App cleanly forgets a removed saved session when ' + route + ' returns 404', async () => {
      await reset();
      localStorage.setItem(key, id);
      if (route === 'runs') runsStatus = 404;
      else messagesStatus = 404;
      await mount();
      assert.equal(localStorage.getItem(key), null);
      assert.ok(container.querySelector('.welcome'));
      assert.equal(container.querySelector('[role="alert"]'), null);
      assert.equal(container.querySelector('.history-message'), null);
      assert.equal(container.querySelector('.session-item.current'), null);
      await unmount();
    });
  }
} finally {
  releaseSessions?.();
  await unmount();
  dom.window.close();
}
