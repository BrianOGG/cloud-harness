// Turns a run's persistent events into the rows of the "work" block that a
// chat turn shows above its answer.
import type { HarnessEvent } from './api';

export type WorkItem =
  | { kind: 'model'; seq: number; action: string; inputTokens: number; outputTokens: number; latencyMs: number }
  | { kind: 'tool'; seq: number; key: string; name: string; status: string }
  | { kind: 'child'; seq: number; runId: string; agent: string; status: string }
  | { kind: 'note'; seq: number; label: string };

export interface Work {
  items: WorkItem[];
  modelCalls: number;
  toolCalls: number;
  inputTokens: number;
  outputTokens: number;
  latencyMs: number;
}

type Payload = Record<string, unknown>;

function payload(data: string): Payload {
  try {
    const value = JSON.parse(data) as unknown;
    return value && typeof value === 'object' && !Array.isArray(value) ? (value as Payload) : {};
  } catch {
    return {};
  }
}

const text = (value: unknown) => (typeof value === 'string' ? value : '');
const count = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) ? value : 0);

const notes: Record<string, string> = {
  'planner.selected': '规划器选定了上下文',
  'planner.completed': '规划完成',
  'planner.failed': '规划失败',
  'run.cancelled': '运行已取消',
};

export function buildWork(events: HarnessEvent[]): Work {
  const work: Work = { items: [], modelCalls: 0, toolCalls: 0, inputTokens: 0, outputTokens: 0, latencyMs: 0 };
  const tools = new Map<string, Extract<WorkItem, { kind: 'tool' }>>();
  const children = new Map<string, Extract<WorkItem, { kind: 'child' }>>();
  // Provider call ids are only unique within one model response.
  let response = 0;

  const setTool = (seq: number, callId: string, name: string, status: string) => {
    const key = `${response}:${callId || name}`;
    const known = tools.get(key);
    if (known) {
      if (status) known.status = status;
      if (name && !known.name) known.name = name;
      return;
    }
    const item = { kind: 'tool' as const, seq, key, name, status };
    tools.set(key, item);
    work.items.push(item);
    work.toolCalls += 1;
  };

  for (const event of [...events].sort((a, b) => a.seq - b.seq)) {
    const data = payload(event.data);
    if (event.type === 'model.completed') {
      response += 1;
      const item = {
        kind: 'model' as const,
        seq: event.seq,
        action: text(data.action),
        inputTokens: count(data.input_tokens),
        outputTokens: count(data.output_tokens),
        latencyMs: count(data.latency_ms),
      };
      work.items.push(item);
      work.modelCalls += 1;
      work.inputTokens += item.inputTokens;
      work.outputTokens += item.outputTokens;
      work.latencyMs += item.latencyMs;
    } else if (event.type.startsWith('invocation.')) {
      setTool(event.seq, text(data.provider_call_id), text(data.tool), text(data.status) || event.type.slice('invocation.'.length));
    } else if (event.type === 'tool.batch') {
      for (const call of Array.isArray(data.calls) ? (data.calls as Payload[]) : []) {
        setTool(event.seq, text(call.provider_call_id), text(call.name), text(call.status));
      }
    } else if (event.type === 'child.started' || event.type === 'child.finished') {
      const runId = text(data.child_run_id);
      const status = event.type === 'child.started' ? 'RUNNING' : text(data.status) || 'COMPLETED';
      const known = children.get(runId);
      if (known) {
        known.status = status;
      } else {
        const item = { kind: 'child' as const, seq: event.seq, runId, agent: text(data.agent), status };
        children.set(runId, item);
        work.items.push(item);
      }
    } else if (notes[event.type]) {
      work.items.push({ kind: 'note', seq: event.seq, label: notes[event.type] });
    }
  }
  return work;
}

// The text a `stream.delta` block carries.
export function deltaText(data: string): string {
  const value = payload(data);
  return text(value.text) || text(value.delta) || text(value.content);
}

// Merges fresh events into a sorted list, keeping one entry per sequence number.
export function mergeEvents(current: HarnessEvent[], fresh: HarnessEvent[]): HarnessEvent[] {
  const seen = new Set(current.map((event) => event.seq));
  const added = fresh.filter((event) => event.seq > 0 && !seen.has(event.seq) && seen.add(event.seq));
  return added.length ? [...current, ...added].sort((a, b) => a.seq - b.seq) : current;
}
