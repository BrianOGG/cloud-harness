import type { Budget } from './api';

const counters = [
  ['模型调用', 'used_model_calls', 'max_model_calls', '✧'],
  ['工具调用', 'used_tool_calls', 'max_tool_calls', '⌘'],
  ['脚本调用', 'used_script_calls', 'max_script_calls', '⟡'],
  ['子任务', 'used_child_runs', 'max_child_runs', '⑂'],
] as const;

export function getBudgetCounters(budget: Budget) {
  return counters.map(([label, usedKey, maxKey, icon]) => ({
    label,
    used: budget?.[usedKey] ?? 0,
    max: budget?.[maxKey] ?? 0,
    icon,
  }));
}
