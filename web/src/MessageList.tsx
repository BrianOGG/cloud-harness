import type { SessionMessage } from './api';
import { CopyButton, Markdown } from './Markdown';

const kindLabels: Record<SessionMessage['kind'], string> = {
  input: '你的消息',
  response: '助手回复',
  question: '助手提问',
  answer: '你的回答',
  approval_requested: '审批请求',
  approval_decision: '审批决定',
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
export function MessageList({ messages }: { messages: readonly SessionMessage[] }) {
  return (
      <ol className="history-list">
        {messages.map((message) => {
          const user = message.role === 'user';
          const system = message.role === 'system';
          const mappedApprovalDecision = message.kind === 'approval_decision'
            && (message.content === 'approve' || message.content === 'deny');
          const time = new Date(message.created_at);
          const validTime = Number.isFinite(time.getTime());
          const displayedTime = validTime ? timeFormat.format(time) : '';
          return (
            <li key={message.id} className={`history-message history-${message.role}`} data-message-id={message.id} data-run-id={message.run_id ?? undefined} data-kind={message.kind} data-status={message.status}>
              {!user && <div className="history-meta">
                {!system && <span>{kindLabels[message.kind]}</span>}
                {validTime && <time dateTime={message.created_at} title={displayedTime + '（上海时间）'}>{displayedTime}</time>}
              </div>}
              {user
                ? <div className="user-row"><div className="user-bubble">{message.content || '（空消息）'}{validTime && <time className="user-message-time" dateTime={message.created_at} title={displayedTime + '（上海时间）'} aria-label={'消息时间：' + displayedTime + '（上海时间）'} tabIndex={0}>{displayedTime}</time>}</div></div>
                : system
                  ? <div className={`history-fact ${message.status === 'failed' ? 'history-failed' : ''}`}><strong>{factLabel(message)}</strong>{!mappedApprovalDecision && <p>{message.content || '（空消息）'}</p>}</div>
                  : <div className="history-response"><Markdown text={message.content || '（空消息）'} /></div>}
              {message.role === 'assistant' && <div className="history-actions"><CopyButton text={message.content} /></div>}
            </li>
          );
        })}
      </ol>
  );
}
