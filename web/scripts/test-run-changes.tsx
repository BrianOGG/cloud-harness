import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act } from 'react';
import { RunChanges } from '../src/RunChanges';

const dom = new JSDOM('<div id="root"></div>', { url: 'http://fixture.invalid/ui' });
Object.assign(globalThis, { window: dom.window, document: dom.window.document, localStorage: dom.window.localStorage, IS_REACT_ACT_ENVIRONMENT: true });
const { createRoot } = await import('react-dom/client');

await test('review, cancel, handle a stale restore, refresh, and confirm the selected file', async () => {
  const selection = { before: 'snap_pre', after: 'snap_post', path: 'a &[1].txt' };
  const container = document.getElementById('root')!;
  const root = createRoot(container);
  let posts = 0;
  let reads = 0;
  globalThis.fetch = async (input, init) => {
    const url = new URL(String(input), dom.window.location.origin);
    if (url.pathname === '/v1/runs/run_1/changes/diff') {
      assert.deepEqual(Object.fromEntries(url.searchParams), selection);
      return Response.json({ diff: '-before\n+after', truncated: false });
    }
    if (url.pathname === '/v1/runs/run_1/changes/restore') {
      assert.equal(init?.method, 'POST');
      assert.deepEqual(JSON.parse(String(init?.body)), { ...selection, confirm: true });
      posts++;
      if (posts === 1) return Response.json({ error: { message: '文件已改变' } }, { status: 409 });
      return Response.json({ safety_snapshot: 'snap_safety' });
    }
    assert.equal(url.pathname, '/v1/runs/run_1/changes');
    reads++;
    return Response.json({ terminal: true, incomplete: false, batches: [{ ...selection, number: 1, files: [{ path: selection.path, status: 'M' }] }] });
  };
  async function click(label: string) {
    const button = [...container.querySelectorAll('button')].find(el => el.textContent?.includes(label));
    assert.ok(button, label);
    await act(async () => button.click());
  }
  try {
    await act(async () => root.render(<RunChanges runId="run_1" />));
    await click(selection.path);
    assert.match(container.querySelector('pre')!.textContent!, /-before\n\+after/);
    await click('还原此文件');
    await click('取消');
    assert.equal(posts, 0);
    await click('还原此文件');
    await click('确认还原');
    assert.equal(container.querySelector('[role=alert]')?.textContent, '文件已改变');
    await click('刷新');
    assert.equal(container.querySelector('pre'), null);
    await click(selection.path);
    await click('还原此文件');
    await click('确认还原');
    assert.equal(posts, 2);
    assert.equal(reads, 3);
    assert.match(container.querySelector('[role=status]')!.textContent!, /snap_safety/);
  } finally {
    await act(async () => root.unmount());
    dom.window.close();
  }
});
