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

try {
  await test('模式名称及范围保持现有 ID 与真实授权边界', () => {
    assert.equal(approvalModeLabel('ask'), '按策略确认');
    assert.equal(approvalModeLabel('safe'), '文件读取与 Git 查看自动');
    assert.equal(approvalModeLabel('workspace-write'), '工作区文件编辑自动 · 命令另行确认');
    assert.equal(approvalModeLabel(), '未记录审批模式');
    assert.match(approvalModeDescription('ask'), /不是每个读取工具都询问/);
    for (const tool of ['read_file', 'list_dir', 'list_files', 'grep', 'git_status', 'git_diff', 'git_log']) assert.ok(approvalModeDescription('safe').includes(tool));
    assert.match(approvalModeDescription('safe'), /七类.*不覆盖拒绝/);
    assert.match(approvalModeDescription('workspace-write'), /边界校验.*write_file、edit_file/);
    assert.match(approvalWaitReason('workspace-write', 'run_command'), /这条命令不在模式自动执行范围/);
    assert.match(approvalWaitReason('safe', 'run_command'), /不包含命令自动授权/);
    assert.match(approvalWaitReason(undefined, 'run_command'), /不能用下一轮偏好推断权限/);
    assert.match(approvalWaitReason('ask', 'write_file'), /既有策略与底层许可/);
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
    assert.equal(container.querySelectorAll('.mode-option').length, 3);
    assert.equal(container.textContent?.includes('自动执行受限诊断命令'), false);
    await act(async () => { container.querySelectorAll<HTMLButtonElement>('.mode-option')[2].click(); });
    assert.deepEqual(changed, ['workspace-write']);
    assert.equal(container.querySelector('.approval-mode-popover'), null);
    assert.equal(document.activeElement, trigger());
  });
  await test('键盘退出、外点关闭与禁用状态保持可用', async () => {
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
    assert.deepEqual(changed, ['workspace-write']);
  });
} finally {
  if (root) await act(async () => { root!.unmount(); });
  dom.window.close();
}
