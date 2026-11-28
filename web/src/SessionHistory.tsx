import { useMemo } from 'react';
import type { HarnessEvent, RunView, SessionMessage } from './api';
import { Turn } from './Chat';
import type { TurnActions } from './Chat';
import { projectTranscript } from './transcript';

export function SessionHistory({ messages, runs = [], events = {}, live = {}, acting = {}, actions, approvalMode, loading, error, onRefresh, onInspect }: {
  messages: readonly SessionMessage[];
  runs?: readonly RunView[];
  events?: Record<string, HarnessEvent[]>;
  live?: Record<string, string>;
  acting?: Record<string, boolean>;
  actions?: TurnActions;
  approvalMode?: string;
  loading: boolean;
  error: string;
  onRefresh: () => void;
  onInspect: (runId: string) => void;
}) {
  const turns = useMemo(() => projectTranscript(messages, runs), [messages, runs]);
  const turnActions = actions || { respond() {}, addBudget() {}, cancel() {}, inspect: onInspect, focusAnswer() {}, loadEvents() {} };
  return (
    <section className="session-history" aria-label="会话历史" aria-busy={loading}>
      {loading && turns.length === 0 && <p className="history-empty" role="status">正在加载历史…</p>}
      {error && <div className="history-error" role="alert"><span>历史加载失败：{error}</span><button type="button" className="button" onClick={onRefresh}>重试</button></div>}
      {!loading && !error && turns.length === 0 && <p className="history-empty">这个会话还没有已保存的消息。</p>}
      {turns.map((turn) => <Turn key={turn.id} turn={turn} events={events} live={live} acting={acting} actions={turnActions} approvalMode={approvalMode} />)}
    </section>
  );
}
