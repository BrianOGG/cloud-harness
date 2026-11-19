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
  name?: string;
  tool?: string;
  arguments?: unknown;
  status?: string;
  ordinal?: number;
}

export interface RunView {
  id: string;
  session_id: string;
  status: 'RUNNING' | 'WAITING' | 'COMPLETED' | 'FAILED' | 'CANCELLED' | string;
  wait_reason?: 'USER' | 'APPROVAL' | 'CHILD' | 'BUDGET' | 'RECONCILIATION' | string;
  wait_kind?: string;
  question?: string;
  pending_invocation?: PendingInvocation;
  budget: Budget;
  final_answer?: string;
  error?: { code?: string; message?: string };
  version: number;
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
    if (response.status === 401) message = '需要有效的 HTTP token。请在浏览器 localStorage 设置 cloud-harness.http-token。';
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
    req<RunView>(`/v1/sessions/${encodeURIComponent(sessionId)}/runs`, {
      method: 'POST',
      body: JSON.stringify({ message }),
    }),
  listApprovals: () => req<ApprovalView[]>('/v1/approvals'),
  getStats: (signal?: AbortSignal) => req<StatsView>('/v1/stats', { signal }),
  getDoctor: (signal?: AbortSignal) => req<DoctorCheck[]>('/v1/doctor', { signal }),
  getRun: (runId: string) => req<RunView>(`/v1/runs/${encodeURIComponent(runId)}`),
  respond: (runId: string, body: { decision?: string; answer?: string; extra_model_calls?: number }) =>
    req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/responses`, { method: 'POST', body: JSON.stringify(body) }),
  addBudget: (runId: string, extra: number) =>
    req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/budget`, {
      method: 'POST',
      body: JSON.stringify({ extra_model_calls: extra }),
    }),
  cancel: (runId: string) => req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/cancel`, { method: 'POST' }),
  getTrace: (runId: string) => req<TraceView>(`/v1/runs/${encodeURIComponent(runId)}/trace`),
  fetchEvents: async (runId: string, after: number): Promise<HarnessEvent[]> => {
    const headers = new Headers();
    if (after > 0) headers.set('Last-Event-ID', String(after));
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 750);
    const response = await fetchWithToken(`/v1/runs/${encodeURIComponent(runId)}/events`, { headers, signal: controller.signal });
    if (!response.ok) {
      window.clearTimeout(timeout);
      const message = response.status === 401
        ? '需要有效的 HTTP token。请在浏览器 localStorage 设置 cloud-harness.http-token。'
        : `${response.status} ${response.statusText || '请求失败'}`;
      throw new Error(message);
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
};
