import { useEffect, useRef, useState } from 'react';
import type { KeyboardEvent } from 'react';
import { api } from './api';
import type { BudgetExtension, HarnessEvent, ModelOption, ReasoningEffort, RunView, TraceView } from './api';
import { ApprovalModeMenu } from './ApprovalMode';
import { ReasoningEffortPicker } from './ReasoningEffort';
import { approvalModeLabel, approvalOperation, approvalWaitReason, approvalWorkingDirectory } from './approval';
import { MessageList } from './MessageList';
import { SkillInput } from './SkillInput';
import { turnSummary } from './transcript';
import type { TranscriptTurn } from './transcript';
import { budgetExtension } from './budget';
import { actionLabels, errorText, isActive, statusName, statusTone } from './format';
import { Icon } from './Icon';
import { Markdown } from './Markdown';
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
        <span className={`chip tone-${statusTone(item.status)}`}>{statusName(item.status)}</span>
      </li>
    );
  }
  return <li className="work-row"><span className="work-icon"><Icon name="right" size={14} /></span><span className="work-label">{item.label}</span></li>;
}

function WorkBlock({ turn, events, live, acting, actions, approvalMode }: { turn: TranscriptTurn; events: Record<string, HarnessEvent[]>; live: Record<string, string>; acting: Record<string, boolean>; actions: TurnActions; approvalMode?: string }) {
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
  const primary = turn.root || turn.runs[0];
  const active = turn.runs.filter(isActive);
  return (
    <div className={'work ' + (open ? 'open' : '')}>
      <div className="work-status-row">
        <button type="button" className="work-toggle" aria-expanded={open} onClick={() => {
          if (!open) turn.runs.forEach((run) => actions.loadEvents(run));
          setOpen(!open);
        }}>
          <span className={'work-state tone-' + statusTone(status)}>
            {status === 'RUNNING' ? <span className="spinner" /> : <Icon name={status === 'COMPLETED' ? 'check' : status === 'WAITING' ? 'question' : 'alert'} size={13} />}
          </span>
          <span className="work-summary">{turnSummary(turn)}</span><span className="work-caret"><Icon name="right" size={13} /></span>
        </button>
        {active.filter((run) => approvalMode && run.approval_mode && run.approval_mode !== approvalMode).map((run) => {
          const description = (run.parent_id ? '子任务' : '本轮') + '使用「' + approvalModeLabel(run.approval_mode) + '」，下一轮为「' + approvalModeLabel(approvalMode) + '」。当前选择不会改变本轮。';
          return <span key={run.id} className="mode-difference" role="img" tabIndex={0} title={description} aria-label={description}><Icon name="shield" size={13} /><span className="mode-tooltip" aria-hidden="true">{description}</span></span>;
        })}
        <div className="turn-actions">
          {primary && <button type="button" className="icon-button" aria-label="运行详情" title="运行详情" onClick={() => actions.inspect(primary.id)}><Icon name="panel" size={14} /></button>}
          {active.map((run, index) => {
            const label = run.parent_id ? '取消子任务 ' + (index + 1) : '取消运行';
            return <button key={run.id} type="button" className="icon-button" aria-label={label} title={label} disabled={acting[run.id]} onClick={() => actions.cancel(run.id)}><Icon name="stop" size={12} /></button>;
          })}
        </div>
      </div>
      {open && <div className="turn-process" aria-label="本轮过程审计">
        <MessageList messages={turn.process} />
        {turn.runs.map((run, index) => {
          const log = events[run.id] || [];
          const work = buildWork(log);
          const trace = traces[run.id];
          const calls = (trace?.data?.run?.steps || []).flatMap((step) => (step.batches || []).flatMap((batch) => batch.calls || []));
          return <section className="process-run" key={run.id}>
            <h3>{run.parent_id ? '子任务 ' + (index + 1) : '本轮'} · {statusName(run.status)}</h3>
            {work.items.length > 0 && <ul className="work-list">{work.items.map((item) => <WorkRow key={item.kind + '-' + item.seq + '-' + (item.kind === 'tool' ? item.key : '')} item={item} />)}</ul>}
            {run.status === 'RUNNING' && live[run.id] && <div className="live-draft"><strong>实时草稿（非最终答复）</strong><div className="streaming"><Markdown text={live[run.id]} /></div></div>}
            {trace?.error && <p className="history-error" role="alert">工具明细加载失败：{trace.error}<button type="button" className="ghost-button" onClick={() => setRetry((value) => value + 1)}>重试工具明细</button></p>}
            {!trace && <p className="process-note" role="status">正在加载工具参数与结果…</p>}
            {calls.map((call) => <details className="process-call" key={call.id}><summary><code>{call.name}</code> · {statusName(call.status)}</summary><p className="process-note">调用 {call.id}</p>{call.arguments !== undefined && <><strong>参数</strong><pre>{JSON.stringify(call.arguments, null, 2)}</pre></>}{call.result !== undefined && <><strong>结果</strong><pre>{JSON.stringify(call.result, null, 2)}</pre></>}</details>)}
            {log.length > 0 ? <details className="process-event-log"><summary>事件 · {log.length} 条</summary><ol>{log.map((event) => <li key={event.seq}><code>#{event.seq} {event.type}</code><pre>{event.data}</pre></li>)}</ol></details> : <p className="process-note">暂无事件。</p>}
            {run.id !== primary?.id && <button type="button" className="ghost-button" onClick={() => actions.inspect(run.id)}><Icon name="panel" size={14} /><span>子任务详情</span></button>}
          </section>;
        })}
      </div>}
    </div>
  );
}

function WaitCard({ run, acting, actions, questionVisible = false }: { run: RunView; acting: boolean; actions: TurnActions; questionVisible?: boolean }) {
  const pending = run.pending_invocation;
  if (run.wait_reason === 'APPROVAL') {
    if (!pending) return <div className="wait-card" role="status">正在同步待批操作…</div>;
    const args = pending.arguments && typeof pending.arguments === 'object' && !Array.isArray(pending.arguments) ? pending.arguments as Record<string, unknown> : {};
    const cwd = approvalWorkingDirectory(run.workspace, args.cwd);
    return (
      <div className="wait-card" role="group" aria-label="等待审批">
        <div className="wait-head"><Icon name="shield" /><strong>需要你的批准</strong></div>
        <code className="wait-operation" tabIndex={0}>{approvalOperation(pending)}</code>
        {cwd && <p className="wait-cwd">工作目录 <code>{cwd}</code></p>}
        <p className="wait-text">{approvalWaitReason()}</p>
        {(pending?.warnings ?? []).map((warning) => <p key={warning} className="wait-warning" role="alert"><Icon name="alert" size={14} />{warning}</p>)}
        {pending.arguments !== undefined && <details className="wait-details"><summary>详情</summary><pre className="wait-args">{JSON.stringify(pending.arguments, null, 2)}</pre></details>}
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
          <Icon name="question" size={14} /><span>等待你的回答</span>
          <button type="button" className="ghost-button" disabled={acting} onClick={() => actions.focusAnswer(run.id)}>回答这个问题</button>
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
        <p className="wait-text">请确认实际结果；此调用不会自动重试。</p>
        <div className="wait-actions">
          <button type="button" className="button" disabled={acting} onClick={() => actions.respond(run.id, { decision: 'reconcile_fail' })}>标记失败</button>
          <button type="button" className="button primary" disabled={acting} onClick={() => actions.respond(run.id, { decision: 'reconcile_success' })}>标记成功</button>
        </div>
      </div>
    );
  }
  return null;
}

export function Turn({ turn, events, live, acting, actions, approvalMode }: {
  turn: TranscriptTurn;
  events: Record<string, HarnessEvent[]>;
  live: Record<string, string>;
  acting: Record<string, boolean>;
  actions: TurnActions;
  approvalMode?: string;
}) {
  const root = turn.root;
  return (
    <article className="turn run-turn" data-run-id={root?.id} data-turn-id={turn.id} data-status={root?.status} aria-label="对话轮次">
      <MessageList messages={turn.conversation} />
      <div className="assistant">
        {turn.finalAnswer && <div className="turn-final" aria-label="Harness 最终答复"><Markdown text={turn.finalAnswer} copyReply /></div>}
        <WorkBlock turn={turn} events={events} live={live} acting={acting} actions={actions} approvalMode={approvalMode} />
        {root?.status === 'FAILED' && <p className="run-error" role="alert">{root.error?.message || root.error?.code || '未生成最终答复，请查看运行详情。'}</p>}
        {turn.runs.filter((run) => run.status === 'WAITING' && ['APPROVAL', 'USER', 'BUDGET', 'RECONCILIATION'].includes(run.wait_reason || '')).map((run) => <section key={run.id} className="current-run" data-run-id={run.id} aria-label={(run.parent_id ? '子任务' : '本轮') + '当前操作'}>
          <WaitCard run={run} acting={Boolean(acting[run.id])} actions={actions} questionVisible={turn.conversation.some((message) => message.run_id === run.id && message.kind === 'question' && message.content === run.question)} />
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

export function Composer({ value, onChange, onSubmit, onStop, busy, stopping, placeholder, skillsEnabled, hint, autoFocus, approvalMode, onApprovalModeChange, preferencesBusy, models, selectedModel, modelsLoading, modelSelectionDisabled, onModelChange, reasoningEffort, onReasoningChange }: {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  onStop?: () => void;
  busy: boolean;
  stopping?: boolean;
  placeholder: string;
  skillsEnabled?: boolean;
  hint?: string;
  autoFocus?: boolean;
  approvalMode: string;
  onApprovalModeChange: (value: string) => void;
  preferencesBusy?: boolean;
  models: ModelOption[];
  selectedModel?: ModelOption;
  modelsLoading?: boolean;
  modelSelectionDisabled?: boolean;
  onModelChange: (id: string) => void;
  reasoningEffort?: ReasoningEffort;
  onReasoningChange: (value: ReasoningEffort) => void;
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
      <SkillInput inputRef={ref} value={value} placeholder={placeholder} enabled={skillsEnabled} onChange={onChange} onKeyDown={onKeyDown} />
      <div className="composer-bar">
        <ApprovalModeMenu value={approvalMode} onChange={onApprovalModeChange} disabled={preferencesBusy} />
        {hint && <span className="composer-hint">{hint}</span>}
        <div className="composer-controls-right">
          <ReasoningEffortPicker models={models} selectedModel={selectedModel} modelsLoading={modelsLoading} modelSelectionDisabled={modelSelectionDisabled} value={reasoningEffort} onModelChange={onModelChange} onChange={onReasoningChange} disabled={preferencesBusy} />
          {busy && onStop
            ? <button type="button" className="send stop" onClick={onStop} disabled={stopping} aria-label="停止运行" title="停止运行"><Icon name="stop" /></button>
            : <button type="button" className="send" onClick={onSubmit} disabled={busy || !value.trim()} aria-label="发送" title="发送"><Icon name="up" /></button>}
        </div>
      </div>
    </div>
  );
}
