import type { RunView } from './api';

export const shortId = (id: string) => (id.length > 16 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id);

export const isActive = (run: Pick<RunView, 'status'>) => run.status === 'RUNNING' || run.status === 'WAITING';

const statusLabels: Record<string, string> = {
  RUNNING: '运行中', WAITING: '等待处理', COMPLETED: '已完成', FAILED: '失败', CANCELLED: '已取消',
  PENDING: '排队中', WAITING_APPROVAL: '待审批', SUCCEEDED: '成功', DENIED: '已拒绝', SKIPPED: '已跳过', UNKNOWN: '结果未知',
  COMPLETE: '已完成',
};
export const statusName = (status?: string) => {
  const value = status?.trim() || '';
  return statusLabels[value.toUpperCase()] || value || '未知';
};

export type Tone = 'run' | 'wait' | 'ok' | 'fail' | 'idle';
export function statusTone(status?: string): Tone {
  const value = (status || '').toUpperCase();
  if (value === 'RUNNING') return 'run';
  if (value.startsWith('WAITING') || value === 'PENDING' || value === 'UNKNOWN') return 'wait';
  if (value === 'COMPLETED' || value === 'SUCCEEDED' || value === 'COMPLETE') return 'ok';
  if (value === 'FAILED' || value === 'DENIED') return 'fail';
  return 'idle';
}

export const waitLabels: Record<string, string> = {
  USER: '等待你的回答', APPROVAL: '等待审批', CHILD: '等待子任务', BUDGET: '等待补充预算', RECONCILIATION: '等待确认工具结果',
};

export const actionLabels: Record<string, string> = {
  ANSWER: '生成回复', RETRIEVE: '检索上下文', EXECUTE: '决定调用工具', PLAN: '制定计划', ASK_USER: '向你提问',
};

export function errorText(error: unknown) {
  return error instanceof Error ? error.message : '请求失败，请检查 serve 连接后重试。';
}

export function elapsedLabel(seconds: number) {
  if (seconds < 60) return `已等待 ${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  return minutes < 60 ? `已等待 ${minutes} 分钟` : `已等待 ${Math.floor(minutes / 60)} 小时`;
}

// Sidebar group of a session by the day of its latest activity.
export function dayGroup(value: string | undefined, now = new Date()): string {
  const date = value ? new Date(value) : null;
  if (!date || Number.isNaN(date.valueOf())) return '更早';
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate()).valueOf();
  const day = 86_400_000;
  if (date.valueOf() >= start) return '今天';
  if (date.valueOf() >= start - day) return '昨天';
  if (date.valueOf() >= start - 6 * day) return '近 7 天';
  return '更早';
}
