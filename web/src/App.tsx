import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from './api';
import type { ApprovalView, HarnessEvent, RunView, SessionView, TraceView } from './api';
import { RunBudget } from './RunBudget';
import { Observability } from './Observability';
import './App.css';

type Screen = 'sessions' | 'session' | 'run' | 'approvals' | 'observability';
type Panel = 'trace' | 'events';
const THEME_KEY = 'cloud-harness.web-theme';
const MESSAGES_KEY = 'cloud-harness.web-messages';

function readMessages(): Record<string, string> {
  try {
    return JSON.parse(localStorage.getItem(MESSAGES_KEY) || '{}') as Record<string, string>;
  } catch {
    return {};
  }
}

const shortId = (id: string) => (id.length > 18 ? `${id.slice(0, 12)}…${id.slice(-4)}` : id);
const statusLabel: Record<string, string> = {
  RUNNING: '运行中', WAITING: '等待处理', COMPLETED: '已完成', FAILED: '失败', CANCELLED: '已取消',
};
const normalizedStatus = (status?: string) => status?.trim() || '';
const statusClass = (status?: string) => `status status-${normalizedStatus(status).toLowerCase() || 'unknown'}`;
const statusName = (status?: string) => {
  const value = normalizedStatus(status);
  return statusLabel[value] || value || '未知';
};
const actionLabel: Record<string, string> = {
  ANSWER: '生成回复', RETRIEVE: '检索上下文', EXECUTE: '执行工具', PLAN: '制定计划', ASK_USER: '向你提问',
};

function dateLabel(value?: string) {
  if (!value) return '时间未知';
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? '时间未知' : new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

function elapsedLabel(seconds: number) {
  if (seconds < 60) return `等待 ${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  return minutes < 60 ? `等待 ${minutes} 分钟` : `等待 ${Math.floor(minutes / 60)} 小时`;
}

function errorText(error: unknown) {
  return error instanceof Error ? error.message : '请求失败，请检查 serve 连接后重试。';
}

function eventSummary(data: string) {
  try {
    const value = JSON.parse(data) as unknown;
    return JSON.stringify(value, null, 2);
  } catch {
    return data || '—';
  }
}

export default function App() {
  const [screen, setScreen] = useState<Screen>('sessions');
  const [theme, setTheme] = useState<'light' | 'dark'>(() => localStorage.getItem(THEME_KEY) === 'dark' ? 'dark' : 'light');
  const [sessions, setSessions] = useState<SessionView[]>([]);
  const [runs, setRuns] = useState<Record<string, RunView[]>>({});
  const [messages, setMessages] = useState<Record<string, string>>(readMessages);
  const [activeSession, setActiveSession] = useState<string | null>(null);
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null);
  const [approvals, setApprovals] = useState<ApprovalView[]>([]);
  const [events, setEvents] = useState<Record<string, HarnessEvent[]>>({});
  const [traceState, setTraceState] = useState<{ runId: string; data: TraceView } | null>(null);
  const [panel, setPanel] = useState<Panel>('trace');
  const [provider, setProvider] = useState('');
  const [online, setOnline] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [draft, setDraft] = useState('');
  const [answer, setAnswer] = useState('');
  const [sending, setSending] = useState(false);
  const [actingRunId, setActingRunId] = useState<string | null>(null);
  const lastSeq = useRef<Record<string, number>>({});

  const activeRuns = useMemo(() => activeSession ? runs[activeSession] || [] : [], [activeSession, runs]);
  const selectedRun = useMemo(
    () => activeRuns.find((run) => run.id === selectedRunId) || activeRuns[activeRuns.length - 1] || null,
    [activeRuns, selectedRunId],
  );
  const activeRunKey = useMemo(
    () => activeRuns.filter((run) => run.status === 'RUNNING' || run.status === 'WAITING').map((run) => run.id).join(','),
    [activeRuns],
  );

  const refreshSessions = useCallback(async () => {
    const rows = await api.listSessions();
    setSessions(rows);
    return rows;
  }, []);

  const refreshRuns = useCallback(async (sessionId: string) => {
    const rows = await api.listRuns(sessionId);
    setRuns((previous) => ({ ...previous, [sessionId]: rows }));
    return rows;
  }, []);

  const refreshRun = useCallback(async (runId: string) => {
    const run = await api.getRun(runId);
    setRuns((previous) => {
      const list = previous[run.session_id] || [];
      const index = list.findIndex((item) => item.id === run.id);
      const next = [...list];
      if (index >= 0) next[index] = run;
      else next.push(run);
      return { ...previous, [run.session_id]: next };
    });
    return run;
  }, []);

  const pullEvents = useCallback(async (runId: string) => {
    const after = lastSeq.current[runId] || 0;
    const fresh = await api.fetchEvents(runId, after);
    if (!fresh.length) return;
    const numbered = fresh.filter((event) => event.seq > 0);
    if (numbered.length) lastSeq.current[runId] = Math.max(after, ...numbered.map((event) => event.seq));
    setEvents((previous) => {
      const current = previous[runId] || [];
      const keys = new Set(current.map((event) => `${event.seq}:${event.type}`));
      const merged = [...current];
      for (const event of fresh) {
        const key = `${event.seq}:${event.type}`;
        if (!keys.has(key)) merged.push(event);
      }
      return { ...previous, [runId]: merged.sort((a, b) => a.seq - b.seq) };
    });
  }, []);

  const refreshQueue = useCallback(async () => {
    setApprovals(await api.listApprovals());
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem(THEME_KEY, theme);
  }, [theme]);

  useEffect(() => {
    localStorage.setItem(MESSAGES_KEY, JSON.stringify(messages));
  }, [messages]);

  useEffect(() => {
    let mounted = true;
    api.health().then((health) => {
      if (!mounted) return;
      setOnline(true);
      setProvider(health.provider);
    }).catch((reason: unknown) => {
      if (!mounted) return;
      setOnline(false);
      setError(`无法连接 serve：${errorText(reason)}`);
    });
    queueMicrotask(() => {
      if (mounted) refreshSessions().catch((reason: unknown) => setError(`会话加载失败：${errorText(reason)}`));
    });
    return () => { mounted = false; };
  }, [refreshSessions]);

  useEffect(() => {
    if (!activeSession) return;
    const sessionId = activeSession;
    queueMicrotask(() => refreshRuns(sessionId).catch((reason: unknown) => setError(`运行列表加载失败：${errorText(reason)}`)));
  }, [activeSession, refreshRuns]);

  useEffect(() => {
    if (screen !== 'approvals') return;
    queueMicrotask(() => refreshQueue().catch((reason: unknown) => setError(`审批队列加载失败：${errorText(reason)}`)));
    const timer = window.setInterval(() => {
      refreshQueue().catch((reason: unknown) => setError(`审批队列刷新失败：${errorText(reason)}`));
    }, 1200);
    return () => window.clearInterval(timer);
  }, [screen, refreshQueue]);

  useEffect(() => {
    if (!activeSession) return;
    if (!activeRunKey) return;
    const activeIds = activeRunKey.split(',');
    let cancelled = false;
    const timer = window.setInterval(() => {
      void (async () => {
        for (const runId of activeIds) {
          if (cancelled) return;
          try {
            await refreshRun(runId);
            await pullEvents(runId);
            if (selectedRunId === runId) {
              const freshTrace = await api.getTrace(runId);
              if (!cancelled) setTraceState({ runId, data: freshTrace });
            }
          } catch {
            // A later interval retries transient disconnects.
          }
        }
        refreshSessions().catch(() => undefined);
      })();
    }, 1000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [activeSession, activeRunKey, pullEvents, refreshRun, refreshSessions, selectedRunId]);

  const selectedRunStatus = selectedRun?.status;
  useEffect(() => {
    if (screen !== 'run' || !selectedRunId) return;
    let cancelled = false;
    void Promise.all([
      api.getTrace(selectedRunId).then((next) => { if (!cancelled) setTraceState({ runId: selectedRunId, data: next }); }).catch((reason: unknown) => {
        if (!cancelled) setError(`轨迹加载失败：${errorText(reason)}`);
      }),
      pullEvents(selectedRunId).catch((reason: unknown) => {
        if (!cancelled) setError(`事件加载失败：${errorText(reason)}`);
      }),
    ]);
    return () => { cancelled = true; };
  }, [screen, selectedRunId, selectedRunStatus, pullEvents]);

  const openSession = (sessionId: string) => {
    setError('');
    setActiveSession(sessionId);
    setSelectedRunId(null);
    setScreen('session');
  };

  const showSessions = () => {
    setError('');
    setScreen('sessions');
    refreshSessions().catch((reason: unknown) => setError(`会话加载失败：${errorText(reason)}`));
  };

  const createSession = async () => {
    setError('');
    try {
      const created = await api.createSession();
      setActiveSession(created.id);
      setRuns((previous) => ({ ...previous, [created.id]: [] }));
      setSessions((previous) => [{ id: created.id, created_at: new Date().toISOString(), updated_at: new Date().toISOString(), run_count: 0 }, ...previous]);
      setScreen('session');
    } catch (reason) {
      setError(`新建会话失败：${errorText(reason)}`);
    }
  };

  const sendMessage = async () => {
    const text = draft.trim();
    if (!text || sending) return;
    setSending(true);
    setError('');
    try {
      let sessionId = activeSession;
      if (!sessionId) {
        const created = await api.createSession();
        sessionId = created.id;
        setActiveSession(sessionId);
        setSessions((previous) => [{ id: sessionId!, created_at: new Date().toISOString(), updated_at: new Date().toISOString(), run_count: 0 }, ...previous]);
      }
      const run = await api.createRun(sessionId, text);
      setRuns((previous) => ({ ...previous, [sessionId!]: [...(previous[sessionId!] || []), run] }));
      setMessages((previous) => ({ ...previous, [run.id]: text }));
      setSelectedRunId(run.id);
      setDraft('');
      setPanel('trace');
      setScreen('run');
      void pullEvents(run.id).catch(() => undefined);
      refreshSessions().catch(() => undefined);
    } catch (reason) {
      setError(`发送失败：${errorText(reason)}`);
    } finally {
      setSending(false);
    }
  };

  const respond = async (runId: string, payload: { decision?: string; answer?: string }) => {
    setError('');
    setActingRunId(runId);
    try {
      const updated = await api.respond(runId, payload);
      setRuns((previous) => ({
        ...previous,
        [updated.session_id]: (previous[updated.session_id] || []).map((run) => run.id === updated.id ? updated : run),
      }));
      setNotice('已提交，运行会在后台继续。');
      window.setTimeout(() => setNotice(''), 3200);
      await Promise.all([refreshQueue().catch(() => undefined), pullEvents(runId).catch(() => undefined)]);
      window.setTimeout(() => { refreshRun(runId).catch(() => undefined); }, 350);
    } catch (reason) {
      setError(`提交失败：${errorText(reason)}`);
    } finally {
      setActingRunId(null);
    }
  };

  const createAndShowSession = async () => {
    await createSession();
  };

  const openRunById = async (runId: string) => {
    setError('');
    try {
      const run = await api.getRun(runId);
      setActiveSession(run.session_id);
      setRuns((previous) => ({ ...previous, [run.session_id]: [...(previous[run.session_id] || []).filter((item) => item.id !== run.id), run] }));
      setSelectedRunId(run.id);
      setPanel('trace');
      setScreen('run');
    } catch (reason) {
      setError(`运行加载失败：${errorText(reason)}`);
    }
  };

  const runEvents = selectedRun ? events[selectedRun.id] || [] : [];
  const localMessage = selectedRun ? messages[selectedRun.id] : '';
  const waiting = selectedRun?.status === 'WAITING' ? selectedRun : null;
  const trace = selectedRun && traceState?.runId === selectedRun.id ? traceState.data : null;

  return (
    <div className="app-shell">
      <header className="topbar">
        <button className="brand" type="button" onClick={showSessions} aria-label="Cloud Harness 首页">
          <span className="brand-mark">☁</span><span>Cloud Harness</span>
        </button>
        <nav className="main-nav" aria-label="主导航">
          <button className={screen === 'sessions' || screen === 'session' || screen === 'run' ? 'nav-link active' : 'nav-link'} onClick={() => setScreen(activeSession ? 'session' : 'sessions')}>工作区</button>
          <button className={screen === 'approvals' ? 'nav-link active' : 'nav-link'} onClick={() => { setError(''); setScreen('approvals'); }}>审批队列{approvals.length > 0 && <span className="nav-count">{approvals.length}</span>}</button>
          <button className={screen === 'observability' ? 'nav-link active' : 'nav-link'} onClick={() => { setError(''); setScreen('observability'); }}>观测</button>
        </nav>
        <div className="topbar-actions">
          <div className={online ? 'connection connected' : 'connection'}><span className="connection-dot" />{online ? `${provider || 'serve'} 已连接` : '未连接'}</div>
          <button className="icon-button theme-toggle" onClick={() => setTheme(theme === 'light' ? 'dark' : 'light')} aria-label={theme === 'light' ? '切换深色模式' : '切换浅色模式'} title={theme === 'light' ? '深色模式' : '浅色模式'}>{theme === 'light' ? '◐' : '☼'}</button>
        </div>
      </header>

      {(error || notice) && <div className={error ? 'flash error-flash' : 'flash success-flash'} role="status"><span>{error || notice}</span><button onClick={() => { setError(''); setNotice(''); }} aria-label="关闭提示">×</button></div>}

      {screen === 'sessions' && (
        <main className="page page-narrow">
          <div className="page-heading">
            <div><p className="eyebrow">WORKSPACE</p><h1>会话</h1><p className="page-subtitle">从最近的运行继续，或开始一段新对话。</p></div>
            <button className="button primary" onClick={createAndShowSession}><span>＋</span> 新建会话</button>
          </div>
          <section className="session-list" aria-label="会话列表">
            {sessions.length === 0 ? (
              <div className="empty-state large-empty"><div className="empty-icon">✦</div><h2>开始你的第一个会话</h2><p>会话与运行来自 serve，可在刷新后继续查看。</p><button className="button primary" onClick={createAndShowSession}>新建会话</button></div>
            ) : sessions.map((session) => (
              <button type="button" className="session-card" key={session.id} onClick={() => openSession(session.id)}>
                <span className="session-icon">◷</span>
                <span className="session-card-main"><strong>会话 {shortId(session.id)}</strong><span>最近更新 {dateLabel(session.latest_run_at || session.updated_at)}</span></span>
                <span className="session-card-meta"><span>{session.run_count} {session.run_count === 1 ? 'run' : 'runs'}</span>{session.latest_run_status?.trim() && <span className={statusClass(session.latest_run_status)}>{statusName(session.latest_run_status)}</span>}</span>
                <span className="chevron">›</span>
              </button>
            ))}
          </section>
          <p className="page-footnote">会话列表和运行状态实时读取自当前 serve。</p>
        </main>
      )}

      {screen === 'session' && (
        <main className="page page-narrow">
          <div className="breadcrumb"><button onClick={showSessions}>‹ 全部会话</button><span>/</span><span>{activeSession ? shortId(activeSession) : '会话'}</span></div>
          <div className="page-heading session-heading"><div><p className="eyebrow">CONVERSATION</p><h1>会话</h1><p className="page-subtitle mono">{activeSession}</p></div><button className="button secondary" onClick={() => refreshRuns(activeSession || '').catch((reason: unknown) => setError(errorText(reason)))}>刷新</button></div>
          <div className="conversation-layout">
            <section className="run-list-card">
              <div className="section-heading"><div><h2>运行记录</h2><p>每条消息对应一次独立运行</p></div><span className="count-pill">{activeRuns.length}</span></div>
              {activeRuns.length === 0 ? <div className="empty-state compact-empty"><div className="empty-icon">⌁</div><strong>还没有运行</strong><span>在下方输入消息开始。</span></div> : (
                <div className="run-list">
                  {[...activeRuns].reverse().map((run, index) => (
                    <button className="run-row" key={run.id} onClick={() => { setSelectedRunId(run.id); setPanel('trace'); setScreen('run'); }}>
                      <span className={`run-indicator ${(run.status || '').toLowerCase()}`} />
                      <span className="run-row-main"><strong>{messages[run.id] || `运行 ${activeRuns.length - index}`}</strong><span className="mono">{shortId(run.id)}</span></span>
                      <span className={statusClass(run.status)}>{statusName(run.status)}</span><span className="chevron">›</span>
                    </button>
                  ))}
                </div>
              )}
            </section>
            <section className="composer-card">
              <div className="composer-heading"><span className="assistant-avatar">✦</span><div><strong>给 Cloud Harness 发消息</strong><span>每条消息都会创建一个新的 run</span></div></div>
              <textarea value={draft} onChange={(event) => setDraft(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); void sendMessage(); } }} placeholder="描述你想完成的任务…" rows={3} aria-label="消息内容" />
              <div className="composer-footer"><span>Enter 发送 · Shift + Enter 换行</span><button className="button primary" disabled={!draft.trim() || sending} onClick={() => void sendMessage()}>{sending ? '正在发送…' : '发送'} <span>↑</span></button></div>
            </section>
          </div>
        </main>
      )}

      {screen === 'run' && selectedRun && (
        <main className="run-page">
          <div className="run-page-main">
          <div className="breadcrumb"><button onClick={showSessions}>‹ 会话</button><span>/</span><button onClick={() => setScreen('session')}>{shortId(selectedRun.session_id)}</button><span>/</span><span>运行详情</span></div>
            <div className="run-title-row"><div><p className="eyebrow">RUN DETAIL</p><h1>运行详情</h1><p className="mono run-id">{selectedRun.id}</p></div><span className={statusClass(selectedRun.status)}>{statusName(selectedRun.status)}</span></div>
            <section className="message-card">
              <div className="message-author"><span className="user-avatar">你</span><div><strong>你的消息</strong><span className="mono">{shortId(selectedRun.id)}</span></div></div>
              <p className="message-body">{localMessage || '此运行来自已有会话，原始用户消息无法通过当前运行列表接口取回。'}</p>
            </section>
            <section className="run-summary-card">
              <div className="section-heading"><div><h2>运行状态</h2><p>自动同步 · 每秒刷新</p></div>{(selectedRun.status === 'RUNNING' || selectedRun.status === 'WAITING') && <button className="button danger-outline" onClick={() => { setActingRunId(selectedRun.id); api.cancel(selectedRun.id).then((run) => { setRuns((previous) => ({ ...previous, [run.session_id]: (previous[run.session_id] || []).map((item) => item.id === run.id ? run : item) })); }).catch((reason: unknown) => setError(errorText(reason))).finally(() => setActingRunId(null)); }} disabled={actingRunId === selectedRun.id}>取消运行</button>}</div>
              <div className="state-line"><span className={statusClass(selectedRun.status)}>{statusName(selectedRun.status)}</span>{selectedRun.wait_reason?.trim() && <span className="wait-chip">{selectedRun.wait_reason}</span>}<span className="version-label">版本 {selectedRun.version}</span></div>
              <RunBudget budget={selectedRun.budget} />
            </section>

            {waiting && <section className="wait-card">
              {waiting.wait_reason === 'APPROVAL' && <><div className="wait-heading"><span className="wait-symbol">⌑</span><div><h2>等待审批</h2><p>确认后，运行会在后台自动继续。</p></div></div>{(waiting.pending_invocation?.warnings ?? []).map((warning) => <p key={warning} className="pending-warning" role="alert">{warning}</p>)}<div className="pending-detail"><strong>{waiting.pending_invocation?.name || waiting.pending_invocation?.tool || '工具调用'}</strong>{waiting.pending_invocation?.arguments !== undefined && <pre>{JSON.stringify(waiting.pending_invocation.arguments, null, 2)}</pre>}</div><div className="wait-actions"><button className="button secondary" onClick={() => void respond(waiting.id, { decision: 'deny' })} disabled={actingRunId === waiting.id}>拒绝</button><button className="button primary" onClick={() => void respond(waiting.id, { decision: 'approve' })} disabled={actingRunId === waiting.id}>{actingRunId === waiting.id ? '提交中…' : '批准并继续'}</button></div></>}
              {waiting.wait_reason === 'USER' && <><div className="wait-heading"><span className="wait-symbol">?</span><div><h2>需要你的回答</h2><p>{waiting.question || '运行正在等待你提供更多信息。'}</p></div></div><div className="answer-row"><input value={answer} onChange={(event) => setAnswer(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter' && answer.trim()) void respond(waiting.id, { answer: answer.trim() }).then(() => setAnswer('')); }} placeholder="输入你的回答…" /><button className="button primary" disabled={!answer.trim() || actingRunId === waiting.id} onClick={() => void respond(waiting.id, { answer: answer.trim() }).then(() => setAnswer(''))}>提交回答</button></div></>}
              {waiting.wait_reason === 'BUDGET' && <><div className="wait-heading"><span className="wait-symbol">↗</span><div><h2>等待补充预算</h2><p>运行进度已保留，补充模型调用后即可继续。</p></div></div><button className="button primary" onClick={() => api.addBudget(waiting.id, 10).then((run) => setRuns((previous) => ({ ...previous, [run.session_id]: (previous[run.session_id] || []).map((item) => item.id === run.id ? run : item) }))).catch((reason: unknown) => setError(errorText(reason)))}>增加 10 次模型调用</button></>}
              {waiting.wait_reason === 'CHILD' && <div className="wait-heading"><span className="wait-symbol">⑂</span><div><h2>子任务运行中</h2><p>子任务结束后，父运行会自动继续。</p></div></div>}
              {waiting.wait_reason === 'RECONCILIATION' && <><div className="wait-heading"><span className="wait-symbol">!</span><div><h2>需要确认工具结果</h2><p>运行暂停在对账步骤，请选择工具调用结果。</p></div></div><div className="wait-actions"><button className="button secondary" onClick={() => void respond(waiting.id, { decision: 'reconcile_failure' })}>标记失败</button><button className="button primary" onClick={() => void respond(waiting.id, { decision: 'reconcile_success' })}>标记成功</button></div></>}
            </section>}

            {selectedRun.final_answer && <section className="answer-card"><div className="answer-heading"><span className="assistant-avatar">✦</span><div><h2>最终回答</h2><span>运行已完成</span></div></div><div className="answer-content">{selectedRun.final_answer}</div></section>}
            {selectedRun.error && <section className="error-card"><strong>运行失败{selectedRun.error.code ? ` · ${selectedRun.error.code}` : ''}</strong><p>{selectedRun.error.message || '发生未知错误。'}</p></section>}

            <section className="process-card">
              <div className="process-heading"><div><p className="eyebrow">OBSERVABILITY</p><h2>运行过程</h2><p>查看编排步骤与持久事件</p></div><div className="tab-switch"><button className={panel === 'trace' ? 'selected' : ''} onClick={() => setPanel('trace')}>执行轨迹</button><button className={panel === 'events' ? 'selected' : ''} onClick={() => setPanel('events')}>事件流 <span>{runEvents.length}</span></button></div></div>
              {panel === 'trace' ? <TracePanel trace={trace} run={selectedRun} /> : <EventPanel events={runEvents} />}
            </section>
          </div>
          <aside className="run-sidebar">
            <div className="sidebar-heading"><div><h2>本会话运行</h2><span>{activeRuns.length} 条记录</span></div><button className="icon-button" aria-label="返回会话" onClick={() => setScreen('session')}>↗</button></div>
            {activeRuns.length === 0 ? <p className="muted">没有运行记录。</p> : [...activeRuns].reverse().map((run) => (
              <button className={`sidebar-run ${run.id === selectedRun.id ? 'current' : ''}`} key={run.id} onClick={() => setSelectedRunId(run.id)}>
                <span className={`run-indicator ${normalizedStatus(run.status).toLowerCase()}`} /><span className="sidebar-run-copy"><strong>{messages[run.id] || shortId(run.id)}</strong><span className={statusClass(run.status)}>{statusName(run.status)}{run.wait_reason?.trim() ? ` · ${run.wait_reason}` : ''}</span></span>
              </button>
            ))}
            <button className="button secondary full-width new-run-button" onClick={() => setScreen('session')}>＋ 发送新消息</button>
          </aside>
        </main>
      )}

      {screen === 'approvals' && (
        <main className="page page-narrow approvals-page">
          <div className="page-heading"><div><p className="eyebrow">HUMAN IN THE LOOP</p><h1>审批队列</h1><p className="page-subtitle">处理等待中的工具授权与用户问题。</p></div><button className="button secondary" onClick={() => refreshQueue().catch((reason: unknown) => setError(errorText(reason)))}>刷新队列</button></div>
          <section className="approval-list" aria-label="待处理项目">
            {approvals.length === 0 ? <div className="empty-state large-empty"><div className="empty-icon">✓</div><h2>队列已清空</h2><p>新的审批请求会出现在这里。</p></div> : approvals.map((item) => (
              <article className="approval-card" key={item.run_id}>
                <div className="approval-card-top"><div className="approval-kind"><span className={item.wait_reason === 'approval' ? 'approval-icon' : 'question-icon'}>{item.wait_reason === 'approval' ? '⌑' : '?'}</span><div><strong>{item.wait_reason === 'approval' ? '工具审批' : '等待你的回答'}</strong><span className="mono">{shortId(item.run_id)}</span></div></div><span className="waiting-time">{elapsedLabel(item.waiting_seconds)}</span></div>
                <p className="approval-summary">{item.summary}</p>
                <div className="approval-card-bottom"><span>会话 <span className="mono">{shortId(item.session_id)}</span> · {dateLabel(item.waiting_since)}</span>{item.wait_reason === 'approval' ? <div className="approval-actions"><button className="button secondary small" disabled={actingRunId === item.run_id} onClick={() => void respond(item.run_id, { decision: 'deny' })}>拒绝</button><button className="button primary small" disabled={actingRunId === item.run_id} onClick={() => void respond(item.run_id, { decision: 'approve' })}>{actingRunId === item.run_id ? '提交中…' : '批准'}</button></div> : <button className="button secondary small" onClick={() => { setActiveSession(item.session_id); setSelectedRunId(item.run_id); refreshRuns(item.session_id).then(() => setScreen('run')).catch((reason: unknown) => setError(errorText(reason))); }}>在运行视图回复 <span>→</span></button>}</div>
              </article>
            ))}
          </section>
          <p className="page-footnote">审批操作直接提交到当前 serve。运行恢复由后端继续处理。</p>
        </main>
      )}

      {screen === 'observability' && <Observability onOpenRun={(runId) => void openRunById(runId)} />}
    </div>
  );
}

function TracePanel({ trace, run }: { trace: TraceView | null; run: RunView }) {
  if (!trace?.run) return <div className="empty-state process-empty"><div className="loader-dot" /><span>{run.status === 'RUNNING' ? '正在收集运行轨迹…' : '暂时没有轨迹数据。'}</span></div>;
  const steps = trace.run.steps || [];
  const children = trace.run.children || [];
  if (!steps.length && !children.length) return <div className="empty-state process-empty"><div className="empty-icon">⌁</div><strong>暂无执行步骤</strong><span>步骤与工具批次会在运行推进后显示。</span></div>;
  return <div className="timeline">
    {steps.map((step, index) => {
      const action = normalizedStatus(step.action);
      return <div className="timeline-item" key={step.id}>
      <span className="timeline-node">{String(index + 1).padStart(2, '0')}</span>
      <div className="timeline-content"><div className="timeline-title"><strong>{actionLabel[action] || action || `步骤 ${index + 1}`}</strong>{action && <span className="action-chip">{action}</span>}</div>
        {(step.batches || []).map((batch) => <div className="batch-card" key={batch.id}><div className="batch-title"><span>工具批次</span>{batch.action?.trim() && <span>{batch.action}</span>}<span className={statusClass(batch.status)}>{statusName(batch.status)}</span></div>
          {(batch.calls || []).map((call) => <div className="tool-call" key={call.id}><span className="tool-order">{String(call.ordinal + 1).padStart(2, '0')}</span><code>{call.name || '未知工具'}</code><span className={statusClass(call.status)}>{statusName(call.status)}</span></div>)}
        </div>)}
      </div>
    </div>;
    })}
    {children.map((child) => <div className="timeline-item child-item" key={child.id}><span className="timeline-node child-node">⑂</span><div className="timeline-content child-card"><strong>子任务</strong><span className="mono">{shortId(child.id)}</span><span className={statusClass(child.status)}>{statusName(child.status)}</span>{child.wait_reason?.trim() && <span className="wait-chip">{child.wait_reason}</span>}</div></div>)}
  </div>;
}

function EventPanel({ events }: { events: HarnessEvent[] }) {
  if (!events.length) return <div className="empty-state process-empty"><div className="empty-icon">⌁</div><strong>等待事件</strong><span>新事件会自动增量拉取。</span></div>;
  return <div className="event-list">{events.map((event, index) => <article className="event-row" key={`${event.seq}-${event.type}-${index}`}><div className="event-marker" /><div className="event-content"><div className="event-title"><span>#{event.seq || '—'}</span><strong>{event.type}</strong></div><pre>{eventSummary(event.data)}</pre></div></article>)}</div>;
}
