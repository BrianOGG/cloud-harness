import { useEffect, useRef, useState } from 'react';
import type { KeyboardEvent } from 'react';
import { api } from './api';
import type { BudgetExtension, HarnessEvent, RunView, TraceView } from './api';
import { ApprovalModeMenu } from './ApprovalMode';
import { approvalModeLabel, approvalWaitReason } from './approval';
import { MessageList } from './MessageList';
import { turnSummary } from './transcript';
import type { TranscriptTurn } from './transcript';
import { budgetExtension } from './budget';
import { actionLabels, errorText, isActive, shortId, statusName, statusTone } from './format';
import { Icon } from './Icon';
import { CopyButton, Markdown } from './Markdown';
import { buildWork } from './timeline';
import type { WorkItem } from './timeline';

export interface TurnActions {
  respond: (runId: string, body: { decision?: string; answer?: string }) => void;
  addBudget: (runId: string, extension: BudgetExtension) => void;
  cancel: (runId: string) => void;
  inspect: (runId: string) => void;
  focusAnswer: (runId: string) => void;
  loadEvents: (run: RunView) => void;
}

function WorkRow({ item }: { item: WorkItem }) {
  if (item.kind === 'model') {
    return (
      <li className="work-row">
        <span className="work-icon"><Icon name="spark" size={14} /></span>
        <span className="work-label">{actionLabels[item.action] || item.action || '模型响应'}</span>
        <span className="work-meta">{item.inputTokens + item.outputTokens} tokens · {item.latencyMs} ms</span>
      </li>
    );
  }
  if (item.kind === 'tool') {
    return (
      <li className="work-row">
        <span className="work-icon"><Icon name="tool" size={14} /></span>
        <code className="work-tool">{item.name || '未知工具'}</code>
        <span className={`chip tone-${statusTone(item.status)}`}>{statusName(item.status)}</span>
      </li>
    );
  }
  if (item.kind === 'child') {
    return (
      <li className="work-row">
        <span className="work-icon"><Icon name="branch" size={14} /></span>
        <span className="work-label">子任务{item.agent ? ` · ${item.agent}` : ''}</span>
        <span className="work-meta mono">{shortId(item.runId)}</span>
        <span className={`chip tone-${statusTone(item.status)}`}>{statusName(item.status)}</span>
      </li>
    );
  }
  return <li className="work-row"><span className="work-icon"><Icon name="right" size={14} /></span><span className="work-label">{item.label}</span></li>;
}

function WorkBlock({ turn, events, live, actions }: { turn: TranscriptTurn; events: Record<string, HarnessEvent[]>; live: Record<string, string>; actions: TurnActions }) {
  const [open, setOpen] = useState(false);
  const [traces, setTraces] = useState<Record<string, { data?: TraceView; error?: string }>>({});
  const [retry, setRetry] = useState(0);
  const versions = JSON.stringify(turn.runs.map((run) => [run.id, run.version]));
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    for (const [id] of JSON.parse(versions) as [string, number][]) {
      api.getTrace(id).then((data) => {
        if (!cancelled) setTraces((previous) => ({ ...previous, [id]: { data } }));
      }).catch((reason: unknown) => {
        if (!cancelled) setTraces((previous) => ({ ...previous, [id]: { error: errorText(reason) } }));
      });
    }
    return () => { cancelled = true; };
  }, [open, versions, retry]);
  const status = turn.root?.status || '';
  return (
    <div className={'work ' + (open ? 'open' : '')}>
      <button type="button" className="work-toggle" aria-expanded={open} onClick={() => {
        if (!open) turn.runs.forEach((run) => actions.loadEvents(run));
        setOpen(!open);
      }}>
        <span className={'work-state tone-' + statusTone(status)}>
          {status === 'RUNNING' ? <span className="spinner" /> : <Icon name={status === 'COMPLETED' ? 'check' : status === 'WAITING' ? 'question' : 'alert'} size={13} />}
        </span>
        <span>{turnSummary(turn)}</span><span className="work-caret"><Icon name="right" size={13} /></span>
      </button>
      {open && <div className="turn-process" aria-label="本轮过程审计">
        <p className="process-note">完整历史按原始顺序保留；模型中间文本与子运行输出仅供过程查看。审批请求和决定属于同次调用，不是新的待办。</p>
        <MessageList messages={turn.process} onInspect={actions.inspect} />
        {turn.runs.map((run) => {
          const log = events[run.id] || [];
          const work = buildWork(log);
          const trace = traces[run.id];
          const calls = (trace?.data?.run?.steps || []).flatMap((step) => (step.batches || []).flatMap((batch) => batch.calls || []));
          return <section className="process-run" key={run.id}>
            <h3>{run.parent_id ? '子运行' : '根运行'} {shortId(run.id)} · {statusName(run.status)}</h3>
            <p className="process-note">本轮快照：{approvalModeLabel(run.approval_mode)}{run.workspace ? ' · 工作区 ' + run.workspace : ''}</p>
            {work.items.length > 0 && <ul className="work-list">{work.items.map((item) => <WorkRow key={item.kind + '-' + item.seq + '-' + (item.kind === 'tool' ? item.key : '')} item={item} />)}</ul>}
            {run.status === 'RUNNING' && live[run.id] && <div className="live-draft"><strong>实时草稿（非最终答复）</strong><div className="streaming"><Markdown text={live[run.id]} /></div></div>}
            {trace?.error && <p className="history-error" role="alert">工具明细加载失败：{trace.error}<button type="button" className="ghost-button" onClick={() => setRetry((value) => value + 1)}>重试工具明细</button></p>}
            {!trace && <p className="process-note" role="status">正在加载工具参数与结果…</p>}
            {calls.map((call) => <details className="process-call" key={call.id}><summary><code>{call.name}</code> · {statusName(call.status)}</summary><p className="process-note">调用 {call.id}</p>{call.arguments !== undefined && <><strong>参数</strong><pre>{JSON.stringify(call.arguments, null, 2)}</pre></>}{call.result !== undefined && <><strong>结果</strong><pre>{JSON.stringify(call.result, null, 2)}</pre></>}</details>)}
            {log.length > 0 ? <details className="process-event-log"><summary>规则来源与状态事件 · {log.length} 条</summary><ol>{log.map((event) => <li key={event.seq}><code>#{event.seq} {event.type}</code><pre>{event.data}</pre></li>)}</ol></details> : <p className="process-note">详细事件尚未加载；可从运行详情查看或重新展开。</p>}
            <button type="button" className="ghost-button" onClick={() => actions.inspect(run.id)}><Icon name="panel" size={14} /><span>运行详情</span></button>
          </section>;
        })}
      </div>}
    </div>
  );
}

function WaitCard({ run, acting, actions, questionVisible = false }: { run: RunView; acting: boolean; actions: TurnActions; questionVisible?: boolean }) {
  const pending = run.pending_invocation;
  if (run.wait_reason === 'APPROVAL') {
    if (!pending) return <div className="wait-card" role="status">正在同步待批操作的权威详情，请刷新运行状态。</div>;
    return (
      <div className="wait-card" role="group" aria-label="等待审批">
        <div className="wait-head"><Icon name="shield" /><strong>需要你的批准</strong><code>{pending?.tool || pending?.name || '工具调用'}</code></div>
        <p className="wait-text">{approvalWaitReason(run.approval_mode, pending.tool || pending.name)}</p>
        <dl className="wait-context">
          <dt>本轮工作区</dt><dd>{run.workspace || '快照未提供'}</dd>
          <dt>工作目录参数</dt><dd>{pending.arguments && typeof pending.arguments === 'object' && 'cwd' in pending.arguments && typeof pending.arguments.cwd === 'string' && pending.arguments.cwd ? pending.arguments.cwd + '（相对于本轮工作区解析；绝对路径仍由后端校验）' : run.workspace || '快照未提供，请核对调用参数'}</dd>
          <dt>资源范围</dt><dd>{pending.resources?.join(' · ') || '见调用参数；未提供独立资源摘要'}</dd>
        </dl>
        {(pending?.warnings ?? []).map((warning) => <p key={warning} className="wait-warning" role="alert"><Icon name="alert" size={14} />{warning}</p>)}
        {pending?.arguments !== undefined && <pre className="wait-args">{JSON.stringify(pending.arguments, null, 2)}</pre>}
        <div className="wait-actions">
          <button type="button" className="button" disabled={acting} onClick={() => actions.respond(run.id, { decision: 'deny' })}>拒绝</button>
          <button type="button" className="button primary" disabled={acting} onClick={() => actions.respond(run.id, { decision: 'approve' })}>{acting ? '提交中…' : '批准并继续'}</button>
        </div>
      </div>
    );
  }
  if (run.wait_reason === 'USER') {
    return (
      <div className="question">
        {!questionVisible && run.question && <div className="current-question"><Markdown text={run.question} /></div>}
        <p className="question-hint">
          <Icon name="question" size={14} /><span>正在等待你的回答，问题见会话历史。</span>
          <button type="button" className="ghost-button" disabled={acting} onClick={() => actions.focusAnswer(run.id)}>回答这个问题</button>
          <button type="button" className="ghost-button" disabled={acting} onClick={() => actions.cancel(run.id)}><Icon name="stop" size={12} /><span>取消运行</span></button>
        </p>
      </div>
    );
  }
  if (run.wait_reason === 'BUDGET') {
    const { title, label, extension } = budgetExtension(run.budget);
    return (
      <div className="wait-card" role="group" aria-label="等待补充预算">
        <div className="wait-head"><Icon name="alert" /><strong>{title}</strong></div>
        <p className="wait-text">运行已暂停，进度不会丢失。补充预算后继续。</p>
        <div className="wait-actions"><button type="button" className="button primary" disabled={acting} onClick={() => actions.addBudget(run.id, extension)}>{label}</button></div>
      </div>
    );
  }
  if (run.wait_reason === 'RECONCILIATION') {
    return (
      <div className="wait-card" role="group" aria-label="等待确认工具结果">
        <div className="wait-head"><Icon name="alert" /><strong>有一次工具调用的结果未知</strong></div>
        <p className="wait-text">serve 不会自动重放这次调用。请确认它实际的结果。</p>
        <div className="wait-actions">
          <button type="button" className="button" disabled={acting} onClick={() => actions.respond(run.id, { decision: 'reconcile_fail' })}>标记失败</button>
          <button type="button" className="button primary" disabled={acting} onClick={() => actions.respond(run.id, { decision: 'reconcile_success' })}>标记成功</button>
        </div>
      </div>
    );
  }
  return null;
}

export function Turn({ turn, events, live, acting, actions }: {
  turn: TranscriptTurn;
  events: Record<string, HarnessEvent[]>;
  live: Record<string, string>;
  acting: Record<string, boolean>;
  actions: TurnActions;
}) {
  const root = turn.root;
  return (
    <article className="turn run-turn" data-run-id={root?.id} data-turn-id={turn.id} data-status={root?.status} aria-label={root ? '运行 ' + shortId(root.id) : '历史轮次'}>
      <MessageList messages={turn.conversation} onInspect={actions.inspect} />
      <div className="assistant">
        {turn.finalAnswer && <div className="turn-final" aria-label="Harness 最终答复"><Markdown text={turn.finalAnswer} /><CopyButton text={turn.finalAnswer} /></div>}
        {root?.status === 'FAILED' && <div className="error-card" role="alert"><strong>运行失败</strong><p>{root.error?.message || root.error?.code || '本轮未生成最终答复。请查看过程或运行详情。'}</p></div>}
        {root?.status === 'CANCELLED' && <p className="cancelled">本轮已取消，未生成最终答复。</p>}
        <WorkBlock turn={turn} events={events} live={live} actions={actions} />
        {turn.runs.filter(isActive).map((run) => <section key={run.id} className="current-run" data-run-id={run.id} aria-label={(run.parent_id ? '子运行' : '本轮') + '当前操作'}>
          <p className="run-snapshot">{run.parent_id ? '子运行 ' + shortId(run.id) : '本轮'}实际生效 · {approvalModeLabel(run.approval_mode)}<span>（运行快照；下一轮选择不会改变此轮）</span></p>
          {run.status === 'WAITING' && <WaitCard run={run} acting={Boolean(acting[run.id])} actions={actions} questionVisible={turn.conversation.some((message) => message.run_id === run.id && message.kind === 'question' && message.content === run.question)} />}
          <div className="turn-actions">
            <button type="button" className="ghost-button" onClick={() => actions.inspect(run.id)}><Icon name="panel" size={14} /><span>运行详情</span></button>
            {run.wait_reason !== 'USER' && <button type="button" className="ghost-button" disabled={acting[run.id]} onClick={() => actions.cancel(run.id)}><Icon name="stop" size={12} /><span>取消运行</span></button>}
          </div>
        </section>)}
      </div>
    </article>
  );
}

export function PendingTurn({ text }: { text: string }) {
  return (
    <article className="turn" data-status="SENDING">
      <div className="user-row"><div className="user-bubble">{text}</div></div>
      <div className="assistant">
        <div className="work" role="status"><div className="work-toggle static"><span className="work-state tone-run"><span className="spinner" /></span><span className="shimmer">正在发送…</span></div></div>
      </div>
    </article>
  );
}

export function Composer({ value, onChange, onSubmit, onStop, busy, stopping, placeholder, hint, autoFocus, approvalMode, onApprovalModeChange, preferencesBusy }: {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  onStop?: () => void;
  busy: boolean;
  stopping?: boolean;
  placeholder: string;
  hint?: string;
  autoFocus?: boolean;
  approvalMode: string;
  onApprovalModeChange: (value: string) => void;
  preferencesBusy?: boolean;
}) {
  const ref = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    const node = ref.current;
    if (!node) return;
    node.style.height = 'auto';
    node.style.height = `${Math.min(node.scrollHeight, 220)}px`;
    node.style.overflowY = node.scrollHeight > 220 ? 'auto' : 'hidden';
  }, [value]);
  useEffect(() => {
    if (autoFocus) ref.current?.focus();
  }, [autoFocus]);
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    // Enter confirms an IME candidate while composing; it must not send.
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    if (!busy && value.trim()) onSubmit();
  };
  return (
    <div className="composer">
      <textarea ref={ref} rows={1} value={value} placeholder={placeholder} aria-label={placeholder} onChange={(event) => onChange(event.target.value)} onKeyDown={onKeyDown} />
      <div className="composer-bar">
        <ApprovalModeMenu value={approvalMode} onChange={onApprovalModeChange} disabled={preferencesBusy} />
        <span className="composer-hint">{hint}</span>
        {busy && onStop
          ? <button type="button" className="send stop" onClick={onStop} disabled={stopping} aria-label="停止运行" title="停止运行"><Icon name="stop" /></button>
          : <button type="button" className="send" onClick={onSubmit} disabled={busy || !value.trim()} aria-label="发送" title="发送（Enter）"><Icon name="up" /></button>}
      </div>
    </div>
  );
}
