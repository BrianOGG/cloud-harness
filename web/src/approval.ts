import type { PendingInvocation } from './api';

export const approvalModes = [
  {
    id: 'ask',
    label: '按策略确认',
    description: '继续使用既有策略与底层许可；不是每个读取工具都询问。原来需授权的仍询问，既有拒绝仍拒绝。',
  },
  {
    id: 'safe',
    label: '文件读取与 Git 查看自动',
    description: '仅自动授权 read_file、list_dir、list_files、grep、git_status、git_diff、git_log 七类工具，不覆盖拒绝。命令不会因本模式自动获准。',
  },
  {
    id: 'workspace-write',
    label: '工作区文件编辑自动 · 命令另行确认',
    description: '在文件读取与 Git 查看范围上，增加通过现有边界校验的 write_file、edit_file；不授予工作区外写入、敏感或控制路径权限。命令不会因本模式自动获准。',
  },
  {
    id: 'full-auto',
    label: '完全自主',
    description: '自动执行本会询问的命令、写入及其他工具调用；既有拒绝规则仍生效。仅在你信任当前任务时使用。',
  },
];

export const commandScope = '既有明确允许与默认安全命令 ls / pwd / wc 仍按原规则处理；ask / safe / workspace-write 下的其他命令按策略询问或拒绝；完全自主自动放行策略要求询问的调用，既有拒绝仍拒绝。';

// Missing snapshot data must never be replaced by the next-turn preference.
export function approvalModeLabel(mode?: string): string {
  return approvalModes.find((item) => item.id === mode)?.label || mode || '未记录审批模式';
}

export function approvalModeDescription(mode?: string): string {
  return approvalModes.find((item) => item.id === mode)?.description || '该轮未记录可识别的审批模式，请以实际待办与后端策略为准。';
}

export function approvalWaitReason(): string {
  return '此调用需要你确认。';
}

export function approvalOperation(pending: PendingInvocation): string {
  const tool = pending.tool || pending.name || '工具调用';
  const args = pending.arguments && typeof pending.arguments === 'object' && !Array.isArray(pending.arguments)
    ? pending.arguments as Record<string, unknown> : {};
  if (tool === 'run_command' && typeof args.command === 'string' && args.command.trim()) return args.command;
  const path = [args.path, args.file_path, args.directory, args.url, pending.resources?.[0]]
    .find((value): value is string => typeof value === 'string' && Boolean(value.trim()));
  return path ? tool + ' · ' + path : tool;
}

// Compare lexical paths without claiming to resolve backend symlinks.
export function approvalWorkingDirectory(workspace: string | undefined, cwd: unknown): string | undefined {
  if (typeof cwd !== 'string' || !cwd.trim()) return undefined;
  const normalize = (path: string) => {
    const absolute = path.startsWith('/');
    const parts: string[] = [];
    for (const part of path.split('/')) {
      if (!part || part === '.') continue;
      if (part === '..' && parts.length && parts.at(-1) !== '..') parts.pop();
      else if (part !== '..' || !absolute) parts.push(part);
    }
    return (absolute ? '/' : '') + parts.join('/') || '.';
  };
  const resolved = normalize(cwd.startsWith('/') || !workspace ? cwd : workspace + '/' + cwd);
  return resolved === normalize(workspace || '.') ? undefined : resolved;
}
