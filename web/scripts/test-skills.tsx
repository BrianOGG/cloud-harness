import assert from 'node:assert/strict';
import { afterEach, test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act } from 'react';
import type { Root } from 'react-dom/client';

const dom = new JSDOM('<div id="root"></div><button id="outside">外部</button>', { url: 'http://fixture.invalid/ui/', pretendToBeVisual: true });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
Object.assign(window, { matchMedia: () => ({ matches: true, addEventListener() {}, removeEventListener() {} }) });
const { createRoot } = await import('react-dom/client');
const { default: App } = await import('../src/App');
const skills = [
  { name: 'build', description: '构建项目' },
  { name: 'Review-UI', description: '检查界面' },
  { name: 'review_api', description: '检查接口' },
];
let root: Root;
const input = () => document.querySelector('textarea')!;
const options = () => [...document.querySelectorAll<HTMLButtonElement>('.skill-popover [role="option"]')];
const optionNames = () => options().map(row => row.querySelector('strong')?.textContent);

async function mount({ catalog = () => Response.json(skills), existing = false, rejectRun = false, waiting = false }:
  { catalog?: () => Response | Promise<Response>; existing?: boolean; rejectRun?: boolean; waiting?: boolean } = {}) {
  const posts: { path: string; body: Record<string, string> }[] = [];
  const catalogRequests: RequestInit[] = [];
  const now = new Date().toISOString();
  const prefs = { model_id: 'default', approval_mode: 'safe' };
  const run = { id: 'run_1', session_id: 'ses_one', status: 'COMPLETED', final_answer: '完成', version: 1, budget: {}, created_at: now };
  let hasSession = existing;
  if (existing) localStorage.setItem('cloud-harness.web-session', 'ses_one');
  localStorage.setItem('cloud-harness.http-token', 'fixture');
  globalThis.fetch = async (url, init) => {
    const path = String(url);
    const body = JSON.parse(String(init?.body || '{}'));
    if (init?.method === 'POST') posts.push({ path, body });
    if (path === '/healthz') return Response.json({ status: 'ok' });
    if (path === '/v1/models') return Response.json([{ id: 'default', name: 'Test', model: 'test' }]);
    if (path === '/v1/skills') {
      catalogRequests.push(init || {});
      return catalog();
    }
    if (path === '/v1/sessions') {
      if (init?.method === 'POST') {
        hasSession = true;
        return Response.json({ id: 'ses_one' });
      }
      return Response.json(hasSession ? [{ id: 'ses_one', run_count: 0, created_at: now, updated_at: now }] : []);
    }
    if (path.endsWith('/preferences')) return Response.json(init?.method === 'PUT' ? body : prefs);
    if (path.includes('/messages')) return Response.json({ messages: [], next_cursor: '' });
    if (path.endsWith('/runs')) return Response.json(waiting ? [{ ...run, status: 'WAITING', wait_reason: 'USER', question: '继续？' }] : []);
    if (path.includes('/events')) {
      return new Promise<Response>((_, reject) => init?.signal?.addEventListener('abort', () => reject(new Error('aborted')), { once: true }));
    }
    if (path.endsWith('/responses?wait=0')) return Response.json(run);
    if (path.endsWith('/runs?wait=0')) {
      if (rejectRun) return Response.json({ error: { message: '无法加载所选技能' } }, { status: 400 });
      return Response.json(run);
    }
    throw new Error('Unexpected request ' + path);
  };
  root = createRoot(document.getElementById('root')!);
  await act(async () => root.render(<App />));
  assert.equal(document.activeElement, input());
  assert.equal(catalogRequests.length, 0);
  return { posts, catalogRequests };
}

async function type(value: string) {
  await act(async () => {
    Object.getOwnPropertyDescriptor(dom.window.HTMLTextAreaElement.prototype, 'value')!.set!.call(input(), value);
    input().dispatchEvent(new Event('input', { bubbles: true }));
  });
}

async function key(key: string, init: KeyboardEventInit = {}) {
  const event = new dom.window.KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...init });
  await act(async () => { input().dispatchEvent(event); });
  return event.defaultPrevented;
}

afterEach(async () => {
  await act(async () => root.unmount());
  localStorage.clear();
});

test('new session discovers only on slash, filters summaries and sends the chosen skill with its task', async () => {
  const { posts, catalogRequests } = await mount();
  await type('/');
  assert.deepEqual(optionNames(), skills.map(skill => skill.name));
  assert.match(options()[1].textContent || '', /检查界面/);
  assert.equal(new Headers(catalogRequests[0].headers).get('Authorization'), 'Bearer fixture');
  await type('/ReV');
  assert.deepEqual(optionNames(), ['Review-UI', 'review_api']);
  assert.equal(catalogRequests.length, 1);
  assert.equal(await key('Enter', { isComposing: true }), false);
  assert.equal(await key('Enter', { shiftKey: true }), false);
  assert.equal(input().value, '/ReV');
  await key('ArrowDown');
  assert.equal(input().getAttribute('aria-activedescendant'), options()[1].id);
  await key('Enter');
  assert.equal(input().value, '/skill review_api ');
  await key('Enter', { repeat: true });
  assert.equal(document.activeElement, input());
  assert.equal(document.querySelector('.skill-popover'), null);
  assert.deepEqual(posts, []);
  const task = '检查参数\n  保留缩进';
  await type(input().value + task);
  await key('Enter');
  assert.deepEqual(posts, [
    { path: '/v1/sessions', body: {} },
    { path: '/v1/sessions/ses_one/runs?wait=0', body: { message: task, skill: 'review_api', model_id: 'default', approval_mode: 'ask' } },
  ]);
});

test('existing session supports mouse, Tab, dismissing and continuing ordinary drafts', async () => {
  const { posts } = await mount({ existing: true });
  assert.ok(input().closest('.dock'));
  await type('/');
  await act(async () => options()[1].click());
  assert.equal(input().value, '/skill Review-UI ');
  assert.equal(document.activeElement, input());
  await type('/skill b');
  await key('Tab');
  assert.equal(input().value, '/skill build ');
  await type('/review');
  await key('Escape');
  assert.equal(document.querySelector('.skill-popover'), null);
  assert.equal(input().value, '/review');
  await type('/rev');
  await act(async () => document.getElementById('outside')!.focus());
  assert.equal(document.querySelector('.skill-popover'), null);
  await act(async () => input().focus());
  assert.equal(options().length, 2);
  await type('/检查界面');
  assert.equal(document.querySelector('.skill-popover'), null);
  await type('/no-match');
  assert.match(document.querySelector('.skill-popover')!.textContent || '', /没有匹配/);
  await type('/tmp/readme 普通消息');
  assert.deepEqual(posts, []);
  await key('Enter');
  assert.deepEqual(posts[0].body, { message: '/tmp/readme 普通消息', model_id: 'default', approval_mode: 'safe' });
});

test('catalog failure, empty catalog and stale requests leave the draft editable', async () => {
  let release!: (result: Response) => void;
  let attempts = 0;
  const { posts } = await mount({ catalog: () => {
    if (++attempts === 1) return new Promise<Response>(resolve => { release = resolve; });
    if (attempts === 2) return Response.json({}, { status: 503 });
    return Response.json([]);
  } });
  await type('/');
  assert.match(document.querySelector('.skill-popover')!.textContent || '', /正在加载/);
  await key('Enter');
  await key('Escape');
  await type('/b');
  assert.match(document.querySelector('.skill-popover')!.textContent || '', /暂不可用/);
  await act(async () => release(Response.json(skills)));
  assert.equal(options().length, 0);
  await type('');
  await type('/');
  assert.match(document.querySelector('.skill-popover')!.textContent || '', /暂无技能/);
  assert.equal(input().value, '/');
  assert.deepEqual(posts, []);
});

test('an explicit skill without arguments sends on click and keeps the command if loading fails', async () => {
  const { posts } = await mount({ existing: true, rejectRun: true });
  await type('/rev');
  await key('ArrowUp');
  await key('Tab');
  assert.deepEqual(posts, []);
  await act(async () => document.querySelector<HTMLButtonElement>('button[aria-label="发送"]')!.click());
  assert.equal(posts[0].body.skill, 'review_api');
  assert.equal(posts[0].body.message, '');
  assert.equal(input().value, '/skill review_api');
  assert.match(document.querySelector('[role="alert"]')!.textContent || '', /无法加载所选技能/);
});

test('a waiting run receives slash text as a literal answer', async () => {
  const { posts, catalogRequests } = await mount({ existing: true, waiting: true });
  assert.equal(input().placeholder, '回答问题…');
  await type('/');
  assert.equal(catalogRequests.length, 0);
  assert.equal(document.querySelector('.skill-popover'), null);
  await type('/skill Review-UI 示例');
  await key('Enter');
  assert.deepEqual(posts, [{ path: '/v1/runs/run_1/responses?wait=0', body: { answer: '/skill Review-UI 示例' } }]);
});
