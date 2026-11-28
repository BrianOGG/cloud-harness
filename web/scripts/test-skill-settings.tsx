import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act, useRef, useState } from 'react';
import { editDocument, newDocument } from '../src/skillDocument';
import type { SkillDocument } from '../src/skillSettingsApi';

const dom = new JSDOM('<main id="root"></main>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'localStorage', 'HTMLElement', 'Event']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
const { createRoot } = await import('react-dom/client');
const { SkillSettings } = await import('../src/SkillSettings');
const { SkillInput } = await import('../src/SkillInput');

function Composer() {
  const [value, setValue] = useState('/');
  const inputRef = useRef<HTMLTextAreaElement>(null);
  return <SkillInput value={value} onChange={setValue} inputRef={inputRef} placeholder="草稿" onKeyDown={() => {}} />;
}

const imported = "---\r\nx-author: local\r\nname: review\r\n description : 'Review: API'\r\n---\r\n# Review\r\n\r\nextra notes\r\n";

test('document edits preserve unknown fields, body bytes and CRLF', () => {
  assert.equal(editDocument(imported, 'Review: API', 'Updated', '# Review\r\n\r\nextra notes\r\n'),
    imported.replace("'Review: API'", 'Updated'));
  assert.equal(editDocument(imported, 'Review: API', 'Review: API', 'line one\nline two\n'),
    imported.split('# Review')[0] + 'line one\r\nline two\r\n');
  assert.equal(editDocument('---\nname: empty\ndescription: desc\n---', 'desc', 'desc', 'instructions'),
    '---\nname: empty\ndescription: desc\n---\ninstructions');
});

test('author, preview/import, edit and confirmed delete use scoped revisions and refresh the picker', async () => {
  const container = document.getElementById('root')!;
  const root = createRoot(container);
  const docs = new Map<string, SkillDocument>();
  const writes: { method: string; path: string; body: Record<string, string> }[] = [];
  const base = '/v1/skill-definitions';
  let failList = true;
  let failEdit = true;
  let revision = 0;
  let pickerReads = 0;
  localStorage.setItem('cloud-harness.http-token', 'test-token');
  globalThis.fetch = async (url, init) => {
    const path = String(url);
    assert.equal(new Headers(init?.headers).get('Authorization'), 'Bearer test-token');
    const method = init?.method || 'GET';
    const rows = [...docs.values()].map(({ name, scope, description }) => ({ name, scope, description,
      effective: scope === 'project' || !docs.has(`${base}/project/${name}`) }));
    if (path === '/v1/skills') {
      pickerReads++;
      return Response.json(rows.filter(row => row.effective));
    }
    if (path === base && failList) {
      failList = false;
      return Response.json({ error: { message: '读取失败' } }, { status: 503 });
    }
    if (path === base) return Response.json(rows);
    if (method === 'GET') return Response.json(docs.get(path));
    const body = JSON.parse(String(init?.body));
    writes.push({ method, path, body });
    if (method === 'DELETE') {
      assert.equal(body.revision, docs.get(path)?.revision);
      docs.delete(path);
      return new Response(null, { status: 204 });
    }
    if (method === 'PUT' && failEdit) {
      failEdit = false;
      return Response.json({ error: { message: '请重新打开后再试' } }, { status: 409 });
    }
    if (method === 'PUT') assert.equal(body.revision, docs.get(path)?.revision);
    const scope = path.split('/')[3] as SkillDocument['scope'];
    const name = /^name: (.+)$/m.exec(body.content)![1];
    const description = /description\s*:\s*(.+)$/m.exec(body.content)![1].replace(/^'|'$/g, '');
    const doc = { name, description, scope, content: body.content, truncated: false, revision: String(++revision) };
    docs.set(`${base}/${scope}/${name}`, doc);
    return Response.json(doc, { status: method === 'POST' ? 201 : 200 });
  };
  const click = async (label: string) => {
    const button = [...container.querySelectorAll('button')].find(b => b.textContent === label || b.getAttribute('aria-label') === label);
    assert.ok(button && !button.disabled, label);
    await act(async () => button.click());
  };
  const field = (label: string) => {
    const element = [...container.querySelectorAll('label')].find(node => node.textContent?.startsWith(label));
    assert.ok(element, label);
    return element.querySelector('input, select, textarea') as HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement;
  };
  const type = async (label: string, value: string) => {
    const input = field(label);
    await act(async () => {
      Object.getOwnPropertyDescriptor(Object.getPrototypeOf(input), 'value')!.set!.call(input, value);
      input.dispatchEvent(new Event(input.tagName === 'SELECT' ? 'change' : 'input', { bubbles: true }));
    });
  };
  try {
    await act(async () => root.render(<><Composer /><SkillSettings /></>));
    assert.match(container.textContent!, /读取失败/);
    await click('刷新');
    assert.match(container.textContent!, /暂无技能/);
    const draft = container.querySelector<HTMLTextAreaElement>('[aria-label="草稿"]')!;
    await act(async () => draft.focus());
    assert.equal(pickerReads, 1);
    await click('新建技能');
    await type('作用域', 'global');
    await type('名称', 'review');
    await type('简介', 'global review');
    await type('Markdown 指令', 'instructions');
    await click('保存');
    assert.deepEqual(writes.at(-1), { method: 'POST', path: base + '/global', body: { content: newDocument('review', 'global review', 'instructions') } });
    assert.match(container.querySelector('[role="option"]')!.textContent!, /global review/);
    const fileInput = container.querySelector<HTMLInputElement>('input[type="file"]')!;
    Object.defineProperty(fileInput, 'files', { configurable: true, value: [new File([imported], 'SKILL.md')] });
    await act(async () => fileInput.dispatchEvent(new Event('change', { bubbles: true })));
    assert.equal(writes.length, 1, 'preview must not publish');
    assert.match(field('文件内容').value, /x-author: local/);
    await click('保存');
    assert.deepEqual(writes.at(-1), { method: 'POST', path: base + '/project', body: { content: imported } });
    assert.match(container.textContent!, /全局 · 被项目覆盖/);
    assert.match(container.querySelector('[role="option"]')!.textContent!, /Review: API/);
    await click('打开项目技能 review');
    assert.ok(field('作用域').disabled);
    assert.equal(field('名称').getAttribute('readonly'), '');
    await type('简介', 'updated');
    await click('保存');
    assert.match(container.textContent!, /请重新打开后再试/);
    assert.equal(field('简介').value, 'updated');
    await click('返回列表');
    await click('打开项目技能 review');
    await type('简介', 'updated');
    await click('保存');
    assert.equal(writes.at(-1)!.body.content, imported.replace("'Review: API'", 'updated'));
    assert.match(container.querySelector('[role="option"]')!.textContent!, /updated/);
    await click('打开项目技能 review');
    await click('删除');
    await click('取消删除');
    assert.ok(!writes.some(w => w.method === 'DELETE'));
    await click('删除');
    await click('确认删除');
    assert.equal(writes.at(-1)!.path, base + '/project/review');
    assert.equal(docs.size, 1);
    assert.match(container.querySelector('[role="option"]')!.textContent!, /global review/);
    await act(async () => { draft.blur(); draft.focus(); });
    const option = container.querySelector<HTMLButtonElement>('[role="option"]');
    assert.ok(option);
    await act(async () => option.click());
    assert.equal(draft.value, '/skill review ');
  } finally {
    await act(async () => root.unmount());
    localStorage.clear();
  }
});
