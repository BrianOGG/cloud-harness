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
  await test('模式短名称与说明保持既定文案', () => {
    assert.equal(approvalModeLabel('ask'), '按策略确认');
    assert.equal(approvalModeLabel('safe'), '读取自动');
    assert.equal(approvalModeLabel('workspace-write'), '工作区自动');
    assert.equal(approvalModeLabel('full-auto'), '完全自主');
    assert.equal(approvalModeLabel(), '未记录审批模式');
    assert.equal(approvalModeDescription('ask'), '按已有规则询问或拒绝。');
    assert.equal(approvalModeDescription('safe'), '读文件和查看 Git 自动执行，写入和命令仍需确认。');
    assert.equal(approvalModeDescription('workspace-write'), '工作区内读写自动执行，命令仍需确认。');
    assert.equal(approvalModeDescription('full-auto'), '自动执行本会询问的调用；拒绝规则仍生效。仅在你信任当前任务时使用。');
    assert.equal(approvalWaitReason(), '此调用需要你确认。');
    assert.doesNotMatch(approvalWaitReason(), /应自动放行|已自动执行|模式未记录/);
  });
  await test('菜单只展示四种模式短名称，芯片承载下一轮提示', async () => {
    await mount();
    assert.equal(trigger().textContent, '下一轮 · 按策略确认');
    assert.equal(trigger().title, '下一轮审批模式；不改变正在运行或等待的回合');
    assert.equal(trigger().getAttribute('aria-expanded'), 'false');
    await open();
    assert.equal(trigger().getAttribute('aria-expanded'), 'true');
    assert.equal(container.querySelectorAll('.mode-option').length, 4);
    assert.deepEqual(Array.from(container.querySelectorAll('.mode-option strong'), (node) => node.textContent), [
      '按策略确认', '读取自动', '工作区自动', '完全自主',
    ]);
    assert.equal(container.querySelectorAll('.mode-option span').length, 0);
    const menuText = container.querySelector('.approval-mode-popover')?.textContent || '';
    for (const removed of [
      '按已有规则询问或拒绝。',
      '读文件和查看 Git 自动执行，写入和命令仍需确认。',
      '工作区内读写自动执行，命令仍需确认。',
      '自动执行本会询问的调用；拒绝规则仍生效。仅在你信任当前任务时使用。',
      'ask', 'safe', 'workspace-write', 'ls', 'pwd', 'wc', 'read_file', 'list_dir', 'list_files', 'grep', 'git_status', 'git_diff', 'git_log',
      '底层许可', '敏感或控制路径', '仅下一轮生效，不改变正在运行或等待的回合', '不会自动重放或放行当前调用',
    ]) assert.equal(menuText.includes(removed), false, removed);
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
      assert.equal(trigger().textContent, '下一轮 · 读取自动');
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
