import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Block, Button, Empty, Flexbox, Input, Tag, Text } from '@lobehub/ui';
import { ChatInputArea, ChatItem } from '@lobehub/ui/chat';
import { api } from './api';
import type { HarnessEvent, RunView, TraceView } from './api';

interface RunEntry {
  run: RunView;
  userText: string;
}

const LS_SESSIONS = 'ch-ui.sessions';
const LS_RUNS = 'ch-ui.runs';
const LS_ACTIVE = 'ch-ui.active';

const load = <T,>(key: string, fallback: T): T => {
  try {
    const v = localStorage.getItem(key);
    return v ? (JSON.parse(v) as T) : fallback;
  } catch {
    return fallback;
  }
};

const short = (id: string) => (id.length > 14 ? `${id.slice(0, 11)}…` : id);

const statusColor = (s: string) =>
  s === 'COMPLETED'
    ? 'green'
    : s === 'RUNNING'
      ? 'blue'
      : s === 'WAITING'
        ? 'orange'
        : s === 'FAILED'
          ? 'red'
          : 'default';

const waitText = (r: RunView) => {
  switch (r.wait_reason) {
    case 'USER':
      return '等待你的回答…';
    case 'APPROVAL':
      return '有操作等待审批…';
    case 'CHILD':
      return '子任务执行中…';
    case 'BUDGET':
      return '模型调用预算已用完，等待补充…';
    case 'RECONCILIATION':
      return '等待对账确认…';
    default:
      return '等待中…';
  }
};

const assistantContent = (e: RunEntry) => {
  const r = e.run;
  if (r.final_answer) return r.final_answer;
  if (r.status === 'RUNNING') return '正在运行…';
  if (r.status === 'WAITING') return waitText(r);
  if (r.status === 'CANCELLED') return '（这次运行已取消）';
  if (r.status === 'FAILED') return r.error?.message || '运行失败';
  return '';
};

export default function App() {
  const [sessions, setSessions] = useState<string[]>(() => load(LS_SESSIONS, []));
  const [runs, setRuns] = useState<Record<string, RunEntry[]>>(() => load(LS_RUNS, {}));
  const [activeSession, setActiveSession] = useState<string | null>(() => load(LS_ACTIVE, null));
  const [provider, setProvider] = useState('');
  const [online, setOnline] = useState(false);
  const [input, setInput] = useState('');
  const [sending, setSending] = useState(false);
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null);
  const [events, setEvents] = useState<Record<string, HarnessEvent[]>>({});
  const [trace, setTrace] = useState<TraceView | null>(null);
  const [panelTab, setPanelTab] = useState<'events' | 'trace'>('events');
  const [answer, setAnswer] = useState('');
  const [err, setErr] = useState('');
  const lastSeq = useRef<Record<string, number>>({});

  useEffect(() => localStorage.setItem(LS_SESSIONS, JSON.stringify(sessions)), [sessions]);
  useEffect(() => localStorage.setItem(LS_RUNS, JSON.stringify(runs)), [runs]);
  useEffect(() => localStorage.setItem(LS_ACTIVE, JSON.stringify(activeSession)), [activeSession]);

  const activeRuns = useMemo(
    () => (activeSession ? runs[activeSession] || [] : []),
    [runs, activeSession],
  );
  const latestRun = activeRuns.length ? activeRuns[activeRuns.length - 1].run : null;
  const selectedRun = useMemo(() => {
    const all = activeRuns.map((e) => e.run);
    return all.find((r) => r.id === selectedRunId) || latestRun;
  }, [activeRuns, selectedRunId, latestRun]);

  const upsertRun = useCallback((entry: RunEntry) => {
    setRuns((prev) => {
      const list = [...(prev[entry.run.session_id] || [])];
      const i = list.findIndex((e) => e.run.id === entry.run.id);
      if (i >= 0) list[i] = { ...list[i], run: entry.run };
      else list.push(entry);
      return { ...prev, [entry.run.session_id]: list };
    });
  }, []);

  const replaceRun = useCallback((run: RunView) => {
    setRuns((prev) => {
      const list = [...(prev[run.session_id] || [])];
      const i = list.findIndex((e) => e.run.id === run.id);
      if (i >= 0) list[i] = { ...list[i], run };
      return { ...prev, [run.session_id]: list };
    });
  }, []);

  const refreshRun = useCallback(
    async (runId: string) => {
      const run = await api.getRun(runId);
      replaceRun(run);
      return run;
    },
    [replaceRun],
  );

  const pullEvents = useCallback(async (runId: string) => {
    const after = lastSeq.current[runId] || 0;
    const fresh = await api.fetchEvents(runId, after);
    if (!fresh.length) return;
    for (const ev of fresh) {
      if (ev.seq > (lastSeq.current[runId] || 0)) lastSeq.current[runId] = ev.seq;
    }
    setEvents((prev) => {
      const merged = [...(prev[runId] || [])];
      const seen = new Set(merged.map((e) => e.seq));
      for (const ev of fresh) if (ev.seq === 0 || !seen.has(ev.seq)) merged.push(ev);
      return { ...prev, [runId]: merged };
    });
  }, []);

  // Health check once on mount.
  useEffect(() => {
    api
      .health()
      .then((h) => {
        setOnline(true);
        setProvider(h.provider);
      })
      .catch(() => setOnline(false));
  }, []);

  // Hydrate persisted runs once (the API has no list endpoint, so the UI
  // remembers the run ids it created and re-fetches their state).
  useEffect(() => {
    const saved = load<Record<string, { id: string; userText: string }[]>>('ch-ui.runids', {});
    (async () => {
      for (const [sid, list] of Object.entries(saved)) {
        if (sid) setSessions((prev) => (prev.includes(sid) ? prev : [...prev, sid]));
        for (const item of list) {
          try {
            const run = await api.getRun(item.id);
            upsertRun({ run, userText: item.userText });
          } catch {
            /* run belongs to an older server data dir */
          }
        }
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Persist run ids + user text for hydration.
  useEffect(() => {
    const slim: Record<string, { id: string; userText: string }[]> = {};
    for (const [sid, list] of Object.entries(runs)) {
      slim[sid] = list.map((e) => ({ id: e.run.id, userText: e.userText }));
    }
    localStorage.setItem('ch-ui.runids', JSON.stringify(slim));
  }, [runs]);

  const hasActive = activeRuns.some(
    (e) => e.run.status === 'RUNNING' || e.run.status === 'WAITING',
  );

  // Poll active runs and their events.
  useEffect(() => {
    if (!hasActive) return;
    const t = setInterval(async () => {
      for (const e of activeRuns) {
        if (e.run.status === 'RUNNING' || e.run.status === 'WAITING') {
          try {
            await refreshRun(e.run.id);
            await pullEvents(e.run.id);
          } catch {
            /* transient */
          }
        }
      }
    }, 1200);
    return () => clearInterval(t);
  }, [hasActive, activeRuns, refreshRun, pullEvents]);

  // Load trace for the selected run when the trace tab is open.
  useEffect(() => {
    if (panelTab !== 'trace' || !selectedRun) return;
    api
      .getTrace(selectedRun.id)
      .then(setTrace)
      .catch(() => setTrace(null));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [panelTab, selectedRun?.id, selectedRun?.status]);

  const newSession = async () => {
    setErr('');
    try {
      const s = await api.createSession();
      setSessions((prev) => [s.id, ...prev]);
      setActiveSession(s.id);
      setSelectedRunId(null);
    } catch (e) {
      setErr(`创建会话失败：${(e as Error).message}（harness serve 没在跑？）`);
    }
  };

  const send = async () => {
    const text = input.trim();
    if (!text || sending) return;
    setSending(true);
    setErr('');
    try {
      let sid = activeSession;
      if (!sid) {
        const s = await api.createSession();
        sid = s.id;
        setSessions((prev) => [s.id, ...prev]);
        setActiveSession(s.id);
      }
      const run = await api.createRun(sid, text);
      upsertRun({ run, userText: text });
      setSelectedRunId(run.id);
      setInput('');
      await pullEvents(run.id).catch(() => undefined);
    } catch (e) {
      setErr(`发送失败：${(e as Error).message}`);
    } finally {
      setSending(false);
    }
  };

  const act = async (fn: () => Promise<RunView>) => {
    setErr('');
    try {
      const run = await fn();
      replaceRun(run);
      await pullEvents(run.id).catch(() => undefined);
    } catch (e) {
      setErr(`操作失败：${(e as Error).message}`);
    }
  };

  // Note: @lobehub/ui v6's ChatList does not render message content by
  // itself, so items are rendered with ChatItem directly (avatar + message).
  const renderMessages = () => (
    <Flexbox gap={6} style={{ padding: '16px 12px', maxWidth: 860, margin: '0 auto', width: '100%' }}>
      {activeRuns.map((e) => (
        <div key={e.run.id}>
          <ChatItem
            avatar={{ avatar: '🧑‍💻', title: '你' }}
            message={e.userText}
            placement="right"
            primary
          />
          <ChatItem
            avatar={{ avatar: '🤖', title: 'Cloud Harness' }}
            loading={e.run.status === 'RUNNING'}
            message={e.run.final_answer ?? assistantContent(e)}
            messageExtra={
              <Tag color={statusColor(e.run.status)}>
                {e.run.status}
                {e.run.wait_reason ? ` · ${e.run.wait_reason}` : ''}
              </Tag>
            }
            error={
              e.run.status === 'FAILED'
                ? { title: e.run.error?.message || '运行失败', type: 'error' }
                : undefined
            }
          />
        </div>
      ))}
    </Flexbox>
  );

  const waiting = latestRun && latestRun.status === 'WAITING' ? latestRun : null;
  const selectedEvents = selectedRun ? events[selectedRun.id] || [] : [];

  return (
    <Flexbox horizontal style={{ height: '100vh', width: '100vw' }}>
      {/* Sidebar */}
      <Flexbox
        gap={12}
        style={{
          width: 264,
          flexShrink: 0,
          padding: 16,
          borderRight: '1px solid rgba(0,0,0,0.06)',
          background: 'rgba(0,0,0,0.02)',
          overflowY: 'auto',
        }}
      >
        <Flexbox horizontal align="center" justify="space-between">
          <Text strong style={{ fontSize: 16 }}>
            ⛅ Cloud Harness
          </Text>
          <Tag color={online ? 'green' : 'red'}>{online ? `已连接 · ${provider || '…'}` : '未连接'}</Tag>
        </Flexbox>
        <Button block type="primary" onClick={newSession}>
          ＋ 新会话
        </Button>
        <Flexbox gap={6}>
          {sessions.length === 0 && (
            <Text type="secondary" style={{ fontSize: 12 }}>
              还没有会话。点上面新建一个，或直接在右边发消息会自动创建。
            </Text>
          )}
          {sessions.map((sid) => (
            <Block
              key={sid}
              clickable
              variant={sid === activeSession ? 'filled' : 'borderless'}
              onClick={() => {
                setActiveSession(sid);
                setSelectedRunId(null);
              }}
              style={{ padding: '8px 10px' }}
            >
              <Text style={{ fontSize: 13 }} ellipsis>
                会话 {short(sid)}
              </Text>
              {(runs[sid] || []).map((e) => (
                <Flexbox key={e.run.id} horizontal align="center" gap={6} style={{ marginTop: 4 }}>
                  <span
                    style={{
                      width: 7,
                      height: 7,
                      borderRadius: '50%',
                      flexShrink: 0,
                      background:
                        e.run.status === 'COMPLETED'
                          ? '#52c41a'
                          : e.run.status === 'RUNNING'
                            ? '#1677ff'
                            : e.run.status === 'WAITING'
                              ? '#fa8c16'
                              : e.run.status === 'FAILED'
                                ? '#f5222d'
                                : '#bfbfbf',
                    }}
                  />
                  <Text type="secondary" style={{ fontSize: 12 }} ellipsis>
                    {e.userText || short(e.run.id)}
                  </Text>
                </Flexbox>
              ))}
            </Block>
          ))}
        </Flexbox>
        <Text type="secondary" style={{ fontSize: 11, marginTop: 'auto' }}>
          API 127.0.0.1:19421 · 界面经 Vite 代理同源访问
        </Text>
      </Flexbox>

      {/* Main column */}
      <Flexbox flex={1} style={{ minWidth: 0 }}>
        <Flexbox
          horizontal
          align="center"
          justify="space-between"
          style={{ padding: '10px 16px', borderBottom: '1px solid rgba(0,0,0,0.06)' }}
        >
          <Flexbox horizontal align="center" gap={10}>
            <Text strong>{activeSession ? `会话 ${short(activeSession)}` : 'Cloud Harness 控制台'}</Text>
            {latestRun && <Tag color={statusColor(latestRun.status)}>{latestRun.status}</Tag>}
            {latestRun?.wait_reason && <Tag color="orange">{latestRun.wait_reason}</Tag>}
          </Flexbox>
          <Flexbox horizontal align="center" gap={10}>
            {latestRun && (
              <Text type="secondary" style={{ fontSize: 12 }}>
                模型调用 {latestRun.budget.used_model_calls}/{latestRun.budget.max_model_calls} · 工具{' '}
                {latestRun.budget.used_tool_calls}/{latestRun.budget.max_tool_calls} · 子任务{' '}
                {latestRun.budget.used_child_runs}/{latestRun.budget.max_child_runs}
              </Text>
            )}
            {latestRun && (latestRun.status === 'RUNNING' || latestRun.status === 'WAITING') && (
              <Button danger size="small" onClick={() => act(() => api.cancel(latestRun.id))}>
                取消运行
              </Button>
            )}
          </Flexbox>
        </Flexbox>

        {err && (
          <div style={{ padding: '8px 16px', background: '#fff2f0', color: '#cf1322', fontSize: 13 }}>
            {err}
          </div>
        )}

        <div style={{ flex: 1, overflowY: 'auto', minHeight: 0 }}>
          {activeRuns.length === 0 ? (
            <Flexbox align="center" justify="center" style={{ height: '100%' }}>
              <Empty title="还没有对话" description="在下方输入一句话，harness 就会开一个 run 来处理。" />
            </Flexbox>
          ) : (
            renderMessages()
          )}
        </div>

        {waiting && (
          <Block variant="filled" style={{ margin: '0 16px 8px', padding: '10px 12px' }}>
            {waiting.wait_reason === 'APPROVAL' && (
              <Flexbox horizontal align="center" justify="space-between">
                <Text>🔐 有工具调用在等你审批（run {short(waiting.id)}）</Text>
                <Flexbox horizontal gap={8}>
                  <Button
                    type="primary"
                    onClick={() => act(() => api.respond(waiting.id, { decision: 'approve' }))}
                  >
                    同意
                  </Button>
                  <Button danger onClick={() => act(() => api.respond(waiting.id, { decision: 'deny' }))}>
                    拒绝
                  </Button>
                </Flexbox>
              </Flexbox>
            )}
            {waiting.wait_reason === 'USER' && (
              <Flexbox horizontal align="center" gap={8}>
                <Text style={{ flexShrink: 0 }}>💬 Agent 在等你的回答：</Text>
                <Input
                  value={answer}
                  onChange={(e) => setAnswer(e.target.value)}
                  placeholder="输入回答…"
                  onPressEnter={() => {
                    if (answer.trim()) {
                      act(() => api.respond(waiting.id, { answer: answer.trim() })).then(() => setAnswer(''));
                    }
                  }}
                />
                <Button
                  type="primary"
                  disabled={!answer.trim()}
                  onClick={() =>
                    act(() => api.respond(waiting.id, { answer: answer.trim() })).then(() => setAnswer(''))
                  }
                >
                  提交
                </Button>
              </Flexbox>
            )}
            {waiting.wait_reason === 'BUDGET' && (
              <Flexbox horizontal align="center" justify="space-between">
                <Text>⏳ 模型调用预算已用完，run 停在这里等补充（不会丢进度）。</Text>
                <Button type="primary" onClick={() => act(() => api.addBudget(waiting.id, 10))}>
                  +10 次调用
                </Button>
              </Flexbox>
            )}
            {waiting.wait_reason === 'CHILD' && <Text>🧩 子任务执行中，完成后会自动继续。</Text>}
            {waiting.wait_reason === 'RECONCILIATION' && (
              <Flexbox horizontal align="center" justify="space-between">
                <Text>⚠️ 有一步工具调用结果不明，需要你确认它到底成没成。</Text>
                <Flexbox horizontal gap={8}>
                  <Button
                    type="primary"
                    onClick={() => act(() => api.respond(waiting.id, { decision: 'reconcile_success' }))}
                  >
                    标记成功
                  </Button>
                  <Button
                    danger
                    onClick={() => act(() => api.respond(waiting.id, { decision: 'reconcile_failure' }))}
                  >
                    标记失败
                  </Button>
                </Flexbox>
              </Flexbox>
            )}
          </Block>
        )}

        <div style={{ padding: '0 16px 12px', height: 108, flexShrink: 0 }}>
          <ChatInputArea
            expand={false}
            setExpand={() => undefined}
            heights={{ inputHeight: 64, maxHeight: 84, minHeight: 64 }}
            loading={sending}
            value={input}
            onInput={setInput}
            onSend={send}
            placeholder="给 Cloud Harness 发消息…（回车发送）"
          />
        </div>
      </Flexbox>

      {/* Right panel: events / trace */}
      <Flexbox
        style={{
          width: 330,
          flexShrink: 0,
          borderLeft: '1px solid rgba(0,0,0,0.06)',
          minHeight: 0,
        }}
      >
        <Flexbox
          horizontal
          align="center"
          justify="space-between"
          style={{ padding: '10px 12px', borderBottom: '1px solid rgba(0,0,0,0.06)' }}
        >
          <Flexbox horizontal gap={6}>
            <Button size="small" type={panelTab === 'events' ? 'primary' : 'default'} onClick={() => setPanelTab('events')}>
              事件流
            </Button>
            <Button size="small" type={panelTab === 'trace' ? 'primary' : 'default'} onClick={() => setPanelTab('trace')}>
              执行轨迹
            </Button>
          </Flexbox>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {selectedRun ? short(selectedRun.id) : '无 run'}
          </Text>
        </Flexbox>

        <Flexbox gap={4} style={{ padding: '8px 12px', overflowY: 'auto', minHeight: 0 }}>
          {activeRuns.length > 1 && (
            <Flexbox gap={4} style={{ marginBottom: 6 }}>
              <Text type="secondary" style={{ fontSize: 12 }}>
                查看哪个 run：
              </Text>
              {activeRuns.map((e) => (
                <Button
                  key={e.run.id}
                  size="small"
                  block
                  type={selectedRun?.id === e.run.id ? 'primary' : 'default'}
                  onClick={() => {
                    setSelectedRunId(e.run.id);
                    pullEvents(e.run.id).catch(() => undefined);
                  }}
                >
                  {short(e.run.id)} · {e.run.status}
                </Button>
              ))}
            </Flexbox>
          )}

          {panelTab === 'events' &&
            (selectedEvents.length === 0 ? (
              <Text type="secondary" style={{ fontSize: 12 }}>
                暂无事件。发送一条消息后，这里会刷出这个 run 的持久事件。
              </Text>
            ) : (
              selectedEvents.map((ev, i) => (
                <div key={`${ev.seq}-${i}`} style={{ fontSize: 12, lineHeight: 1.5 }}>
                  <Text strong style={{ fontSize: 12 }}>
                    #{ev.seq || '–'} {ev.type}
                  </Text>
                  <div
                    style={{
                      color: 'rgba(0,0,0,0.55)',
                      whiteSpace: 'nowrap',
                      overflow: 'hidden',
                      textOverflow: 'ellipsis',
                      fontFamily: 'monospace',
                      fontSize: 11,
                    }}
                    title={ev.data}
                  >
                    {ev.data}
                  </div>
                </div>
              ))
            ))}

          {panelTab === 'trace' &&
            (!trace || !trace.run ? (
              <Text type="secondary" style={{ fontSize: 12 }}>
                暂无轨迹数据。
              </Text>
            ) : (
              <Flexbox gap={8}>
                <Text style={{ fontSize: 12 }}>
                  状态 <Tag color={statusColor(trace.run.status)}>{trace.run.status}</Tag>{' '}
                  {trace.run.wait_reason && <Tag color="orange">{trace.run.wait_reason}</Tag>}
                </Text>
                {(trace.run.steps || []).map((step, i) => (
                  <Block key={step.id} variant="outlined" style={{ padding: '8px 10px' }}>
                    <Text style={{ fontSize: 12 }} strong>
                      步骤 {i + 1}
                      {step.action ? ` · ${step.action}` : ''}
                    </Text>
                    {(step.batches || []).map((b) => (
                      <div key={b.id} style={{ marginTop: 4 }}>
                        <Text type="secondary" style={{ fontSize: 12 }}>
                          批次 {b.action} · {b.status}
                        </Text>
                        {(b.calls || []).map((c) => (
                          <Flexbox key={c.id} horizontal justify="space-between" style={{ marginTop: 2 }}>
                            <Text style={{ fontSize: 12, fontFamily: 'monospace' }}>{c.name}</Text>
                            <Tag color={statusColor(c.status)}>{c.status}</Tag>
                          </Flexbox>
                        ))}
                      </div>
                    ))}
                  </Block>
                ))}
                {(trace.run.children || []).map((c) => (
                  <Text key={c.id} style={{ fontSize: 12 }}>
                    子 run {short(c.id)} · {c.status}
                  </Text>
                ))}
              </Flexbox>
            ))}
        </Flexbox>
      </Flexbox>
    </Flexbox>
  );
}
