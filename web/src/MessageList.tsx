import type { SessionMessage } from './api';
import { shortId } from './format';
import { Icon } from './Icon';
import { CopyButton, Markdown } from './Markdown';

const kindLabels: Record<SessionMessage['kind'], string> = {
  input: '你的消息',
  response: '助手回复',
  question: '助手提问 · 历史记录',
  answer: '你的回答',
  approval_requested: '审批请求 · 历史记录',
  approval_decision: '审批决定 · 历史记录',
  run_status: '运行结果',
};

const timeFormat = new Intl.DateTimeFormat('zh-CN', {
  timeZone: 'Asia/Shanghai', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
});

function factLabel(message: SessionMessage) {
  if (message.kind === 'approval_decision') {
    if (message.content === 'approve') return '已批准';
    if (message.content === 'deny') return '已拒绝';
  }
  if (message.kind === 'run_status') {
    if (message.status === 'failed') return '运行失败';
    if (message.status === 'cancelled') return '运行已取消';
    return '运行已完成';
  }
  return kindLabels[message.kind];
}

// Human dialogue, or the original audit inside a collapsed turn.
export function MessageList({ messages, onInspect }: { messages: readonly SessionMessage[]; onInspect: (runId: string) => void }) {
  return (
      <ol className="history-list">
        {messages.map((message) => {
          const user = message.role === 'user';
          const system = message.role === 'system';
          const mappedApprovalDecision = message.kind === 'approval_decision'
            && (message.content === 'approve' || message.content === 'deny');
          const time = new Date(message.created_at);
          return (
            <li key={message.id} className={`history-message history-${message.role}`} data-message-id={message.id} data-run-id={message.run_id ?? undefined} data-kind={message.kind} data-status={message.status}>
              <div className="history-meta">
                <span>{kindLabels[message.kind]}</span>
                {Number.isFinite(time.getTime()) && <time dateTime={message.created_at} title={`${message.created_at}（显示为上海时间）`}>{timeFormat.format(time)}</time>}
              </div>
              {user
                ? <div className="user-row"><div className="user-bubble">{message.content || '（空消息）'}</div></div>
                : system
                  ? <div className={`history-fact ${message.status === 'failed' ? 'history-failed' : ''}`}><strong>{factLabel(message)}</strong>{!mappedApprovalDecision && <p>{message.content || '（空消息）'}</p>}</div>
                  : <div className="history-response"><Markdown text={message.content || '（空消息）'} /></div>}
              <div className="history-actions">
                {message.role === 'assistant' && <CopyButton text={message.content} />}
                {message.run_id && <button type="button" className="ghost-button" onClick={() => { if (message.run_id) onInspect(message.run_id); }} aria-label={`查看运行 ${message.run_id} 的详情`}><Icon name="panel" size={13} /><span>运行 {shortId(message.run_id)}</span></button>}
              </div>
            </li>
          );
        })}
      </ol>
  );
}
