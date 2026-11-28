import type { HarnessEvent } from './api';
import { eventPayload } from './timeline';

interface RuleRow {
  label: string;
  bytes?: number;
  truncated?: boolean;
  error?: string;
}

export function RuleSources({ events }: { events: HarnessEvent[] }) {
  const rows = new Map<string, RuleRow>();
  for (const event of events) {
    if (!event.type.startsWith('rules.')) continue;
    const data = eventPayload(event.data);
    if (event.type === 'rules.skipped' && typeof data.path === 'string') {
      rows.set(data.path, { label: data.path, error: String(data.error || '无法读取') });
      continue;
    }
    if (!Array.isArray(data.files)) continue;
    for (const file of data.files) {
      if (!file || typeof file.path !== 'string') continue;
      const source = typeof file.source === 'string' ? file.source : file.path;
      const row: RuleRow = rows.get(file.path) || { label: source === 'global' ? '全局 AGENTS.md' : source };
      if (event.type === 'rules.loaded' && typeof file.bytes === 'number') row.bytes = file.bytes;
      if (event.type === 'rules.truncated') row.truncated = true;
      rows.set(file.path, row);
    }
  }
  if (!rows.size) return null;
  return <details className="rule-sources">
    <summary>项目规则 · {rows.size}</summary>
    <ul>{Array.from(rows, ([path, row]) => <li key={path}>
      <code title={path}>{row.label}</code>
      <small>{row.error || [row.bytes === undefined ? '未载入' : `${row.bytes} B`, row.truncated && '已达长度上限'].filter(Boolean).join(' · ')}</small>
    </li>)}</ul>
  </details>;
}
