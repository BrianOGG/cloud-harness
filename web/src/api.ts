// Same-origin API client. Requests carry the optional bearer token used by serve.

import { EventStreamDecoder, parseEventStream } from './sse';
import type { HarnessEvent } from './sse';
export { parseEventStream } from './sse';
export type { HarnessEvent } from './sse';

export interface Budget {
  max_model_calls: number;
  used_model_calls: number;
  max_tool_calls: number;
  used_tool_calls: number;
  max_script_calls: number;
  used_script_calls: number;
  max_child_runs: number;
  used_child_runs: number;
  // Token and estimated-cost usage. A limit of 0 is unlimited.
  max_tokens?: number;
  used_input_tokens?: number;
  used_output_tokens?: number;
  used_cached_input_tokens?: number;
  used_cache_creation_input_tokens?: number;
  used_reasoning_tokens?: number;
  cache_reported?: boolean;
  reasoning_reported?: boolean;
  max_cost_usd?: number;
  used_cost_usd?: number;
}

export interface BudgetExtension {
  extra_model_calls?: number;
  extra_tokens?: number;
  extra_cost_usd?: number;
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
  turn_id?: string;
  // Set on a child run started by delegate_task.
  parent_id?: string;
  // Frozen runtime choices from this run's checkpoint, never session preferences.
  approval_mode?: string;
  reasoning_effort?: ReasoningEffort;
  workspace?: string;
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

export type ReasoningEffort = 'default' | 'off' | 'low' | 'medium' | 'high';
export type ProviderProtocol = 'openai' | 'anthropic' | 'gemini';
export interface CatalogModel { id: string; display_name?: string; thinking?: 'supported' | 'unsupported' | 'unknown'; reasoning_type?: 'toggle' | 'effort'; reasoning_values?: string[]; category?: 'chat' | 'other'; }
export interface ModelDetection { protocol: ProviderProtocol; models: CatalogModel[]; fetched_at: string; alternatives?: ProviderProtocol[]; }
export interface SessionPreferences { approval_mode: string; model_id: string; reasoning_effort?: ReasoningEffort; }
export interface ModelOption { id: string; name: string; model: string; fallback_model?: string; provider: string; provider_id?: string; provider_name?: string; legacy_id?: string; thinking?: CatalogModel['thinking']; reasoning_type?: CatalogModel['reasoning_type']; reasoning_values?: string[]; category?: CatalogModel['category']; display_name?: string; }
export interface CustomProvider { id: string; name: string; base_url: string; model: string; has_api_key: boolean; protocol?: ProviderProtocol; protocol_source?: 'auto' | 'manual'; models?: string[]; catalog?: CatalogModel[]; catalog_fetched_at?: string; }

export interface SessionMessage {
  id: string;
  session_id: string;
  run_id: string | null;
  turn_id: string | null;
  role: 'user' | 'assistant' | 'system';
  kind: 'input' | 'response' | 'question' | 'answer' | 'approval_requested' | 'approval_decision' | 'run_status';
  status: 'recorded' | 'failed' | 'cancelled';
  content: string;
  event: string;
  created_at: string;
}

export interface SessionMessagesPage {
  messages: SessionMessage[];
  next_cursor: string;
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

export interface TraceCall {
  id: string;
  name: string;
  status: string;
  ordinal: number;
  arguments?: unknown;
  result?: { ok: boolean; data?: unknown; error?: { code?: string; message?: string } };
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

const TOKEN_HINT = '需要有效的 HTTP token。请在浏览器 localStorage 设置 cloud-harness.http-token。';

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

export function isNotFound(reason: unknown): boolean {
  return reason instanceof ApiError && reason.status === 404;
}

function eventsRequest(runId: string, after: number, signal: AbortSignal): Promise<Response> {
  const headers = new Headers({ Accept: 'text/event-stream' });
  if (after > 0) headers.set('Last-Event-ID', String(after));
  return fetchWithToken(`/v1/runs/${encodeURIComponent(runId)}/events`, { headers, signal });
}

function eventsError(response: Response): Error {
  return new Error(response.status === 401 ? TOKEN_HINT : `${response.status} ${response.statusText || '请求失败'}`);
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
    throw new ApiError(response.status, message);
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const api = {
  health: () => req<{ status: string; provider: string }>('/healthz'),
  listSessions: () => req<SessionView[]>('/v1/sessions'),
  createSession: () => req<{ id: string }>('/v1/sessions', { method: 'POST' }),
  getPreferences: (sessionId: string) => req<SessionPreferences>('/v1/sessions/' + encodeURIComponent(sessionId) + '/preferences'),
  savePreferences: (sessionId: string, value: SessionPreferences) => req<SessionPreferences>('/v1/sessions/' + encodeURIComponent(sessionId) + '/preferences', { method: 'PUT', body: JSON.stringify(value) }),
  listModels: () => req<ModelOption[]>('/v1/models'),
  listProviders: () => req<CustomProvider[]>('/v1/providers'),
  saveProvider: (value: { id: string; name: string; base_url: string; model: string; api_key?: string; protocol?: ProviderProtocol; protocol_source?: 'auto' | 'manual'; models?: string[]; catalog?: CatalogModel[]; catalog_fetched_at?: string }) => req<CustomProvider>('/v1/providers', { method: 'PUT', body: JSON.stringify(value) }),
  deleteProvider: (id: string) => req<void>('/v1/providers/' + encodeURIComponent(id), { method: 'DELETE' }),
  detectProvider: (value: { base_url: string; api_key?: string; provider_id?: string; protocol?: ProviderProtocol }) => req<ModelDetection>('/v1/providers/detect', { method: 'POST', body: JSON.stringify(value) }),
  refreshProviderModels: (id: string) => req<CustomProvider>('/v1/providers/' + encodeURIComponent(id) + '/refresh-models', { method: 'POST' }),
  testProvider: (id: string) => req<{ ok: boolean; model_available: boolean }>('/v1/providers/' + encodeURIComponent(id) + '/test', { method: 'POST' }),
  listMessages: (sessionId: string, options?: { limit?: number; cursor?: string; signal?: AbortSignal }): Promise<SessionMessagesPage> => {
    const params = new URLSearchParams();
    if (options?.limit !== undefined) params.set('limit', String(options.limit));
    if (options?.cursor !== undefined) params.set('cursor', options.cursor);
    const query = params.toString();
    return req<SessionMessagesPage>(`/v1/sessions/${encodeURIComponent(sessionId)}/messages${query ? `?${query}` : ''}`, { signal: options?.signal });
  },
  listRuns: (sessionId: string) => req<RunView[]>(`/v1/sessions/${encodeURIComponent(sessionId)}/runs`),
  createRun: (sessionId: string, message: string, options?: { model_id?: string; approval_mode?: string; reasoning_effort?: ReasoningEffort }) =>
    req<RunView>(`/v1/sessions/${encodeURIComponent(sessionId)}/runs?wait=0`, {
      method: 'POST',
      body: JSON.stringify({ message, ...options }),
    }),
  listApprovals: () => req<ApprovalView[]>('/v1/approvals'),
  getStats: (signal?: AbortSignal) => req<StatsView>('/v1/stats', { signal }),
  getDoctor: (signal?: AbortSignal) => req<DoctorCheck[]>('/v1/doctor', { signal }),
  getRun: (runId: string) => req<RunView>(`/v1/runs/${encodeURIComponent(runId)}`),
  respond: (runId: string, body: { decision?: string; answer?: string; extra_model_calls?: number }) =>
    req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/responses?wait=0`, { method: 'POST', body: JSON.stringify(body) }),
  addBudget: (runId: string, extension: BudgetExtension) =>
    req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/budget?wait=0`, {
      method: 'POST',
      body: JSON.stringify(extension),
    }),
  cancel: (runId: string) => req<RunView>(`/v1/runs/${encodeURIComponent(runId)}/cancel`, { method: 'POST' }),
  getTrace: (runId: string) => req<TraceView>(`/v1/runs/${encodeURIComponent(runId)}/trace`),
  // One short read of the persistent events after `after`. serve keeps the
  // stream of a live run open, so the read stops at the first complete block
  // or after 750 ms.
  fetchEvents: async (runId: string, after: number): Promise<HarnessEvent[]> => {
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 750);
    const events: HarnessEvent[] = [];
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
    try {
      const response = await eventsRequest(runId, after, controller.signal);
      if (!response.ok) throw eventsError(response);
      if (!response.body) return parseEventStream(await response.text());
      reader = response.body.getReader();
      const decoder = new EventStreamDecoder();
      while (true) {
        const { done, value } = await reader.read();
        if (done) {
          events.push(...decoder.finish());
          break;
        }
        events.push(...decoder.push(value));
        if (events.length > 0) break;
      }
    } catch (reason) {
      if (!controller.signal.aborted) throw reason;
    } finally {
      window.clearTimeout(timeout);
      controller.abort();
      await reader?.cancel().catch(() => undefined);
    }
    return events;
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
    const decoder = new EventStreamDecoder();
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) {
          decoder.finish().forEach(onEvent);
          break;
        }
        decoder.push(value).forEach(onEvent);
      }
    } catch (reason) {
      if (!signal.aborted) throw reason;
    } finally {
      await reader.cancel().catch(() => undefined);
    }
  },
};
