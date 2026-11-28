import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api, isNotFound } from './api';
import type { ModelOption, SessionPreferences, BudgetExtension, HarnessEvent, RunView, SessionMessage, SessionView, TraceView } from './api';
import { Composer, PendingTurn } from './Chat';
import { skillInvocation } from './skills';
import type { TurnActions } from './Chat';
import { actionLabels, dayGroup, errorText, isActive, shortId, statusName, statusTone, waitLabels } from './format';
import { Icon } from './Icon';
import { Observability } from './Observability';
import { RunBudget } from './RunBudget';
import { syncSessionMessages, type TranscriptSnapshot } from './history';
import { SessionHistory } from './SessionHistory';
import { Settings } from './Settings';
import { approvalModeLabel } from './approval';
import { liveText, mergeEvents } from './timeline';

type View = 'chat' | 'observability' | 'settings';
type Theme = 'light' | 'dark';

const THEME_KEY = 'cloud-harness.web-theme';
const TITLES_KEY = 'cloud-harness.web-titles';
const SESSION_KEY = 'cloud-harness.web-session';
const SIDEBAR_KEY = 'cloud-harness.web-sidebar';
const DRAFT_PREFERENCES_KEY = 'cloud-harness.web-draft-preferences';
const DEFAULT_PREFERENCES: SessionPreferences = { model_id: 'default', approval_mode: 'ask' };
const NARROW = '(max-width: 860px)';

const suggestions = [
  { label: '了解助手能力', icon: 'spark', prompt: '介绍一下你能做什么' },
  { label: '创建工作区文件', icon: 'folder-edit', prompt: '在工作区写一个 hello.txt，然后列出文件' },
  { label: '规划一次部署', icon: 'question', prompt: '部署前先问我要用哪个区域' },
  { label: '委派文件检查', icon: 'branch', prompt: '派一个子任务检查工作区里的文件' },
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

// Coalesce updates while a sync is in flight. Each selected session owns its cursor.
function useSessionMessages(sessionId: string | null, revision: string, onMissing: (sessionId: string) => void) {
  const [state, setState] = useState<{ sessionId: string | null; messages: SessionMessage[]; loading: boolean; error: string }>({ sessionId: null, messages: [], loading: false, error: '' });
  const current = useRef<{ sync: () => Promise<void> } | null>(null);
  const refresh = useCallback(() => { void current.current?.sync(); }, []);

  useEffect(() => {
    if (!sessionId) return;
    const controller = new AbortController();
    let snapshot: TranscriptSnapshot = { cursor: '0.0', messages: [] };
    const request = {
      syncing: false,
      again: false,
      sync: async () => {
        if (controller.signal.aborted || current.current !== request) return;
        if (request.syncing) {
          request.again = true;
          return;
        }
        request.syncing = true;
        do {
          request.again = false;
          setState((previous) => ({ sessionId, messages: previous.sessionId === sessionId ? previous.messages : [], loading: true, error: '' }));
          try {
            const next = await syncSessionMessages(sessionId, snapshot, controller.signal);
            if (controller.signal.aborted || current.current !== request) return;
            snapshot = next;
            setState({ sessionId, messages: next.messages, loading: request.again, error: '' });
          } catch (reason) {
            if (controller.signal.aborted || current.current !== request) return;
            if (isNotFound(reason)) {
              onMissing(sessionId);
              return;
            }
            setState((previous) => ({ ...previous, loading: request.again, error: errorText(reason) }));
          }
        } while (request.again && !controller.signal.aborted);
        request.syncing = false;
      },
    };
    current.current = request;
    const onFocus = () => { void request.sync(); };
    const onVisible = () => { if (document.visibilityState === 'visible') onFocus(); };
    window.addEventListener('focus', onFocus);
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      controller.abort();
      if (current.current === request) current.current = null;
      window.removeEventListener('focus', onFocus);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [sessionId, onMissing]);

  useEffect(refresh, [sessionId, revision, refresh]);

  // Mask the previous session during the render before effect cleanup, too.
  const matches = state.sessionId === sessionId;
  return { messages: matches ? state.messages : [], loading: Boolean(sessionId) && (!matches || state.loading), error: matches ? state.error : '', refresh };
}

export default function App() {
  const [view, setView] = useState<View>('chat');
  const [theme, setTheme] = useState<Theme>(() => {
    const saved = localStorage.getItem(THEME_KEY);
    if (saved === 'dark' || saved === 'light') return saved;
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  });
  const [sidebarOpen, setSidebarOpen] = useState(() => localStorage.getItem(SIDEBAR_KEY) !== 'closed' && !window.matchMedia(NARROW).matches);
  const [sessions, setSessions] = useState<SessionView[]>([]);
  const [sessionQuery, setSessionQuery] = useState('');
  const [runs, setRuns] = useState<Record<string, RunView[]>>({});
  const [titles, setTitles] = useStored<Record<string, string>>(TITLES_KEY, {});
  const [activeSession, setActiveSession] = useState<string | null>(() => new URLSearchParams(window.location.search).get('session') || localStorage.getItem(SESSION_KEY));
  const [pending, setPending] = useState<{ sessionId: string; text: string; runId?: string } | null>(null);
  const [events, setEvents] = useState<Record<string, HarnessEvent[]>>({});
  const [live, setLive] = useState<Record<string, string>>({});
  const [acting, setActing] = useState<Record<string, boolean>>({});
  const [models, setModels] = useState<ModelOption[]>([]);
  const [modelsLoading, setModelsLoading] = useState(true);
  const [modelsError, setModelsError] = useState('');
  const [draftPreferences, setDraftPreferences] = useStored<SessionPreferences>(DRAFT_PREFERENCES_KEY, DEFAULT_PREFERENCES);
  const [loadedPreferences, setLoadedPreferences] = useState<{ sessionId: string; value: SessionPreferences } | null>(null);
  const [savingPreferences, setSavingPreferences] = useState(false);
  const preferences = activeSession ? (loadedPreferences?.sessionId === activeSession ? loadedPreferences.value : DEFAULT_PREFERENCES) : draftPreferences;
  const preferencesBusy = savingPreferences || Boolean(activeSession && loadedPreferences?.sessionId !== activeSession);
  const [inspected, setInspected] = useState<string | null>(null);
  const [trace, setTrace] = useState<{ runId: string; data: TraceView } | null>(null);
  const [panel, setPanel] = useState<'trace' | 'events'>('trace');
  const [online, setOnline] = useState<boolean | null>(null);
  const [error, setError] = useState('');
  const [draft, setDraft] = useState('');
  const [answerRun, setAnswerRun] = useState<string | null>(null);

  const activeSessionRef = useRef(activeSession);
  const selectSession = useCallback((sessionId: string | null) => {
    // Save inside the selection action: a reload must not beat a passive
    // effect, and an old effect must not overwrite a newer selection.
    if (sessionId) localStorage.setItem(SESSION_KEY, sessionId);
    else localStorage.removeItem(SESSION_KEY);
    if (activeSessionRef.current !== sessionId) setLoadedPreferences(null);
    activeSessionRef.current = sessionId;
    setActiveSession(sessionId);
  }, []);
  const forgetMissingSession = useCallback((sessionId: string) => {
    if (activeSessionRef.current !== sessionId) return;
    selectSession(null);
    setError('');
  }, [selectSession]);

  const lastSeq = useRef<Record<string, number>>({});
  const streamedAt = useRef<Record<string, number>>({});
  const responding = useRef(new Set<string>());
  const seenVersion = useRef<Record<string, number>>({});
  const liveNow = useRef<Record<string, string>>({});
  const liveFrame = useRef(0);
  const threadRef = useRef<HTMLDivElement>(null);
  const stickToBottom = useRef(true);
  const [awayFromBottom, setAwayFromBottom] = useState(false);
  const focusRun = useRef<string | null>(null);
  const runsRef = useRef(runs);
  const runRequests = useRef<Record<string, number>>({});

  // History is never filtered by a root run; children retain their own facts
  // and current waits. Prefer the newest root for the main stop/status control.
  const activeRuns = useMemo(() => activeSession ? (runs[activeSession] || []) : [], [activeSession, runs]);
  const sessionSummary = sessions.find((session) => session.id === activeSession);
  const sessionRevision = sessionSummary ? `${sessionSummary.updated_at}:${sessionSummary.run_count}:${sessionSummary.latest_run_at}:${sessionSummary.latest_run_status}` : '';
  const latestRun = [...activeRuns].reverse().find((run) => !run.parent_id) || activeRuns[activeRuns.length - 1] || null;
  const followKey = useMemo(() => activeRuns.filter(isActive).map((run) => run.id).join(','), [activeRuns]);
  const historyRevision = `${sessionRevision}|${activeRuns.map((run) => `${run.id}:${run.version}:${run.status}:${events[run.id]?.at(-1)?.seq || 0}`).join('|')}`;
  const history = useSessionMessages(activeSession, historyRevision, forgetMissingSession);
  const sending = pending !== null;
  const optimistic = pending && pending.sessionId === activeSession && !history.messages.some((message) => message.kind === 'input' && message.run_id === pending.runId) ? pending : null;
  // A question can also wait on a run that is no longer the newest of the session.
  const asking = useMemo(() => {
    const waiting = activeRuns.filter((run) => run.status === 'WAITING' && run.wait_reason === 'USER');
    return waiting.find((run) => run.id === answerRun) || waiting[waiting.length - 1] || null;
  }, [activeRuns, answerRun]);
  const answering = sending ? null : asking;
  const busy = preferencesBusy || sending || (answering ? Boolean(acting[answering.id]) : activeRuns.some(isActive));

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
    const identity = (runRequests.current[sessionId] || 0) + 1;
    runRequests.current[sessionId] = identity;
    const known = new Set((runsRef.current[sessionId] || []).map((run) => run.id));
    const rows = await api.listRuns(sessionId);
    if (runRequests.current[sessionId] !== identity) return rows;
    setRuns((previous) => {
      const existing = previous[sessionId] || [];
      const merged = rows.map((run) => {
        const cached = existing.find((item) => item.id === run.id);
        return cached && cached.version > run.version ? cached : run;
      });
      // Do not drop a run discovered while this list request was in flight.
      merged.push(...existing.filter((run) => !known.has(run.id) && !rows.some((row) => row.id === run.id)));
      return { ...previous, [sessionId]: merged };
    });
    return rows;
  }, []);

  const refreshRun = useCallback(async (runId: string) => {
    const run = await api.getRun(runId);
    upsertRun(run);
    return run;
  }, [upsertRun]);

  const refreshModels = useCallback(async () => {
    setModelsLoading(true); setModelsError('');
    try { setModels(await api.listModels()); }
    catch (reason) { setModelsError('模型加载失败：' + errorText(reason)); }
    finally { setModelsLoading(false); }
  }, []);
  useEffect(() => {
    let cancelled = false;
    api.listModels().then((items) => { if (!cancelled) setModels(items); })
      .catch((reason: unknown) => { if (!cancelled) setModelsError('模型加载失败：' + errorText(reason)); })
      .finally(() => { if (!cancelled) setModelsLoading(false); });
    return () => { cancelled = true; };
  }, []);

  useEffect(() => {
    if (!activeSession) return;
    const sessionId = activeSession;
    let cancelled = false;
    api.getPreferences(sessionId).then((value) => {
      if (!cancelled) setLoadedPreferences({ sessionId, value });
    }).catch((reason: unknown) => {
      if (cancelled) return;
      if (isNotFound(reason)) forgetMissingSession(sessionId);
      else setError('会话偏好加载失败：' + errorText(reason));
    });
    return () => { cancelled = true; };
  }, [activeSession, forgetMissingSession]);

  const changePreferences = async (value: SessionPreferences) => {
    const sessionId = activeSession;
    if (!sessionId) { setDraftPreferences(value); return; }
    setSavingPreferences(true);
    try {
      const saved = await api.savePreferences(sessionId, value);
      if (activeSessionRef.current === sessionId) setLoadedPreferences({ sessionId, value: saved });
    } catch (reason) {
      setError('会话偏好保存失败：' + errorText(reason));
      throw reason;
    } finally { setSavingPreferences(false); }
  };

  // Live text is drawn once per frame from what the stream has delivered so far.
  const flushLive = useCallback(() => {
    liveFrame.current = 0;
    const latest = { ...liveNow.current };
    setLive((previous) => {
      let next = previous;
      for (const [runId, text] of Object.entries(latest)) {
        if ((previous[runId] || '') === text) continue;
        if (next === previous) next = { ...previous };
        next[runId] = text;
      }
      return next;
    });
  }, []);

  const setLiveText = useCallback((runId: string, text: string) => {
    if ((liveNow.current[runId] || '') === text) return;
    liveNow.current[runId] = text;
    if (!liveFrame.current) liveFrame.current = window.requestAnimationFrame(flushLive);
  }, [flushLive]);

  const onEvent = useCallback((runId: string, event: HarnessEvent) => {
    streamedAt.current[runId] = Date.now();
    // Committed response text belongs to messages, even if the run continues
    // with another model call. Stream text is only a temporary in-flight view.
    setLiveText(runId, event.type === 'model.completed' || /^run\.(completed|failed|cancelled)$/i.test(event.type) ? '' : liveText(liveNow.current[runId] || '', event));
    // Stream deltas and resets are not stored and carry no id.
    if (event.seq <= 0) return;
    lastSeq.current[runId] = Math.max(lastSeq.current[runId] || 0, event.seq);
    setEvents((previous) => {
      const current = previous[runId] || [];
      const merged = mergeEvents(current, [event]);
      return merged === current ? previous : { ...previous, [runId]: merged };
    });
  }, [setLiveText]);

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

  // Connection state and the session list.
  useEffect(() => {
    let stopped = false;
    const check = () => {
      api.health().then(() => {
        if (stopped) return;
        setOnline(true);
      }).catch(() => {
        if (!stopped) setOnline(false);
      });
    };
    check();
    api.listSessions().then(setSessions).catch((reason: unknown) => setError(`会话加载失败：${errorText(reason)}`));
    const slow = window.setInterval(() => {
      check();
      refreshSessions().catch(() => undefined);
    }, 8000);
    return () => {
      stopped = true;
      window.clearInterval(slow);
    };
  }, [refreshSessions]);

  useEffect(() => {
    if (!activeSession) return;
    const sessionId = activeSession;
    let stopped = false;
    refreshRuns(sessionId).catch((reason: unknown) => {
      if (stopped || activeSessionRef.current !== sessionId) return;
      // A session id saved by an earlier serve database no longer exists.
      if (isNotFound(reason)) forgetMissingSession(sessionId);
      else setError(`运行状态加载失败：${errorText(reason)}`);
    });
    return () => { stopped = true; };
  }, [activeSession, sessionRevision, refreshRuns, forgetMissingSession]);

  // Follow every run of the open session that has not finished: one event
  // stream each, plus a poll of the run view once a second.
  useEffect(() => {
    if (!followKey) return;
    const ids = followKey.split(',');
    const controller = new AbortController();
    const follow = async (runId: string) => {
      while (!controller.signal.aborted) {
        // A new connection starts with the text so far of the response in
        // progress, so what an earlier connection showed is dropped first.
        setLiveText(runId, '');
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
          // A proxy that buffers the stream would hide events until the run ends.
          if (changed && Date.now() - (streamedAt.current[runId] || 0) > 2000) pullEvents(runId).catch(() => undefined);
        }).catch(() => undefined);
      }
    }, 1000);
    return () => {
      controller.abort();
      window.clearInterval(timer);
      refreshSessions().catch(() => undefined);
    };
  }, [followKey, onEvent, pullEvents, refreshRun, refreshSessions, setLiveText]);

  // Titles are a UI preference; turn output uses the authoritative root run final_answer.
  useEffect(() => {
    if (!activeSession || titles[activeSession]) return;
    const text = history.messages.find((message) => message.kind === 'input')?.content;
    if (text) setTitles((previous) => ({ ...previous, [activeSession]: text.slice(0, 80) }));
  }, [activeSession, history.messages, setTitles, titles]);

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

  // Keep the newest content in view unless the reader scrolled up.
  useEffect(() => {
    const node = threadRef.current;
    if (!node) return;
    const runSelector = focusRun.current ? '[data-run-id="' + CSS.escape(focusRun.current) + '"]' : '';
    const target = runSelector ? node.querySelector('.current-run' + runSelector + ' .wait-card') || node.querySelector('.turn' + runSelector + ' .wait-card') || node.querySelector('.turn' + runSelector) || node.querySelector('.history-message' + runSelector) : null;
    if (target) {
      // A run opened by id is shown at its own turn, not at the newest one.
      stickToBottom.current = false;
      target.scrollIntoView({ block: 'start' });
      if (!history.loading) focusRun.current = null;
    } else if (stickToBottom.current) {
      node.scrollTop = node.scrollHeight;
    }
  }, [activeRuns, events, history.loading, history.messages, live, optimistic, view]);

  const openSession = (sessionId: string | null) => {
    stickToBottom.current = true;
    setAwayFromBottom(false);
    setError('');
    setView('chat');
    setInspected(null);
    setAnswerRun(null);
    selectSession(sessionId);
    if (window.matchMedia(NARROW).matches) setSidebarOpen(false);
  };

  const showPage = (page: View) => {
    setView(page);
    if (window.matchMedia(NARROW).matches) setSidebarOpen(false);
  };

  const send = async (text: string) => {
    setError('');
    setDraft('');
    const selectedPreferences = { ...preferences };
    let sessionId = activeSession;
    try {
      if (!sessionId) {
        sessionId = (await api.createSession()).id;
        const now = new Date().toISOString();
        const created = { id: sessionId, created_at: now, updated_at: now, run_count: 0 };
        setSessions((previous) => [created, ...previous]);
        setRuns((previous) => ({ ...previous, [created.id]: [] }));
        const saved = await api.savePreferences(sessionId, selectedPreferences);
        setLoadedPreferences({ sessionId, value: saved });
        selectSession(sessionId);
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
      upsertRun(run);
      setPending((current) => (current?.sessionId === target ? { ...current, runId: run.id } : current));
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
      const skill = skillInvocation(text);
      adopt(await api.createRun(target, skill?.task ?? text, { ...selectedPreferences, skill: skill?.name }));
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
      history.refresh();
      refreshSessions().catch(() => undefined);
    }
  };

  const respond = async (runId: string, body: { decision?: string; answer?: string }) => {
    if (responding.current.has(runId)) return false;
    responding.current.add(runId);
    setError('');
    setActing((previous) => ({ ...previous, [runId]: true }));
    try {
      upsertRun(await api.respond(runId, body));
      return true;
    } catch (reason) {
      setError(`提交失败：${errorText(reason)}`);
      return false;
    } finally {
      responding.current.delete(runId);
      setActing((previous) => ({ ...previous, [runId]: false }));
      history.refresh();
      refreshRun(runId).catch(() => undefined);
    }
  };

  const addBudget = async (runId: string, extension: BudgetExtension) => {
    setActing((previous) => ({ ...previous, [runId]: true }));
    try {
      upsertRun(await api.addBudget(runId, extension));
    } catch (reason) {
      setError(`补充预算失败：${errorText(reason)}`);
    } finally {
      setActing((previous) => ({ ...previous, [runId]: false }));
      history.refresh();
    }
  };

  const answer = async (run: RunView, text: string) => {
    setDraft('');
    stickToBottom.current = true;
    if (await respond(run.id, { answer: text })) return;
    setDraft((current) => current || text);
  };

  const cancel = async (runId: string) => {
    setActing((previous) => ({ ...previous, [runId]: true }));
    try {
      upsertRun(await api.cancel(runId));
    } catch (reason) {
      setError(`取消失败：${errorText(reason)}`);
    } finally {
      setActing((previous) => ({ ...previous, [runId]: false }));
      history.refresh();
    }
  };

  const stop = async () => {
    if (latestRun && isActive(latestRun)) await cancel(latestRun.id);
  };

  const openRun = async (runId: string) => {
    try {
      const run = await api.getRun(runId);
      openSession(run.session_id);
      // Held while the rest of the session loads around the turn.
      focusRun.current = run.id;
      window.setTimeout(() => { if (focusRun.current === run.id) focusRun.current = null; }, 1500);
      upsertRun(run);
      setInspected(run.id);
      loadEvents(run);
    } catch (reason) {
      setError(`无法打开运行：${errorText(reason)}`);
    }
  };

  useEffect(() => {
    const query = new URLSearchParams(window.location.search);
    const runId = query.get('run');
    let cancelled = false;
    if (runId) {
      api.getRun(runId).then((run) => {
        if (cancelled) return;
        selectSession(run.session_id);
        setView('chat');
        focusRun.current = run.id;
        upsertRun(run);
      }).catch((reason: unknown) => { if (!cancelled) setError('无法打开运行：' + errorText(reason)); });
    }
    return () => { cancelled = true; };
  }, [selectSession, upsertRun]);

  const actions: TurnActions = {
    respond: (runId, body) => void respond(runId, body),
    addBudget: (runId, extension) => void addBudget(runId, extension),
    cancel: (runId) => void cancel(runId),
    inspect: (runId) => {
      const run = activeRuns.find((item) => item.id === runId);
      if (run) {
        setInspected(runId);
        loadEvents(run);
      } else {
        void openRun(runId);
      }
    },
    focusAnswer: (runId) => {
      setAnswerRun(runId);
      threadRef.current?.parentElement?.querySelector('textarea')?.focus();
    },
    loadEvents,
  };

  const groups = useMemo(() => {
    const query = sessionQuery.trim().toLowerCase();
    const sorted = [...sessions].sort((a, b) => (b.latest_run_at || b.updated_at).localeCompare(a.latest_run_at || a.updated_at));
    const out: { label: string; items: SessionView[] }[] = [];
    for (const session of sorted) {
      if (query && ![titles[session.id] || '未命名对话', session.id].some(value => value.toLowerCase().includes(query))) continue;
      const label = dayGroup(session.latest_run_at || session.updated_at);
      const group = out.find((item) => item.label === label);
      if (group) group.items.push(session);
      else out.push({ label, items: [session] });
    }
    return out;
  }, [sessions, titles, sessionQuery]);

  const titleOf = (sessionId: string) => titles[sessionId] || '未命名对话';
  const refreshHistory = () => {
    history.refresh();
    if (activeSession) void refreshRuns(activeSession).catch(() => undefined);
  };
  const empty = view === 'chat' && !activeSession && !optimistic;
  const selectedModel = models.find((model) => model.id === preferences.model_id || model.legacy_id === preferences.model_id);
  const modelAvailable = Boolean(selectedModel);
  const modelBlocked = modelsLoading || Boolean(modelsError) || !modelAvailable;
  const composer = (
    <>
    {!modelsLoading && (modelsError || !modelAvailable) && <div className="model-notice" role="status">
      <span>{modelsError || (models.length === 0 ? '还没有已配置模型。' : '当前模型不可用，请重新选择。')}</span>
      {modelsError && <button type="button" className="ghost-button" onClick={() => void refreshModels()}>重试</button>}
      <button type="button" className="ghost-button" onClick={() => showPage('settings')}>去配置模型服务</button>
    </div>}
    <Composer
      value={draft}
      onChange={setDraft}
      onSubmit={() => {
        const text = draft.trim();
        if (!text || busy || (!answering && modelBlocked)) return;
        if (answering) void answer(answering, text);
        else void send(text);
      }}
      onStop={latestRun && isActive(latestRun) ? () => void stop() : undefined}
      busy={Boolean(busy || (!answering && modelBlocked))}
      stopping={latestRun ? acting[latestRun.id] : false}
      placeholder={answering ? '回答问题…' : '发消息…'}
      skillsEnabled={!answering}
      hint={online === false ? '未连接' : answering ? '回复后继续本轮' : undefined}
      autoFocus={view === 'chat'}
      models={models}
      selectedModel={selectedModel}
      modelsLoading={modelsLoading}
      modelSelectionDisabled={preferencesBusy || modelsLoading || Boolean(modelsError)}
      onModelChange={(model_id) => { void changePreferences({ ...preferences, model_id }).catch(() => undefined); }}
      reasoningEffort={preferences.reasoning_effort}
      onReasoningChange={(reasoning_effort) => { void changePreferences({ ...preferences, reasoning_effort }).catch(() => undefined); }}
      approvalMode={preferences.approval_mode}
      onApprovalModeChange={(approval_mode) => { void changePreferences({ ...preferences, approval_mode }).catch(() => undefined); }}
      preferencesBusy={preferencesBusy || modelBlocked}
    />
    </>
  );

  return (
    <div className={`app ${sidebarOpen ? 'with-sidebar' : ''} ${inspectedRun && view === 'chat' ? 'with-inspector' : ''}`}>
      {sidebarOpen && <button type="button" className="scrim" aria-label="关闭侧栏" onClick={() => toggleSidebar(false)} />}
      <aside className="sidebar" aria-label="对话列表" hidden={!sidebarOpen}>
        <div className="sidebar-top">
          <button type="button" className="brand" onClick={() => openSession(null)}><span className="brand-mark" />Cloud Harness</button>
          <button type="button" className="icon-button" aria-label="收起侧栏" title="收起侧栏" onClick={() => toggleSidebar(false)}><Icon name="sidebar" size={18} /></button>
        </div>
        <nav className="sidebar-nav" aria-label="工作区">
          <button type="button" className={`nav-item new-chat ${view === 'chat' && !activeSession ? 'current' : ''}`} onClick={() => openSession(null)}><Icon name="compose" />新对话</button>
          <button type="button" className={`nav-item ${view === 'observability' ? 'current' : ''}`} aria-current={view === 'observability' ? 'page' : undefined} onClick={() => showPage('observability')}><Icon name="activity" />观测</button>
        </nav>
        <label className="session-search">
          <Icon name="search" size={15} />
          <input type="search" aria-label="搜索对话" placeholder="搜索对话" value={sessionQuery} onChange={event => setSessionQuery(event.target.value)} />
        </label>
        <div className="session-list">
          {groups.length === 0 && <p className="sidebar-empty">{sessionQuery ? '没有匹配的对话' : '历史对话会显示在这里'}</p>}
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
          <button type="button" className={`nav-item ${view === 'settings' ? 'current' : ''}`} aria-current={view === 'settings' ? 'page' : undefined} onClick={() => showPage('settings')}><Icon name="settings" />设置</button>
          <span className="connection" title={online === false ? '无法连接' : '连接状态'}>
            <span className={`dot ${online === false ? 'tone-fail' : online ? 'tone-ok' : 'tone-idle'}`} />
            {online === false ? '未连接' : online ? '已连接' : '连接中…'}
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
          <h1>{view === 'settings' ? '设置' : view === 'observability' ? '观测' : activeSession ? titleOf(activeSession) : '新对话'}</h1>
          {view === 'chat' && activeSession && <button type="button" className="icon-button" disabled={history.loading} onClick={refreshHistory} aria-label="刷新会话历史" title="刷新会话历史"><Icon name="refresh" size={15} /></button>}
        </header>

        {error && <div className="flash" role="alert"><Icon name="alert" /><span>{error}</span><button type="button" className="icon-button" aria-label="关闭提示" onClick={() => setError('')}><Icon name="close" size={14} /></button></div>}

        {view === 'chat' && empty && (
          <div className="welcome">
            <div className="welcome-inner">
              <h2>今天想构建什么？</h2>
              {composer}
              <details className="starters">
                <summary><Icon name="plus" size={14} />示例任务</summary>
                <div className="suggestions" role="group" aria-label="示例任务">
                  {suggestions.map(({ label, icon, prompt }) => <button type="button" key={label} title={prompt} onClick={event => {
                    setDraft(prompt);
                    const details = event.currentTarget.closest('details');
                    if (details) details.open = false;
                    event.currentTarget.closest('.welcome')?.querySelector('textarea')?.focus();
                  }}><Icon name={icon} size={15} /><span>{label}</span></button>)}
                </div>
              </details>
            </div>
          </div>
        )}

        {view === 'chat' && !empty && <>
          <div className="thread" ref={threadRef} onScroll={(event) => {
            const node = event.currentTarget;
            stickToBottom.current = node.scrollHeight - node.scrollTop - node.clientHeight < 120;
            setAwayFromBottom(!stickToBottom.current);
          }}>
            <div className="thread-inner">
              <SessionHistory messages={history.messages} runs={activeRuns} events={events} live={live} acting={acting} actions={actions} approvalMode={preferencesBusy ? undefined : preferences.approval_mode} loading={history.loading} error={history.error} onRefresh={refreshHistory} onInspect={actions.inspect} />
              {optimistic && <PendingTurn text={optimistic.text} />}
            </div>
          </div>
          <div className="dock">
            {awayFromBottom && <button type="button" className="jump-latest" onClick={() => {
              const node = threadRef.current;
              if (node) node.scrollTop = node.scrollHeight;
              stickToBottom.current = true;
              setAwayFromBottom(false);
            }}><Icon name="down" size={14} />回到最新消息</button>}
            <div className="dock-inner">{composer}</div>
          </div>
        </>}

        {view === 'observability' && <div className="page-scroll"><Observability onOpenRun={(runId) => void openRun(runId)} /></div>}
        {view === 'settings' && <div className="page-scroll"><Settings onModelsChanged={refreshModels} onBack={() => showPage('chat')} /></div>}
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
            <dl className="inspector-context">
              <dt>审批模式</dt><dd>{approvalModeLabel(inspectedRun.approval_mode)}</dd>
              {inspectedRun.workspace && <><dt>工作区</dt><dd>{inspectedRun.workspace}</dd></>}
            </dl>
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
