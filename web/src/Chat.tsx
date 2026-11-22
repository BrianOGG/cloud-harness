import { useEffect, useRef, useState } from 'react';
import type { KeyboardEvent } from 'react';
import type { HarnessEvent, RunView } from './api';
import { actionLabels, isActive, shortId, statusName, statusTone, waitLabels } from './format';
import { Icon } from './Icon';
import { CopyButton, Markdown } from './Markdown';
import { buildWork } from './timeline';
import type { WorkItem } from './timeline';

export interface Exchange {
  q: string;
  a: string;
}

export interface TurnActions {
  respond: (runId: string, body: { decision?: string; answer?: string }) => void;
  addBudget: (runId: string) => void;
  inspect: (runId: string) => void;
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

function WorkBlock({ run, events, onOpen }: { run: RunView; events: HarnessEvent[]; onOpen: () => void }) {
  const active = isActive(run);
  // Open while the run is in flight; a finished run opens on request.
  const [toggled, setToggled] = useState<boolean | null>(null);
  const open = toggled ?? active;
  const work = buildWork(events);
  const last = work.items[work.items.length - 1];
  let title = `${run.budget.used_model_calls} 次模型调用`;
  if (run.budget.used_tool_calls) title += ` · ${run.budget.used_tool_calls} 次工具调用`;
  if (run.budget.used_child_runs) title += ` · ${run.budget.used_child_runs} 个子任务`;
  if (run.status === 'RUNNING') {
    title = last?.kind === 'tool' && statusTone(last.status) === 'run' ? `正在运行 ${last.name}` : '正在思考';
  } else if (run.status === 'WAITING') {
    title = waitLabels[run.wait_reason || ''] || '等待处理';
  }
  const toggle = () => {
    if (!open) onOpen();
    setToggled(!open);
  };

  return (
    <div className={`work ${open ? 'open' : ''}`}>
      <button type="button" className="work-toggle" aria-expanded={open} onClick={toggle}>
        <span className={`work-state tone-${statusTone(run.status)}`}>
          {run.status === 'RUNNING' ? <span className="spinner" /> : <Icon name={run.status === 'COMPLETED' ? 'check' : run.status === 'WAITING' ? 'question' : 'alert'} size={13} />}
        </span>
        <span className={run.status === 'RUNNING' ? 'shimmer' : ''}>{title}</span>
        <span className="work-caret"><Icon name="right" size={13} /></span>
      </button>
      {open && work.items.length > 0 && (
        <ul className="work-list">
          {work.items.map((item) => <WorkRow key={`${item.kind}-${item.seq}-${item.kind === 'tool' ? item.key : ''}`} item={item} />)}
        </ul>
      )}
    </div>
  );
}

function WaitCard({ run, acting, actions }: { run: RunView; acting: boolean; actions: TurnActions }) {
  const pending = run.pending_invocation;
  if (run.wait_reason === 'APPROVAL') {
    return (
      <div className="wait-card" role="group" aria-label="等待审批">
        <div className="wait-head"><Icon name="shield" /><strong>需要你的批准</strong><code>{pending?.tool || pending?.name || '工具调用'}</code></div>
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
        <Markdown text={run.question || '运行正在等待你提供更多信息。'} />
        <p className="question-hint"><Icon name="question" size={14} />在下方输入框回答后，运行会继续。</p>
      </div>
    );
  }
  if (run.wait_reason === 'BUDGET') {
    return (
      <div className="wait-card" role="group" aria-label="等待补充预算">
        <div className="wait-head"><Icon name="alert" /><strong>模型调用预算已用完</strong></div>
        <p className="wait-text">运行已暂停，进度不会丢失。补充预算后继续。</p>
        <div className="wait-actions"><button type="button" className="button primary" disabled={acting} onClick={() => actions.addBudget(run.id)}>增加 10 次模型调用</button></div>
      </div>
    );
  }
  if (run.wait_reason === 'RECONCILIATION') {
    return (
      <div className="wait-card" role="group" aria-label="等待确认工具结果">
        <div className="wait-head"><Icon name="alert" /><strong>有一次工具调用的结果未知</strong></div>
        <p className="wait-text">serve 不会自动重放这次调用。请确认它实际的结果。</p>
        <div className="wait-actions">
          <button type="button" className="button" disabled={acting} onClick={() => actions.respond(run.id, { decision: 'reconcile_failure' })}>标记失败</button>
          <button type="button" className="button primary" disabled={acting} onClick={() => actions.respond(run.id, { decision: 'reconcile_success' })}>标记成功</button>
        </div>
      </div>
    );
  }
  return null;
}

export function Turn({ run, message, exchanges, events, live, acting, actions }: {
  run: RunView;
  message?: string;
  exchanges: Exchange[];
  events: HarnessEvent[];
  live: string;
  acting: boolean;
  actions: TurnActions;
}) {
  const text = run.message || message;
  return (
    <article className="turn" data-run-id={run.id} data-status={run.status}>
      <div className="user-row">
        {text
          ? <div className="user-bubble">{text}</div>
          : <div className="user-bubble missing">这条消息不是在本浏览器发送的，原文没有保存。</div>}
      </div>
      <div className="assistant">
        <WorkBlock run={run} events={events} onOpen={() => actions.loadEvents(run)} />
        {exchanges.map((exchange, index) => (
          <div className="exchange" key={index}>
            {exchange.q && <Markdown text={exchange.q} />}
            <div className="user-row"><div className="user-bubble">{exchange.a}</div></div>
          </div>
        ))}
        {run.status === 'WAITING' && <WaitCard run={run} acting={acting} actions={actions} />}
        {run.status === 'RUNNING' && live && <div className="streaming"><Markdown text={live} /></div>}
        {run.final_answer && <div className="answer"><Markdown text={run.final_answer} /></div>}
        {run.status === 'FAILED' && (
          <div className="error-card" role="alert">
            <strong>运行失败{run.error?.code ? ` · ${run.error.code}` : ''}</strong>
            <p>{run.error?.message || '发生未知错误。'}</p>
          </div>
        )}
        {run.status === 'CANCELLED' && <p className="cancelled">已取消</p>}
        {!isActive(run) && (
          <div className="turn-actions">
            {run.final_answer && <CopyButton text={run.final_answer} />}
            <button type="button" className="ghost-button" onClick={() => actions.inspect(run.id)}><Icon name="panel" size={14} /><span>运行详情</span></button>
          </div>
        )}
      </div>
    </article>
  );
}

export function PendingTurn({ text }: { text: string }) {
  return (
    <article className="turn" data-status="SENDING">
      <div className="user-row"><div className="user-bubble">{text}</div></div>
      <div className="assistant">
        <div className="work"><div className="work-toggle static"><span className="work-state tone-run"><span className="spinner" /></span><span className="shimmer">正在思考</span></div></div>
      </div>
    </article>
  );
}

export function Composer({ value, onChange, onSubmit, onStop, busy, stopping, placeholder, hint, autoFocus }: {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  onStop?: () => void;
  busy: boolean;
  stopping?: boolean;
  placeholder: string;
  hint?: string;
  autoFocus?: boolean;
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
        <span className="composer-hint">{hint}</span>
        {busy && onStop
          ? <button type="button" className="send stop" onClick={onStop} disabled={stopping} aria-label="停止运行" title="停止运行"><Icon name="stop" /></button>
          : <button type="button" className="send" onClick={onSubmit} disabled={busy || !value.trim()} aria-label="发送" title="发送（Enter）"><Icon name="up" /></button>}
      </div>
    </div>
  );
}
