import { useMemo } from 'react';
import type { HarnessEvent, RunView, SessionMessage } from './api';
import { Turn } from './Chat';
import type { TurnActions } from './Chat';
import { Icon } from './Icon';
import { projectTranscript } from './transcript';

export function SessionHistory({ messages, runs = [], events = {}, live = {}, acting = {}, actions, loading, error, onRefresh, onInspect }: {
  messages: readonly SessionMessage[];
  runs?: readonly RunView[];
  events?: Record<string, HarnessEvent[]>;
  live?: Record<string, string>;
  acting?: Record<string, boolean>;
  actions?: TurnActions;
  loading: boolean;
  error: string;
  onRefresh: () => void;
  onInspect: (runId: string) => void;
}) {
  const turns = useMemo(() => projectTranscript(messages, runs), [messages, runs]);
  const turnActions = actions || { respond() {}, addBudget() {}, cancel() {}, inspect: onInspect, focusAnswer() {}, loadEvents() {} };
  return (
    <section className="session-history" aria-label="会话历史" aria-busy={loading}>
      <div className="history-toolbar">
        <span role="status">{loading ? (turns.length ? '正在同步历史…' : '正在加载完整历史…') : '会话历史'}</span>
        <button type="button" className="ghost-button" disabled={loading} onClick={onRefresh} aria-label="刷新会话历史"><Icon name="refresh" size={14} /><span>刷新</span></button>
      </div>
      {error && <div className="history-error" role="alert"><span>历史加载失败：{error}</span><button type="button" className="button" onClick={onRefresh}>重试</button></div>}
      {!loading && !error && turns.length === 0 && <p className="history-empty">这个会话还没有已保存的消息。</p>}
      {turns.map((turn) => <Turn key={turn.id} turn={turn} events={events} live={live} acting={acting} actions={turnActions} />)}
    </section>
  );
}
