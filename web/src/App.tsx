import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from './api';
import type { ApprovalView, HarnessEvent, RunView, SessionView, TraceView } from './api';
import { Composer, PendingTurn, Turn } from './Chat';
import type { Exchange, TurnActions } from './Chat';
import { actionLabels, dayGroup, elapsedLabel, errorText, isActive, shortId, statusName, statusTone, waitLabels } from './format';
import { Icon } from './Icon';
import { Observability } from './Observability';
import { RunBudget } from './RunBudget';
import { deltaText, mergeEvents } from './timeline';
import './App.css';

type View = 'chat' | 'approvals' | 'observability';
type Theme = 'light' | 'dark';

const THEME_KEY = 'cloud-harness.web-theme';
const MESSAGES_KEY = 'cloud-harness.web-messages';
const ANSWERS_KEY = 'cloud-harness.web-answers';
const TITLES_KEY = 'cloud-harness.web-titles';
const SESSION_KEY = 'cloud-harness.web-session';
const SIDEBAR_KEY = 'cloud-harness.web-sidebar';
const NARROW = '(max-width: 860px)';

const suggestions = [
  '介绍一下你能做什么',
  '在工作区写一个 hello.txt，然后列出文件',
  '部署前先问我要用哪个区域',
  '派一个子任务检查工作区里的文件',
];

function readStored<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as T) : fallback;
  } catch {
    return fallback;
  }
}

// State that is also written to localStorage as JSON.
function useStored<T>(key: string, fallback: T) {
  const [value, setValue] = useState<T>(() => readStored(key, fallback));
  useEffect(() => {
    localStorage.setItem(key, JSON.stringify(value));
  }, [key, value]);
  return [value, setValue] as const;
}

const sleep = (ms: number) => new Promise((resolve) => window.setTimeout(resolve, ms));

export default function App() {
  const [view, setView] = useState<View>('chat');
  const [theme, setTheme] = useState<Theme>(() => {
    const saved = localStorage.getItem(THEME_KEY);
    if (saved === 'dark' || saved === 'light') return saved;
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  });
  const [sidebarOpen, setSidebarOpen] = useState(() => localStorage.getItem(SIDEBAR_KEY) !== 'closed' && !window.matchMedia(NARROW).matches);
  const [sessions, setSessions] = useState<SessionView[]>([]);
  const [runs, setRuns] = useState<Record<string, RunView[]>>({});
  const [messages, setMessages] = useStored<Record<string, string>>(MESSAGES_KEY, {});
  const [answers, setAnswers] = useStored<Record<string, Exchange[]>>(ANSWERS_KEY, {});
  const [titles, setTitles] = useStored<Record<string, string>>(TITLES_KEY, {});
  const [activeSession, setActiveSession] = useState<string | null>(() => localStorage.getItem(SESSION_KEY));
  const [pending, setPending] = useState<{ sessionId: string; text: string; runId?: string } | null>(null);
  const [events, setEvents] = useState<Record<string, HarnessEvent[]>>({});
  const [live, setLive] = useState<Record<string, string>>({});
  const [acting, setActing] = useState<Record<string, boolean>>({});
  const [approvals, setApprovals] = useState<ApprovalView[]>([]);
  const [inspected, setInspected] = useState<string | null>(null);
  const [trace, setTrace] = useState<{ runId: string; data: TraceView } | null>(null);
  const [panel, setPanel] = useState<'trace' | 'events'>('trace');
  const [provider, setProvider] = useState('');
  const [online, setOnline] = useState<boolean | null>(null);
  const [error, setError] = useState('');
  const [draft, setDraft] = useState('');

  const lastSeq = useRef<Record<string, number>>({});
  const streamedAt = useRef<Record<string, number>>({});
  const seenVersion = useRef<Record<string, number>>({});
  const liveBuffer = useRef<Record<string, string>>({});
  const liveFrame = useRef(0);
  const threadRef = useRef<HTMLDivElement>(null);
  const stickToBottom = useRef(true);
  const runsRef = useRef(runs);

  // Child runs share the session but are not turns of the conversation.
  const activeRuns = useMemo(() => (activeSession ? (runs[activeSession] || []).filter((run) => !run.parent_id) : []), [activeSession, runs]);
  const latestRun = activeRuns[activeRuns.length - 1] || null;
  const followKey = useMemo(() => activeRuns.filter(isActive).map((run) => run.id).join(','), [activeRuns]);
  const sending = pending !== null;
  const optimistic = pending && pending.sessionId === activeSession && !activeRuns.some((run) => run.id === pending.runId) ? pending : null;
  const busy = sending || (latestRun?.status === 'RUNNING') || (latestRun?.status === 'WAITING' && latestRun.wait_reason !== 'USER');
  const answering = !sending && latestRun?.status === 'WAITING' && latestRun.wait_reason === 'USER' ? latestRun : null;

  const upsertRun = useCallback((run: RunView) => {
    setRuns((previous) => {
      const list = previous[run.session_id] || [];
      const index = list.findIndex((item) => item.id === run.id);
      if (index >= 0 && list[index].version > run.version) return previous;
      const next = [...list];
      if (index >= 0) next[index] = run;
      else next.push(run);
      return { ...previous, [run.session_id]: next };
    });
  }, []);

  const refreshSessions = useCallback(async () => {
    setSessions(await api.listSessions());
  }, []);

  const refreshRuns = useCallback(async (sessionId: string) => {
    const rows = await api.listRuns(sessionId);
    setRuns((previous) => ({ ...previous, [sessionId]: rows }));
    return rows;
  }, []);

  const refreshRun = useCallback(async (runId: string) => {
    const run = await api.getRun(runId);
    upsertRun(run);
    return run;
  }, [upsertRun]);

  const refreshApprovals = useCallback(async () => {
    setApprovals(await api.listApprovals());
  }, []);

  const flushLive = useCallback(() => {
    liveFrame.current = 0;
    const buffered = liveBuffer.current;
    liveBuffer.current = {};
    setLive((previous) => {
      const next = { ...previous };
      for (const [runId, text] of Object.entries(buffered)) next[runId] = (next[runId] || '') + text;
      return next;
    });
  }, []);

  const onEvent = useCallback((runId: string, event: HarnessEvent) => {
    streamedAt.current[runId] = Date.now();
    if (event.seq <= 0) {
      // Stream deltas are not stored and carry no id. They are drawn once per frame.
      const text = event.type === 'stream.delta' ? deltaText(event.data) : '';
      if (!text) return;
      liveBuffer.current[runId] = (liveBuffer.current[runId] || '') + text;
      if (!liveFrame.current) liveFrame.current = window.requestAnimationFrame(flushLive);
      return;
    }
    lastSeq.current[runId] = Math.max(lastSeq.current[runId] || 0, event.seq);
    setEvents((previous) => {
      const current = previous[runId] || [];
      const merged = mergeEvents(current, [event]);
      return merged === current ? previous : { ...previous, [runId]: merged };
    });
    // Text streamed before a tool call belongs to that step, not to the answer.
    if (event.type === 'model.completed' && !event.data.includes('"ANSWER"')) {
      delete liveBuffer.current[runId];
      setLive((previous) => (previous[runId] ? { ...previous, [runId]: '' } : previous));
    }
  }, [flushLive]);

  const pullEvents = useCallback(async (runId: string) => {
    const fresh = await api.fetchEvents(runId, lastSeq.current[runId] || 0);
    for (const event of fresh) if (event.seq > 0) onEvent(runId, event);
  }, [onEvent]);

  // Loads the whole event log of a finished run. serve closes that stream itself.
  const loadEvents = useCallback((run: RunView) => {
    if (isActive(run)) return;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 8000);
    api.streamEvents(run.id, lastSeq.current[run.id] || 0, (event) => onEvent(run.id, event), controller.signal)
      .catch(() => undefined)
      .finally(() => window.clearTimeout(timeout));
  }, [onEvent]);

  useEffect(() => {
    runsRef.current = runs;
  }, [runs]);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem(THEME_KEY, theme);
  }, [theme]);

  // The sidebar is an overlay on a narrow window: it closes when the window
  // becomes narrow and returns to the saved choice when it widens again.
  useEffect(() => {
    const query = window.matchMedia(NARROW);
    const onChange = () => setSidebarOpen(!query.matches && localStorage.getItem(SIDEBAR_KEY) !== 'closed');
    query.addEventListener('change', onChange);
    return () => query.removeEventListener('change', onChange);
  }, []);

  const toggleSidebar = (open: boolean) => {
    setSidebarOpen(open);
    if (!window.matchMedia(NARROW).matches) localStorage.setItem(SIDEBAR_KEY, open ? 'open' : 'closed');
  };

  useEffect(() => {
    if (activeSession) localStorage.setItem(SESSION_KEY, activeSession);
    else localStorage.removeItem(SESSION_KEY);
  }, [activeSession]);

  // Connection state, the session list, and the approval badge.
  useEffect(() => {
    let stopped = false;
    const check = () => {
      api.health().then((health) => {
        if (stopped) return;
        setOnline(true);
        setProvider(health.provider);
      }).catch(() => {
        if (!stopped) setOnline(false);
      });
      refreshApprovals().catch(() => undefined);
    };
    check();
    api.listSessions().then(setSessions).catch((reason: unknown) => setError(`会话加载失败：${errorText(reason)}`));
    const fast = window.setInterval(() => refreshApprovals().catch(() => undefined), 3000);
    const slow = window.setInterval(() => {
      check();
      refreshSessions().catch(() => undefined);
    }, 8000);
    return () => {
      stopped = true;
      window.clearInterval(fast);
      window.clearInterval(slow);
    };
  }, [refreshApprovals, refreshSessions]);

  useEffect(() => {
    if (!activeSession) return;
    const sessionId = activeSession;
    stickToBottom.current = true;
    api.listRuns(sessionId).then((rows) => setRuns((previous) => ({ ...previous, [sessionId]: rows }))).catch((reason: unknown) => {
      // A session id saved by an earlier serve database no longer exists.
      setActiveSession((current) => (current === sessionId ? null : current));
      if (!/not.?found|404/i.test(errorText(reason))) setError(`对话加载失败：${errorText(reason)}`);
    });
  }, [activeSession]);

  // Follow every run of the open session that has not finished: one event
  // stream each, plus a poll of the run view once a second.
  useEffect(() => {
    if (!followKey) return;
    const ids = followKey.split(',');
    const controller = new AbortController();
    const follow = async (runId: string) => {
      while (!controller.signal.aborted) {
        try {
          await api.streamEvents(runId, lastSeq.current[runId] || 0, (event) => onEvent(runId, event), controller.signal);
        } catch {
          // Reconnect below with Last-Event-ID.
        }
        if (controller.signal.aborted) return;
        const run = await refreshRun(runId).catch(() => null);
        if (run && !isActive(run)) return;
        await sleep(600);
      }
    };
    for (const runId of ids) void follow(runId);
    const timer = window.setInterval(() => {
      for (const runId of ids) {
        refreshRun(runId).then((run) => {
          const changed = seenVersion.current[runId] !== run.version;
          seenVersion.current[runId] = run.version;
          if (changed) refreshApprovals().catch(() => undefined);
          // A proxy that buffers the stream would hide events until the run ends.
          if (changed && Date.now() - (streamedAt.current[runId] || 0) > 2000) pullEvents(runId).catch(() => undefined);
        }).catch(() => undefined);
      }
    }, 1000);
    return () => {
      controller.abort();
      window.clearInterval(timer);
      refreshSessions().catch(() => undefined);
      refreshApprovals().catch(() => undefined);
    };
  }, [followKey, onEvent, pullEvents, refreshApprovals, refreshRun, refreshSessions]);

  // Name a session after its first message when this browser has it.
  useEffect(() => {
    if (!activeSession || titles[activeSession]) return;
    const first = activeRuns[0];
    const text = first && (first.message || messages[first.id]);
    if (text) setTitles((previous) => ({ ...previous, [activeSession]: text.slice(0, 80) }));
  }, [activeRuns, activeSession, messages, setTitles, titles]);

  const inspectedRun = useMemo(() => {
    if (!inspected) return null;
    for (const list of Object.values(runs)) {
      const found = list.find((run) => run.id === inspected);
      if (found) return found;
    }
    return null;
  }, [inspected, runs]);
  const inspectedVersion = inspectedRun?.version;
  useEffect(() => {
    if (!inspected) return;
    let cancelled = false;
    api.getTrace(inspected).then((data) => {
      if (!cancelled) setTrace({ runId: inspected, data });
    }).catch((reason: unknown) => {
      if (!cancelled) setError(`轨迹加载失败：${errorText(reason)}`);
    });
    return () => { cancelled = true; };
  }, [inspected, inspectedVersion]);

  useEffect(() => {
    if (view !== 'approvals') return;
    api.listApprovals().then(setApprovals).catch((reason: unknown) => setError(`审批队列加载失败：${errorText(reason)}`));
  }, [view]);

  // Keep the newest content in view unless the reader scrolled up.
  useEffect(() => {
    const node = threadRef.current;
    if (node && stickToBottom.current) node.scrollTop = node.scrollHeight;
  }, [activeRuns, events, live, optimistic]);

  const openSession = (sessionId: string | null) => {
    setError('');
    setView('chat');
    setInspected(null);
    setActiveSession(sessionId);
    if (window.matchMedia(NARROW).matches) setSidebarOpen(false);
  };

  const showPage = (page: View) => {
    setView(page);
    if (window.matchMedia(NARROW).matches) setSidebarOpen(false);
  };

  const send = async (text: string) => {
    setError('');
    setDraft('');
    let sessionId = activeSession;
    try {
      if (!sessionId) {
        sessionId = (await api.createSession()).id;
        const now = new Date().toISOString();
        const created = { id: sessionId, created_at: now, updated_at: now, run_count: 0 };
        setSessions((previous) => [created, ...previous]);
        setRuns((previous) => ({ ...previous, [created.id]: [] }));
        setActiveSession(sessionId);
      }
    } catch (reason) {
      setDraft(text);
      setError(`新建对话失败：${errorText(reason)}`);
      return;
    }
    const target = sessionId;
    const known = new Set((runsRef.current[target] || []).map((run) => run.id));
    let adopted = false;
    const adopt = (run: RunView) => {
      adopted = true;
      setMessages((previous) => ({ ...previous, [run.id]: text }));
      upsertRun(run);
      setPending((current) => (current ? { ...current, runId: run.id } : current));
    };
    setTitles((previous) => (previous[target] ? previous : { ...previous, [target]: text.slice(0, 80) }));
    setPending({ sessionId: target, text });
    stickToBottom.current = true;
    // Runs this browser has not loaded yet, such as child runs, are not the new one.
    await api.listRuns(target).then((rows) => rows.forEach((run) => known.add(run.id))).catch(() => undefined);
    // serve may answer the POST only when the run stops. Until then the new
    // run is found in the session's run list so its events can be followed.
    const finder = window.setInterval(() => {
      if (adopted) return;
      api.listRuns(target).then((rows) => {
        const fresh = rows.find((run) => !known.has(run.id) && !run.parent_id);
        if (fresh && !adopted) adopt(fresh);
      }).catch(() => undefined);
    }, 500);
    try {
      adopt(await api.createRun(target, text));
    } catch (reason) {
      if (adopted) {
        refreshRuns(target).catch(() => undefined);
      } else {
        setDraft(text);
        setError(`发送失败：${errorText(reason)}`);
      }
    } finally {
      window.clearInterval(finder);
      setPending(null);
      refreshSessions().catch(() => undefined);
    }
  };

  const respond = async (runId: string, body: { decision?: string; answer?: string }) => {
    setError('');
    setActing((previous) => ({ ...previous, [runId]: true }));
    try {
      upsertRun(await api.respond(runId, body));
    } catch (reason) {
      setError(`提交失败：${errorText(reason)}`);
    } finally {
      setActing((previous) => ({ ...previous, [runId]: false }));
      refreshApprovals().catch(() => undefined);
      refreshRun(runId).catch(() => undefined);
    }
  };

  const addBudget = async (runId: string) => {
    setActing((previous) => ({ ...previous, [runId]: true }));
    try {
      upsertRun(await api.addBudget(runId, 10));
    } catch (reason) {
      setError(`补充预算失败：${errorText(reason)}`);
    } finally {
      setActing((previous) => ({ ...previous, [runId]: false }));
    }
  };

  const answer = (run: RunView, text: string) => {
    setDraft('');
    stickToBottom.current = true;
    setAnswers((previous) => ({ ...previous, [run.id]: [...(previous[run.id] || []), { q: run.question || '', a: text }] }));
    void respond(run.id, { answer: text });
  };

  const stop = async () => {
    if (!latestRun || !isActive(latestRun)) return;
    setActing((previous) => ({ ...previous, [latestRun.id]: true }));
    try {
      upsertRun(await api.cancel(latestRun.id));
    } catch (reason) {
      setError(`取消失败：${errorText(reason)}`);
    } finally {
      setActing((previous) => ({ ...previous, [latestRun.id]: false }));
    }
  };

  const openRun = async (runId: string) => {
    try {
      const run = await api.getRun(runId);
      openSession(run.session_id);
      upsertRun(run);
      setInspected(run.id);
    } catch (reason) {
      setError(`无法打开运行：${errorText(reason)}`);
    }
  };

  const actions: TurnActions = {
    respond: (runId, body) => void respond(runId, body),
    addBudget: (runId) => void addBudget(runId),
    inspect: (runId) => {
      setInspected(runId);
      const run = activeRuns.find((item) => item.id === runId);
      if (run) loadEvents(run);
    },
    loadEvents,
  };

  const groups = useMemo(() => {
    const shown = sessions.filter((session) => session.run_count > 0 || session.id === activeSession);
    const sorted = [...shown].sort((a, b) => (b.latest_run_at || b.updated_at).localeCompare(a.latest_run_at || a.updated_at));
    const out: { label: string; items: SessionView[] }[] = [];
    for (const session of sorted) {
      const label = dayGroup(session.latest_run_at || session.updated_at);
      const group = out.find((item) => item.label === label);
      if (group) group.items.push(session);
      else out.push({ label, items: [session] });
    }
    return out;
  }, [activeSession, sessions]);

  const titleOf = (sessionId: string) => titles[sessionId] || `对话 ${shortId(sessionId)}`;
  const empty = view === 'chat' && activeRuns.length === 0 && !optimistic;
  const composer = (
    <Composer
      value={draft}
      onChange={setDraft}
      onSubmit={() => {
        const text = draft.trim();
        if (!text) return;
        if (answering) answer(answering, text);
        else void send(text);
      }}
      onStop={latestRun && isActive(latestRun) ? () => void stop() : undefined}
      busy={Boolean(busy)}
      stopping={latestRun ? acting[latestRun.id] : false}
      placeholder={answering ? '回答 agent 的问题…' : '给 Cloud Harness 发消息…'}
      hint={online === false ? '未连接到 serve' : provider ? `模型：${provider}` : ''}
      autoFocus={view === 'chat'}
    />
  );

  return (
    <div className={`app ${sidebarOpen ? 'with-sidebar' : ''} ${inspectedRun && view === 'chat' ? 'with-inspector' : ''}`}>
      {sidebarOpen && <button type="button" className="scrim" aria-label="关闭侧栏" onClick={() => toggleSidebar(false)} />}
      <aside className="sidebar" aria-label="对话列表" hidden={!sidebarOpen}>
        <div className="sidebar-top">
          <button type="button" className="brand" onClick={() => openSession(null)}><span className="brand-mark" />Cloud Harness</button>
          <button type="button" className="icon-button" aria-label="收起侧栏" title="收起侧栏" onClick={() => toggleSidebar(false)}><Icon name="sidebar" size={18} /></button>
        </div>
        <nav className="sidebar-nav">
          <button type="button" className={`nav-item new-chat ${view === 'chat' && !activeSession ? 'current' : ''}`} onClick={() => openSession(null)}><Icon name="compose" />新对话</button>
          <button type="button" className={`nav-item ${view === 'approvals' ? 'current' : ''}`} onClick={() => showPage('approvals')}><Icon name="shield" />审批{approvals.length > 0 && <span className="badge">{approvals.length}</span>}</button>
          <button type="button" className={`nav-item ${view === 'observability' ? 'current' : ''}`} onClick={() => showPage('observability')}><Icon name="activity" />观测</button>
        </nav>
        <div className="session-list">
          {groups.length === 0 && <p className="sidebar-empty">还没有对话。</p>}
          {groups.map((group) => (
            <section key={group.label}>
              <h2>{group.label}</h2>
              {group.items.map((session) => (
                <button type="button" key={session.id} data-session-id={session.id} className={`session-item ${view === 'chat' && session.id === activeSession ? 'current' : ''}`} onClick={() => openSession(session.id)} title={titleOf(session.id)}>
                  <span className="session-title">{titleOf(session.id)}</span>
                  {session.latest_run_status === 'RUNNING' && <span className="spinner" aria-label="运行中" />}
                  {session.latest_run_status === 'WAITING' && <span className="dot tone-wait" aria-label="等待处理" />}
                  {session.latest_run_status === 'FAILED' && <span className="dot tone-fail" aria-label="失败" />}
                </button>
              ))}
            </section>
          ))}
        </div>
        <div className="sidebar-foot">
          <span className="connection" title={online === false ? '无法连接 serve' : '已连接 serve'}>
            <span className={`dot ${online === false ? 'tone-fail' : online ? 'tone-ok' : 'tone-idle'}`} />
            {online === false ? '未连接' : provider || '连接中…'}
          </span>
          <button type="button" className="icon-button theme-toggle" aria-label={theme === 'dark' ? '切换到浅色' : '切换到深色'} title={theme === 'dark' ? '切换到浅色' : '切换到深色'} onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}><Icon name={theme === 'dark' ? 'sun' : 'moon'} size={18} /></button>
        </div>
      </aside>

      <main className="main">
        <header className="topbar">
          {!sidebarOpen && <>
            <button type="button" className="icon-button" aria-label="展开侧栏" title="展开侧栏" onClick={() => toggleSidebar(true)}><Icon name="sidebar" size={18} /></button>
            <button type="button" className="icon-button" aria-label="新对话" title="新对话" onClick={() => openSession(null)}><Icon name="compose" size={18} /></button>
          </>}
          <h1>{view === 'approvals' ? '审批' : view === 'observability' ? '观测' : activeSession ? titleOf(activeSession) : '新对话'}</h1>
          {view === 'chat' && latestRun && <>
            <span className={`chip tone-${statusTone(latestRun.status)}`}>{latestRun.status === 'WAITING' ? waitLabels[latestRun.wait_reason || ''] || statusName(latestRun.status) : statusName(latestRun.status)}</span>
            <button type="button" className={`icon-button ${inspectedRun ? 'current' : ''}`} aria-label="运行详情" title="运行详情" onClick={() => (inspectedRun ? setInspected(null) : actions.inspect(latestRun.id))}><Icon name="panel" size={18} /></button>
          </>}
        </header>

        {error && <div className="flash" role="alert"><Icon name="alert" /><span>{error}</span><button type="button" className="icon-button" aria-label="关闭提示" onClick={() => setError('')}><Icon name="close" size={14} /></button></div>}

        {view === 'chat' && empty && (
          <div className="welcome">
            <div className="welcome-inner">
              <span className="brand-mark large" />
              <h2>今天想让 agent 做什么？</h2>
              {composer}
              <div className="suggestions">
                {suggestions.map((text) => <button type="button" key={text} onClick={() => setDraft(text)}>{text}</button>)}
              </div>
            </div>
          </div>
        )}

        {view === 'chat' && !empty && <>
          <div className="thread" ref={threadRef} onScroll={(event) => {
            const node = event.currentTarget;
            stickToBottom.current = node.scrollHeight - node.scrollTop - node.clientHeight < 120;
          }}>
            <div className="thread-inner">
              {activeRuns.map((run) => (
                <Turn key={run.id} run={run} message={messages[run.id]} exchanges={answers[run.id] || []} events={events[run.id] || []} live={live[run.id] || ''} acting={Boolean(acting[run.id])} actions={actions} />
              ))}
              {optimistic && <PendingTurn text={optimistic.text} />}
            </div>
          </div>
          <div className="dock"><div className="dock-inner">{composer}<p className="dock-note">agent 的工具调用受策略和审批约束；回答可能有误，请核对关键结果。</p></div></div>
        </>}

        {view === 'approvals' && (
          <div className="page-scroll">
            <div className="page-inner">
              <div className="page-head">
                <div><h2>待处理</h2><p>等待批准的工具调用和 agent 提出的问题。</p></div>
                <button type="button" className="button" onClick={() => refreshApprovals().catch((reason: unknown) => setError(errorText(reason)))}><Icon name="refresh" size={14} />刷新</button>
              </div>
              {approvals.length === 0
                ? <div className="page-empty"><Icon name="check" size={22} /><strong>没有待处理的请求</strong><span>新的审批和提问会出现在这里。</span></div>
                : approvals.map((item) => (
                  <article className="approval-card" key={item.run_id} data-run-id={item.run_id}>
                    <div className="approval-top">
                      <Icon name={item.wait_reason === 'approval' ? 'shield' : 'question'} />
                      <strong>{item.wait_reason === 'approval' ? '工具审批' : '等待你的回答'}</strong>
                      <span className="approval-wait">{elapsedLabel(item.waiting_seconds)}</span>
                    </div>
                    <p className="approval-summary">{item.summary}</p>
                    <div className="approval-bottom">
                      <button type="button" className="link-button" onClick={() => openSession(item.session_id)}>{titleOf(item.session_id)}</button>
                      {item.wait_reason === 'approval'
                        ? <div className="approval-actions">
                          <button type="button" className="button" disabled={acting[item.run_id]} onClick={() => void respond(item.run_id, { decision: 'deny' })}>拒绝</button>
                          <button type="button" className="button primary" disabled={acting[item.run_id]} onClick={() => void respond(item.run_id, { decision: 'approve' })}>{acting[item.run_id] ? '提交中…' : '批准'}</button>
                        </div>
                        : <button type="button" className="button" onClick={() => openSession(item.session_id)}>去对话里回答</button>}
                    </div>
                  </article>
                ))}
            </div>
          </div>
        )}

        {view === 'observability' && <div className="page-scroll"><Observability onOpenRun={(runId) => void openRun(runId)} /></div>}
      </main>

      {inspectedRun && view === 'chat' && (
        <aside className="inspector" aria-label="运行详情">
          <div className="inspector-head">
            <div><h2>运行详情</h2><code>{inspectedRun.id}</code></div>
            <button type="button" className="icon-button" aria-label="关闭运行详情" onClick={() => setInspected(null)}><Icon name="close" size={18} /></button>
          </div>
          <div className="inspector-body">
            <div className="inspector-status">
              <span className={`chip tone-${statusTone(inspectedRun.status)}`}>{statusName(inspectedRun.status)}</span>
              {inspectedRun.wait_reason?.trim() && <span className="chip tone-wait">{waitLabels[inspectedRun.wait_reason] || inspectedRun.wait_reason}</span>}
            </div>
            <RunBudget budget={inspectedRun.budget} />
            <div className="tabs" role="tablist">
              <button type="button" role="tab" aria-selected={panel === 'trace'} onClick={() => setPanel('trace')}>执行步骤</button>
              <button type="button" role="tab" aria-selected={panel === 'events'} onClick={() => setPanel('events')}>事件 <span>{(events[inspectedRun.id] || []).length}</span></button>
            </div>
            {panel === 'trace'
              ? <TracePanel trace={trace?.runId === inspectedRun.id ? trace.data : null} />
              : <EventPanel events={events[inspectedRun.id] || []} />}
          </div>
        </aside>
      )}
    </div>
  );
}

function TracePanel({ trace }: { trace: TraceView | null }) {
  if (!trace?.run) return <p className="panel-empty">正在加载执行步骤…</p>;
  const steps = trace.run.steps || [];
  const children = trace.run.children || [];
  if (!steps.length && !children.length) return <p className="panel-empty">还没有执行步骤。</p>;
  return (
    <ol className="trace">
      {steps.map((step, index) => {
        const action = step.action?.trim() || (step.batches || []).find((batch) => batch.action?.trim())?.action || '';
        return (
          <li key={step.id}>
            <div className="trace-title"><span className="trace-index">{index + 1}</span><strong>{actionLabels[action] || action || `步骤 ${index + 1}`}</strong></div>
            {(step.batches || []).flatMap((batch) => (batch.calls || []).map((call) => (
              <div className="trace-call" key={call.id}><code>{call.name || '未知工具'}</code><span className={`chip tone-${statusTone(call.status)}`}>{statusName(call.status)}</span></div>
            )))}
          </li>
        );
      })}
      {children.map((child) => (
        <li key={child.id}>
          <div className="trace-title"><span className="trace-index"><Icon name="branch" size={12} /></span><strong>子任务</strong><code>{shortId(child.id)}</code><span className={`chip tone-${statusTone(child.status)}`}>{statusName(child.status)}</span></div>
        </li>
      ))}
    </ol>
  );
}

function prettyData(data: string) {
  try {
    return JSON.stringify(JSON.parse(data), null, 2);
  } catch {
    return data || '—';
  }
}

function EventPanel({ events }: { events: HarnessEvent[] }) {
  if (!events.length) return <p className="panel-empty">还没有事件。</p>;
  return (
    <ol className="events">
      {events.map((event) => (
        <li key={event.seq}>
          <details>
            <summary><span className="event-seq">#{event.seq}</span><strong>{event.type}</strong></summary>
            <pre>{prettyData(event.data)}</pre>
          </details>
        </li>
      ))}
    </ol>
  );
}
