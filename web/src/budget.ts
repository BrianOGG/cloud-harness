import type { Budget, BudgetExtension } from './api';

const counters = [
  ['模型调用', 'used_model_calls', 'max_model_calls', '✧'],
  ['工具调用', 'used_tool_calls', 'max_tool_calls', '⌘'],
  ['脚本调用', 'used_script_calls', 'max_script_calls', '⟡'],
  ['子任务', 'used_child_runs', 'max_child_runs', '⑂'],
] as const;

const usedTokens = (budget: Budget) => (budget?.used_input_tokens ?? 0) + (budget?.used_output_tokens ?? 0);
const usd = (amount: number) => `$${amount.toFixed(amount > 0 && amount < 0.01 ? 4 : 2)}`;

export interface BudgetCounter {
  label: string;
  used: number;
  max: number;
  icon: string;
  usedText: string;
  // "不限" for a token or cost limit of 0.
  maxText: string;
}

export function getBudgetCounters(budget: Budget): BudgetCounter[] {
  const calls = counters.map(([label, usedKey, maxKey, icon]) => {
    const used = budget?.[usedKey] ?? 0;
    const max = budget?.[maxKey] ?? 0;
    return { label, used, max, icon, usedText: String(used), maxText: String(max) };
  });
  const tokens = usedTokens(budget);
  const maxTokens = budget?.max_tokens ?? 0;
  const cost = budget?.used_cost_usd ?? 0;
  const maxCost = budget?.max_cost_usd ?? 0;
  return [
    ...calls,
    { label: 'Token', used: tokens, max: maxTokens, icon: '∑', usedText: tokens.toLocaleString('en-US'), maxText: maxTokens > 0 ? maxTokens.toLocaleString('en-US') : '不限' },
    { label: '预估费用', used: cost, max: maxCost, icon: '$', usedText: usd(cost), maxText: maxCost > 0 ? usd(maxCost) : '不限' },
  ];
}

// What a run waiting for budget needs before it can make another model call:
// every limit it has reached is raised. A token or cost limit goes to half of
// the limit above what is already used, since the last call may have crossed it.
export function budgetExtension(budget: Budget): { title: string; label: string; extension: BudgetExtension } {
  const extension: BudgetExtension = {};
  const titles: string[] = [];
  const labels: string[] = [];
  const maxTokens = budget?.max_tokens ?? 0;
  if (maxTokens > 0 && usedTokens(budget) >= maxTokens) {
    extension.extra_tokens = usedTokens(budget) - maxTokens + Math.ceil(maxTokens / 2);
    titles.push('Token 预算已用完');
    labels.push(`${extension.extra_tokens.toLocaleString('en-US')} 个 token`);
  }
  const maxCost = budget?.max_cost_usd ?? 0;
  const cost = budget?.used_cost_usd ?? 0;
  if (maxCost > 0 && cost >= maxCost) {
    extension.extra_cost_usd = Math.max(Math.ceil((cost - maxCost + maxCost / 2) * 100) / 100, 0.01);
    titles.push('费用预算已用完');
    labels.push(`${usd(extension.extra_cost_usd)} 费用上限`);
  }
  if (titles.length === 0 || (budget?.used_model_calls ?? 0) >= (budget?.max_model_calls ?? 0)) {
    extension.extra_model_calls = 10;
    titles.unshift('模型调用预算已用完');
    labels.unshift('10 次模型调用');
  }
  return { title: titles.length === 1 ? titles[0] : '运行预算已用完', label: `增加 ${labels.join('、')}`, extension };
}
