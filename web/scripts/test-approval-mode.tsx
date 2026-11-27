import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import React, { act } from 'react';
import type { Root } from 'react-dom/client';

const dom = new JSDOM('<!doctype html><div id="root"></div><button id="outside">外部</button>', { url: 'http://fixture.invalid/ui/' });
for (const key of ['window', 'document', 'HTMLElement', 'Event', 'MouseEvent']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key as keyof typeof dom.window] });
}
Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true });
// jsdom has no native dialog methods. The browser provides modal focus
// containment; these shims let the tests exercise our confirmation state.
dom.window.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
dom.window.HTMLDialogElement.prototype.close = function () { this.open = false; };
const { createRoot } = await import('react-dom/client');
const { ApprovalModeMenu } = await import('../src/ApprovalMode');
const { approvalModeLabel, approvalModeDescription, approvalWaitReason } = await import('../src/approval');
const container = document.getElementById('root')!;
let root: Root | undefined;
const changed: string[] = [];
async function mount(value = 'ask', disabled = false) {
  if (root) await act(async () => { root!.unmount(); });
  root = createRoot(container);
  await act(async () => { root!.render(<ApprovalModeMenu value={value} disabled={disabled} onChange={(mode) => changed.push(mode)} />); });
}
const trigger = () => container.querySelector<HTMLButtonElement>('button[aria-label="审批模式"]')!;
async function open() { await act(async () => { trigger().click(); }); }
async function selectFullAuto() {
  await open();
  await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[3].click(); });
}
const confirmation = () => container.querySelector<HTMLDialogElement>('dialog')!;
const enable = () => confirmation().querySelector<HTMLButtonElement>('.primary')!;

try {
  await test('模式名称及范围保持现有 ID 与真实授权边界', () => {
    assert.equal(approvalModeLabel('ask'), '按策略确认');
    assert.equal(approvalModeLabel('safe'), '文件读取与 Git 查看自动');
    assert.equal(approvalModeLabel('workspace-write'), '工作区文件编辑自动 · 命令另行确认');
    assert.equal(approvalModeLabel('full-auto'), '完全自主');
    assert.equal(approvalModeLabel(), '未记录审批模式');
    assert.match(approvalModeDescription('ask'), /不是每个读取工具都询问/);
    for (const tool of ['read_file', 'list_dir', 'list_files', 'grep', 'git_status', 'git_diff', 'git_log']) assert.ok(approvalModeDescription('safe').includes(tool));
    assert.match(approvalModeDescription('safe'), /七类.*不覆盖拒绝/);
    assert.match(approvalModeDescription('workspace-write'), /边界校验.*write_file、edit_file/);
    assert.equal(approvalWaitReason(), '此调用需要你确认。');
    assert.match(approvalModeDescription('full-auto'), /命令、写入及其他工具调用.*既有拒绝规则仍生效/);
    assert.doesNotMatch(approvalWaitReason(), /应自动放行|已自动执行|模式未记录/);
  });
  await test('菜单可见下一轮生效信息并明确保留既有命令规则', async () => {
    await mount();
    assert.equal(trigger().textContent, '下一轮 · 按策略确认');
    assert.equal(trigger().getAttribute('aria-expanded'), 'false');
    await open();
    assert.equal(trigger().getAttribute('aria-expanded'), 'true');
    assert.match(container.textContent || '', /仅下一轮生效，不改变正在运行或等待的回合/);
    assert.match(container.textContent || '', /不会自动重放或放行当前调用/);
    assert.ok(container.textContent?.includes('既有明确允许与默认安全命令 ls / pwd / wc'));
    assert.equal(container.querySelectorAll('.mode-option').length, 4);
    assert.equal(container.querySelectorAll('.mode-option strong')[3].textContent, '完全自主');
    assert.ok(container.textContent?.includes(approvalModeDescription('full-auto')));
    assert.equal(container.textContent?.includes('自动执行受限诊断命令'), false);
    await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[2].click(); });
    assert.deepEqual(changed, ['workspace-write']);
    assert.equal(container.querySelector('.approval-mode-popover'), null);
    assert.equal(document.activeElement, trigger());
  });
  await test('进入完全自主必须勾选风险确认，取消和关闭不保存；已选时不重复确认', async () => {
    for (const mode of ['ask', 'safe', 'workspace-write']) {
      changed.length = 0;
      await mount(mode); await open();
      await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[3].click(); });
      const dialog = confirmation();
      assert.equal(dialog.open, true);
      const warning = dialog.textContent || '';
      for (const text of ['确认启用完全自主？', '下一轮', '工作区外', '不再逐次询问', '提示注入', '执行命令', '对外发送数据', '既有拒绝规则仍生效', '不是安全保障', '信任当前任务和环境', '我已了解风险，并愿意继续']) assert.ok(warning.includes(text), text);
      assert.equal(enable().disabled, true);
      await act(async () => { enable().click(); });
      assert.deepEqual(changed, []);
      assert.equal(trigger().textContent, '下一轮 · ' + approvalModeLabel(mode));
      await act(async () => { dialog.querySelector<HTMLInputElement>('input[type="checkbox"]')!.click(); });
      assert.equal(enable().disabled, false);
      await act(async () => { enable().click(); });
      assert.deepEqual(changed, ['full-auto']);
      assert.equal(container.querySelector('dialog'), null);
      assert.equal(document.activeElement, trigger());
    }

    for (const close of ['cancel', 'escape', 'close']) {
      changed.length = 0;
      await mount('safe'); await open();
      await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[3].click(); });
      assert.equal(confirmation().querySelector<HTMLInputElement>('input')!.checked, false);
      await act(async () => { confirmation().querySelector<HTMLInputElement>('input')!.click(); });
      await act(async () => {
        if (close === 'cancel') confirmation().querySelector<HTMLButtonElement>('button')!.click();
        else confirmation().dispatchEvent(new dom.window.Event(close === 'escape' ? 'cancel' : 'close'));
      });
      assert.equal(container.querySelector('dialog'), null);
      assert.deepEqual(changed, []);
      assert.equal(trigger().textContent, '下一轮 · 文件读取与 Git 查看自动');
    }

    changed.length = 0;
    await mount('full-auto'); await open();
    await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[3].click(); });
    assert.equal(container.querySelector('dialog'), null);
    assert.deepEqual(changed, []);
    await open();
    await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[1].click(); });
    assert.deepEqual(changed, ['safe']);
    assert.equal(container.querySelector('dialog'), null);
  });
  await test('键盘退出、外点关闭与禁用状态保持可用', async () => {
    changed.length = 0;
    await mount('safe'); await open();
    await act(async () => { document.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true })); });
    assert.equal(container.querySelector('.approval-mode-popover'), null);
    assert.equal(document.activeElement, trigger());
    await open();
    await act(async () => { document.getElementById('outside')!.dispatchEvent(new dom.window.Event('pointerdown', { bubbles: true })); });
    assert.equal(container.querySelector('.approval-mode-popover'), null);
    await mount('workspace-write', true); await open();
    assert.equal(trigger().disabled, true);
    assert.equal(container.querySelector('.approval-mode-popover'), null);
    assert.deepEqual(changed, []);
  });
} finally {
  if (root) await act(async () => { root!.unmount(); });
  dom.window.close();
}
