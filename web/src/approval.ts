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

// This describes the mode boundary, not a guessed backend policy decision.
export function approvalWaitReason(mode?: string, tool?: string): string {
  if (mode === 'full-auto') return `本轮记录为完全自主，策略要求询问的调用应自动放行；${tool === 'run_command' ? '这条命令' : '此调用'}当前仍由后端要求确认，请以实际待办为准。`;
  if (tool === 'run_command') {
    if (mode === 'workspace-write') return '本轮模式仅自动授权允许范围内的文件读取与编辑，这条命令不在模式自动执行范围；当前由后端要求确认。';
    if (mode === 'safe') return '本轮模式仅自动授权文件读取与 Git 查看，不包含命令自动授权；这条命令当前由后端要求确认。';
    if (mode === 'ask') return '本轮按既有策略与底层许可判定；这条命令当前由后端要求确认。';
    return '这条命令当前由后端要求确认；该轮模式未记录，不能用下一轮偏好推断权限。';
  }
  if (mode === 'workspace-write') return '本轮仅自动授权通过后端边界校验的文件读取与编辑；此调用仍由后端要求确认。';
  if (mode === 'safe') return '本轮仅自动授权既有七类文件读取与 Git 查看工具；此调用仍由后端要求确认。';
  if (mode === 'ask') return '本轮按既有策略与底层许可判定；此调用由后端要求确认。';
  return '此调用正在等待后端审批；该轮模式未记录，不能用下一轮偏好推断权限。';
}
