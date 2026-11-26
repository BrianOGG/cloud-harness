import type { RunView, SessionMessage } from './api';
import { mergeSessionMessages } from './history';

export interface TranscriptTurn {
  id: string;
  root?: RunView;
  runs: RunView[];
  messages: SessionMessage[];
  conversation: SessionMessage[];
  process: SessionMessage[];
  finalAnswer?: string;
  toolCalls: number;
  approvals: number;
}

// One projection for live, refreshed and restored sessions. Identity comes from
// durable run/turn links, never text equality, timestamps or the last response.
export function projectTranscript(messages: readonly SessionMessage[], runs: readonly RunView[]): TranscriptTurn[] {
  const parents = new Map<string, string>();
  const find = (id: string): string => {
    const parent = parents.get(id);
    if (!parent) { parents.set(id, id); return id; }
    if (parent === id) return id;
    const root = find(parent);
    parents.set(id, root);
    return root;
  };
  const join = (a: string, b: string) => {
    const first = find(a), second = find(b);
    if (first !== second) parents.set(second, first);
  };
  const byRun = new Map(runs.map((run) => [run.id, run]));
  for (const run of runs) {
    find(`run:${run.id}`);
    if (run.turn_id) join(`run:${run.id}`, `turn:${run.turn_id}`);
    if (run.parent_id) join(`run:${run.parent_id}`, `run:${run.id}`);
  }
  for (const message of messages) {
    if (message.run_id && message.turn_id) join(`run:${message.run_id}`, `turn:${message.turn_id}`);
  }
  const groups = new Map<string, TranscriptTurn>();
  const group = (key: string) => {
    const id = find(key);
    let turn = groups.get(id);
    if (!turn) {
      turn = { id, runs: [], messages: [], conversation: [], process: [], toolCalls: 0, approvals: 0 };
      groups.set(id, turn);
    }
    return turn;
  };
  // The API history order is preserved within each turn, including audit facts.
  for (const message of mergeSessionMessages([], messages)) {
    group(message.run_id ? `run:${message.run_id}` : message.turn_id ? `turn:${message.turn_id}` : `message:${message.id}`).messages.push(message);
  }
  for (const run of runs) group(`run:${run.id}`).runs.push(run);
  return [...groups.values()].map((turn) => {
    const roots = turn.runs.filter((run) => !run.parent_id);
    // A missing parent is not permission to promote a child to root.
    turn.root = roots.length === 1 ? roots[0] : undefined;
    // Prefer the root's stable turn identity, including when children have
    // their own turn IDs. No refresh/version key may remount an expanded turn.
    turn.id = turn.root ? `run:${turn.root.id}` : turn.id;
    turn.conversation = turn.messages.filter((message) => (
      message.kind === 'question' || message.kind === 'answer'
      || (message.kind === 'input' && !(message.run_id && byRun.get(message.run_id)?.parent_id))
    ));
    const visible = new Set(turn.conversation.map((message) => message.id));
    turn.process = turn.messages.filter((message) => !visible.has(message.id));
    if (turn.root?.status === 'COMPLETED' && turn.root.final_answer?.trim()) turn.finalAnswer = turn.root.final_answer;
    turn.toolCalls = turn.runs.reduce((total, run) => total + run.budget.used_tool_calls, 0);
    turn.approvals = turn.messages.filter((message) => message.kind === 'approval_requested').length;
    return turn;
  });
}

export function turnSummary(turn: TranscriptTurn): string {
  const root = turn.root;
  let state = '历史过程 · 运行状态未确认';
  if (root?.status === 'COMPLETED') state = turn.finalAnswer ? '工作已完成' : '运行已完成 · 无最终答复';
  else if (root?.status === 'FAILED') state = '运行失败';
  else if (root?.status === 'CANCELLED') state = '运行已取消';
  else if (root?.status === 'WAITING') state = '等待处理 · 最终答复尚未生成';
  else if (root?.status === 'RUNNING') state = '正在运行 · 最终答复尚未生成';
  return `${state} · ${turn.toolCalls} 次工具调用 · ${turn.approvals} 次审批`;
}
