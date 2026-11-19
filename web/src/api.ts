// Client for the same-origin API (proxied by Vite during development).

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

export interface RunView {
  id: string;
  session_id: string;
  status: 'RUNNING' | 'WAITING' | 'COMPLETED' | 'FAILED' | 'CANCELLED' | string;
  wait_reason?: 'USER' | 'APPROVAL' | 'CHILD' | 'BUDGET' | 'RECONCILIATION' | string;
  budget: Budget;
  final_answer?: string;
  error?: { code?: string; message?: string };
  version: number;
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

function fetchWithToken(path: string, init?: RequestInit): Promise<Response> {
  const headers = new Headers(init?.headers);
  const token = localStorage.getItem('cloud-harness.http-token');
  if (token) headers.set('Authorization', `Bearer ${token}`);
  return fetch(path, { ...init, headers });
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const r = await fetchWithToken(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers || {}) },
  });
  if (!r.ok) {
    let msg = `${r.status} ${r.statusText}`;
    try {
      const j = await r.json();
      if (j?.error?.message) msg = j.error.message;
    } catch {
      /* keep default */
    }
    throw new Error(msg);
  }
  return (await r.json()) as T;
}

export const api = {
  health: () => req<{ status: string; provider: string }>('/healthz'),
  createSession: () => req<{ id: string }>('/v1/sessions', { method: 'POST' }),
  createRun: (sessionId: string, message: string) =>
    req<RunView>(`/v1/sessions/${sessionId}/runs`, {
      method: 'POST',
      body: JSON.stringify({ message }),
    }),
  getRun: (runId: string) => req<RunView>(`/v1/runs/${runId}`),
  respond: (runId: string, body: { decision?: string; answer?: string; extra_model_calls?: number }) =>
    req<RunView>(`/v1/runs/${runId}/responses`, { method: 'POST', body: JSON.stringify(body) }),
  addBudget: (runId: string, extra: number) =>
    req<RunView>(`/v1/runs/${runId}/budget`, {
      method: 'POST',
      body: JSON.stringify({ extra_model_calls: extra }),
    }),
  cancel: (runId: string) => req<RunView>(`/v1/runs/${runId}/cancel`, { method: 'POST' }),
  getTrace: (runId: string) => req<TraceView>(`/v1/runs/${runId}/trace`),
  // The events endpoint answers with an SSE dump of stored events after
  // `after` and then closes; parse the text body.
  fetchEvents: async (runId: string, after: number): Promise<HarnessEvent[]> => {
    const r = await fetchWithToken(`/v1/runs/${runId}/events`, {
      headers: after > 0 ? { 'Last-Event-ID': String(after) } : {},
    });
    if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
    const text = await r.text();
    const out: HarnessEvent[] = [];
    for (const block of text.split('\n\n')) {
      let seq = 0;
      let type = '';
      let data = '';
      for (const line of block.split('\n')) {
        if (line.startsWith('id: ')) seq = Number(line.slice(4));
        else if (line.startsWith('event: ')) type = line.slice(7);
        else if (line.startsWith('data: ')) data = line.slice(6);
      }
      if (type) out.push({ seq, type, data });
    }
    return out;
  },
};
