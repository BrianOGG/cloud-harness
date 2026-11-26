import { useEffect, useRef, useState } from 'react';
import type { KeyboardEvent } from 'react';
import type { BudgetExtension, HarnessEvent, RunView } from './api';
import type { CustomProvider, ModelOption } from './api';
import { budgetExtension } from './budget';
import { actionLabels, isActive, shortId, statusName, statusTone, waitLabels } from './format';
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
        <span className="work-meta mono">{shortId(item.runId)}</span>
        <span className={`chip tone-${statusTone(item.status)}`}>{statusName(item.status)}</span>
      </li>
    );
  }
  return <li className="work-row"><span className="work-icon"><Icon name="right" size={14} /></span><span className="work-label">{item.label}</span></li>;
}

function WorkBlock({ run, events, onOpen }: { run: RunView; events: HarnessEvent[]; onOpen: () => void }) {
  // Work details stay collapsed until requested, even while the run is active.
  const [toggled, setToggled] = useState<boolean | null>(null);
  const open = toggled ?? false;
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

export function Turn({ run, events, live, acting, actions }: {
  run: RunView;
  events: HarnessEvent[];
  live: string;
  acting: boolean;
  actions: TurnActions;
}) {
  return (
    <article className="turn run-turn" data-run-id={run.id} data-status={run.status} aria-label={`运行 ${shortId(run.id)}`}>
      <div className="assistant">
        <span className="run-label">{run.parent_id ? '子运行' : '运行'} {shortId(run.id)}</span>
        <WorkBlock run={run} events={events} onOpen={() => actions.loadEvents(run)} />
        {run.status === 'WAITING' && <WaitCard run={run} acting={acting} actions={actions} />}
        {run.status === 'RUNNING' && live && <details className="live-draft"><summary>查看实时草稿</summary><div className="streaming"><Markdown text={live} /></div></details>}
        <div className="turn-actions">
          <button type="button" className="ghost-button" onClick={() => actions.inspect(run.id)}><Icon name="panel" size={14} /><span>运行详情</span></button>
          {isActive(run) && run.wait_reason !== 'USER' && <button type="button" className="ghost-button" disabled={acting} onClick={() => actions.cancel(run.id)}><Icon name="stop" size={12} /><span>取消运行</span></button>}
        </div>
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

export function Composer({ value, onChange, onSubmit, onStop, busy, stopping, placeholder, hint, autoFocus, models, modelId, onModelChange, approvalMode, onApprovalModeChange, providers, onSaveProvider, preferencesBusy }: {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  onStop?: () => void;
  busy: boolean;
  stopping?: boolean;
  placeholder: string;
  hint?: string;
  autoFocus?: boolean;
  models: ModelOption[];
  modelId: string;
  onModelChange: (value: string) => void;
  approvalMode: string;
  onApprovalModeChange: (value: string) => void;
  preferencesBusy?: boolean;
  providers: CustomProvider[];
  onSaveProvider: (value: { id: string; name: string; base_url: string; model: string; api_key?: string }) => Promise<void>;
}) {
  const ref = useRef<HTMLTextAreaElement>(null);
  const selectedProvider = providers.find((item) => 'custom:' + item.id === modelId);
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
      <div className="composer-options">
        <label>审批模式<select aria-label="审批模式" disabled={preferencesBusy} value={approvalMode} onChange={(event) => onApprovalModeChange(event.target.value)}><option value="ask">每次询问</option><option value="safe">自动放行安全操作</option><option value="workspace-write">自动批准工作区文件写入</option></select></label>
        <label>模型<select aria-label="模型" disabled={preferencesBusy} value={modelId} onChange={(event) => onModelChange(event.target.value)}>{models.map((model) => <option key={model.id} value={model.id}>{model.name}</option>)}</select></label>
        <ProviderEditor key={selectedProvider?.id || 'new'} selectedProvider={selectedProvider} onSaveProvider={onSaveProvider} preferencesBusy={preferencesBusy} />
      </div>
      <div className="composer-bar">
        <span className="composer-hint">{hint}</span>
        {busy && onStop
          ? <button type="button" className="send stop" onClick={onStop} disabled={stopping} aria-label="停止运行" title="停止运行"><Icon name="stop" /></button>
          : <button type="button" className="send" onClick={onSubmit} disabled={busy || !value.trim()} aria-label="发送" title="发送（Enter）"><Icon name="up" /></button>}
      </div>
    </div>
  );
}

function ProviderEditor({ selectedProvider, onSaveProvider, preferencesBusy }: {
 selectedProvider?: CustomProvider;
 onSaveProvider: Parameters<typeof Composer>[0]['onSaveProvider'];
 preferencesBusy?: boolean;
}) {
  const [providerId, setProviderId] = useState(selectedProvider?.id || '');
  const [providerName, setProviderName] = useState(selectedProvider?.name || '');
  const [providerURL, setProviderURL] = useState(selectedProvider?.base_url || '');
  const [providerModel, setProviderModel] = useState(selectedProvider?.model || '');
  const [providerKey, setProviderKey] = useState('');
  const [savingProvider, setSavingProvider] = useState(false);
  const [providerError, setProviderError] = useState('');
  const saveProvider = async () => {
    setSavingProvider(true); setProviderError('');
    try { await onSaveProvider({ id: providerId || crypto.randomUUID(), name: providerName, base_url: providerURL, model: providerModel, ...(providerKey ? { api_key: providerKey } : {}) }); setProviderKey(''); }
    catch (reason) { setProviderError(reason instanceof Error ? reason.message : '保存失败'); }
    finally { setSavingProvider(false); }
  };
  return (
        <details className="provider-editor"><summary>OpenAI 兼容设置</summary>
          <div className="provider-fields">
            <label>配置 ID<input value={providerId} onChange={(event) => setProviderId(event.target.value)} placeholder="自动生成" /></label>
            <label>显示名称<input value={providerName} onChange={(event) => setProviderName(event.target.value)} /></label>
            <label>Base URL<input value={providerURL} onChange={(event) => setProviderURL(event.target.value)} placeholder="https://api.example.com/v1" /></label>
            <label>模型名<input value={providerModel} onChange={(event) => setProviderModel(event.target.value)} /></label>
            <label>API key<input type="password" autoComplete="new-password" value={providerKey} onChange={(event) => setProviderKey(event.target.value)} placeholder={selectedProvider?.has_api_key ? '已保存；留空保留原值' : '输入 API key'} /></label>
            <button type="button" className="button" disabled={savingProvider || preferencesBusy} onClick={() => void saveProvider()}>{savingProvider ? '保存中…' : '保存并选择模型'}</button>
            {providerError && <span role="alert">{providerError}</span>}
          </div>
        </details>
  );
}
