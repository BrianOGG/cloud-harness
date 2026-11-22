// Same-origin API client. Requests carry the optional bearer token used by serve.

export interface Budget {
  max_model_calls: number;
  used_model_calls: number;
  max_tool_calls: number;
  used_tool_calls: number;
  max_script_calls: number;
  used_script_calls: number;
  max_child_runs: number;
  used_child_runs: number;
}

export interface PendingInvocation {
  id?: string;
  invocation_id?: string;
  name?: string;
  tool?: string;
  args_summary?: string;
  arguments?: unknown;
  resources?: string[];
  status?: string;
  ordinal?: number;
  warnings?: string[];
}

export interface RunView {
  id: string;
  session_id: string;
  // Set on a child run started by delegate_task.
  parent_id?: string;
  status: 'RUNNING' | 'WAITING' | 'COMPLETED' | 'FAILED' | 'CANCELLED' | string;
  wait_reason?: 'USER' | 'APPROVAL' | 'CHILD' | 'BUDGET' | 'RECONCILIATION' | string;
  wait_kind?: string;
  question?: string;
  pending_invocation?: PendingInvocation;
  budget: Budget;
  final_answer?: string;
  error?: { code?: string; message?: string };
  version: number;
  // Not sent by serve today. Shown instead of the locally saved text when present.
  message?: string;
}

export interface SessionView {
  id: string;
  created_at: string;
  updated_at: string;
  run_count: number;
  latest_run_status?: string;
  latest_run_at?: string;
}

export interface ApprovalView {
  run_id: string;
  session_id: string;
  status: string;
  wait_reason: 'approval' | 'ask_user' | string;
  summary: string;
  waiting_since: string;
  waiting_seconds: number;
  approval_url?: string;
}

export interface HarnessEvent {
  seq: number;
  type: string;
  data: string;
}

export interface TraceCall {
  id: string;
  name: string;
  status: string;
  ordinal: number;
}
export interface TraceBatch {
  id: string;
  status: string;
  action: string;
  calls?: TraceCall[];
}
export interface TraceStep {
  id: string;
  action: string;
  batches?: TraceBatch[];
}
export interface TraceView {
  session_id: string;
  run: {
    id: string;
    status: string;
    wait_reason?: string;
    steps?: TraceStep[];
    children?: { id: string; status: string; wait_reason?: string }[];
  };
}

export interface StatsView {
  sessions: number;
  runs: number;
  events: number;
  active_runs: number;
  failed_runs: number;
  model_calls: number;
  usage: {
    input_tokens: number;
    output_tokens: number;
    cached_tokens: number;
    cache_hit_rate: number;
    cost: number | null;
  };
  budget: { model_used: number; model_max: number; tool_used: number; tool_max: number };
  policy: { allow: number; ask: number; deny: number };
  hooks: { total: number; warnings: number; blocked: number };
  recent_errors: { run_id: string; session_id: string; type: string; message: string; at: string }[];
}

export interface DoctorCheck {
  name: string;
  status: 'ok' | 'warn' | 'fail' | string;
  detail: string;
  run_id?: string;
}

export function parseEventStream(text: string): HarnessEvent[] {
  const events: HarnessEvent[] = [];
  for (const block of text.replace(/\r\n/g, '\n').split(/\n\n+/)) {
    let seq = 0;
    let type = '';
    const data: string[] = [];
    for (const line of block.split('\n')) {
      if (line.startsWith('id:')) {
        const value = line.slice(3).trim();
        if (/^\d+$/.test(value)) seq = Number(value);
      } else if (line.startsWith('event:')) {
        type = line.slice(6).trim();
      } else if (line.startsWith('data:')) {
        data.push(line.slice(5).replace(/^ /, ''));
      }
    }
    if (type) events.push({ seq, type, data: data.join('\n') });
  }
  return events;
}

const TOKEN_HINT = '需要有效的 HTTP token。请在浏览器 localStorage 设置 cloud-harness.http-token。';

function eventsRequest(runId: string, after: number, signal: AbortSignal): Promise<Response> {
  const headers = new Headers({ Accept: 'text/event-stream' });
  if (after > 0) headers.set('Last-Event-ID', String(after));
  return fetchWithToken(`/v1/runs/${encodeURIComponent(runId)}/events`, { headers, signal });
}

function eventsError(response: Response): Error {
  return new Error(response.status === 401 ? TOKEN_HINT : `${response.status} ${response.statusText || '请求失败'}`);
}

export interface RunStreamHandlers {
  /** Persisted events, in sequence order. Each carries a seq to resume from. */
  onEvents: (events: HarnessEvent[]) => void;
  /** The next piece of the model response in progress. Never persisted. */
  onDelta: (text: string) => void;
  /** Text delivered so far will not appear in the persisted events: discard it. */
  onReset: () => void;
}

/** Sorts a batch of parsed events into persisted events and live text, in arrival order. */
export function dispatchStreamEvents(events: HarnessEvent[], handlers: RunStreamHandlers) {
  let persisted: HarnessEvent[] = [];
  const flush = () => {
    if (persisted.length) handlers.onEvents(persisted);
    persisted = [];
  };
  for (const event of events) {
    if (event.type === 'stream.delta') {
      flush();
      try {
        const text = (JSON.parse(event.data) as { text?: unknown }).text;
        if (typeof text === 'string' && text) handlers.onDelta(text);
      } catch {
        // A malformed delta carries nothing to show.
      }
    } else if (event.type === 'stream.reset') {
      flush();
      handlers.onReset();
    } else if (event.seq > 0) {
      persisted.push(event);
    }
  }
  flush();
}

function fetchWithToken(path: string, init?: RequestInit): Promise<Response> {
  const headers = new Headers(init?.headers);
  const token = localStorage.getItem('cloud-harness.http-token');
  if (token) headers.set('Authorization', `Bearer ${token}`);
  return fetch(path, { ...init, headers });
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (init?.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
  const response = await fetchWithToken(path, { ...init, headers });
  if (!response.ok) {
    let message = `${response.status} ${response.statusText || '请求失败'}`;
    try {
      const body = await response.json();
      if (typeof body?.error === 'string') message = body.error;
      else if (body?.error?.message) message = body.error.message;
    } catch {
      // Keep the HTTP status when the error response is not JSON.
    }
    if (response.status === 401) message = TOKEN_HINT;
    throw new Error(message);
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const api = {
  health: () => req<{ status: string; provider: string }>('/healthz'),
  listSessions: () => req<SessionView[]>('/v1/sessions'),
  createSession: () => req<{ id: string }>('/v1/sessions', { method: 'POST' }),
  listRuns: (sessionId: string) => req<RunView[]>(`/v1/sessions/${encodeURIComponent(sessionId)}/runs`),
  createRun: (sessionId: string, message: string) =>
    req<RunView>(`/v1/sessions/${encodeURIComponent(sessionId)}/runs?wait=0`, {
      method: 'POST',
      body: JSON.stringify({ message }),
    }),
  listApprovals: () => req<ApprovalView[]>('/v1/approvals'),
  getStats: (signal?: AbortSignal) => req<StatsView>('/v1/stats', { signal }),
  getDoctor: (signal?: AbortSignal) => req<DoctorCheck[]>('/v1/doctor', { signal }),
  getRun: (runId: string) => req<RunView>(`/v1/runs/${encodeURIComponent(runId)}`),
  respond: (runId: string, body: { decision?: string; answer?: string; extra_model_calls?: number }) =>
    req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/responses?wait=0`, { method: 'POST', body: JSON.stringify(body) }),
  addBudget: (runId: string, extra: number) =>
    req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/budget?wait=0`, {
      method: 'POST',
      body: JSON.stringify({ extra_model_calls: extra }),
    }),
  cancel: (runId: string) => req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/cancel`, { method: 'POST' }),
  getTrace: (runId: string) => req<TraceView>(`/v1/runs/${encodeURIComponent(runId)}/trace`),
  /**
   * Holds the run's event stream open and reports events as they arrive.
   * Resolves when the server ends the stream, which it does once the run is
   * terminal, or when signal aborts. Live text has no cursor: a new connection
   * starts with the text so far of the response in progress, so the caller
   * clears what it showed before reconnecting.
   */
  streamRun: async (runId: string, after: number, handlers: RunStreamHandlers, signal: AbortSignal): Promise<void> => {
    let response: Response;
    try {
      response = await eventsRequest(runId, after, signal);
    } catch (reason) {
      if (signal.aborted) return;
      throw reason;
    }
    if (!response.ok) throw eventsError(response);
    if (!response.body) {
      dispatchStreamEvents(parseEventStream(await response.text()), handlers);
      return;
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    try {
      while (true) {
        const { done, value } = await reader.read();
        buffer += (done ? decoder.decode() : decoder.decode(value, { stream: true }));
        buffer = buffer.replace(/\r\n/g, '\n');
        // Only blocks closed by a blank line are complete; keep the rest.
        const end = done ? buffer.length : buffer.lastIndexOf('\n\n');
        if (end >= 0) {
          dispatchStreamEvents(parseEventStream(buffer.slice(0, end)), handlers);
          buffer = buffer.slice(done ? end : end + 2);
        }
        if (done) return;
      }
    } catch (reason) {
      if (signal.aborted) return;
      throw reason;
    } finally {
      await reader.cancel().catch(() => undefined);
    }
  },
  // One short read of the persistent events after `after`. serve keeps the
  // stream of a live run open, so the read stops at the first complete block
  // or after 750 ms.
  fetchEvents: async (runId: string, after: number): Promise<HarnessEvent[]> => {
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 750);
    const response = await eventsRequest(runId, after, controller.signal);
    if (!response.ok) {
      window.clearTimeout(timeout);
      throw eventsError(response);
    }
    if (!response.body) {
      window.clearTimeout(timeout);
      return parseEventStream(await response.text());
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n');
        const blocks = buffer.split(/\n\n+/);
        if (blocks.length > 1 && blocks.slice(0, -1).some((block) => block.includes('event:'))) break;
      }
      buffer += decoder.decode();
      return parseEventStream(buffer);
    } catch (reason) {
      if (reason instanceof DOMException && reason.name === 'AbortError') return parseEventStream(buffer);
      throw reason;
    } finally {
      window.clearTimeout(timeout);
      controller.abort();
      await reader.cancel().catch(() => undefined);
    }
  },
  // Follows the event stream until serve closes it or `signal` aborts, calling
  // `onEvent` for each block as it arrives. Stream deltas come through with seq 0.
  streamEvents: async (runId: string, after: number, onEvent: (event: HarnessEvent) => void, signal: AbortSignal): Promise<void> => {
    const response = await eventsRequest(runId, after, signal);
    if (!response.ok) throw eventsError(response);
    if (!response.body) {
      parseEventStream(await response.text()).forEach(onEvent);
      return;
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n');
        const cut = buffer.lastIndexOf('\n\n');
        if (cut < 0) continue;
        parseEventStream(buffer.slice(0, cut)).forEach(onEvent);
        buffer = buffer.slice(cut + 2);
      }
      parseEventStream(buffer + decoder.decode()).forEach(onEvent);
    } finally {
      await reader.cancel().catch(() => undefined);
    }
  },
};
