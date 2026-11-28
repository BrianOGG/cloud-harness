import { api } from './api';
import type { SessionMessage } from './api';

// Traverse the retained transcript from its first page. A cursor is opaque,
// and pages are not a snapshot: callers refresh to see late/backdated facts.
export async function loadSessionMessages(
  sessionId: string, options?: { signal?: AbortSignal },
): Promise<SessionMessage[]> {
  const signal = options?.signal;
  const messages: SessionMessage[] = [];
  const seenCursors = new Set<string>();
  let cursor: string | undefined;

  while (true) {
    signal?.throwIfAborted();
    const page = await api.listMessages(sessionId, { cursor, signal });
    signal?.throwIfAborted();
    if (!page || !Array.isArray(page.messages) || typeof page.next_cursor !== 'string') {
      throw new Error('会话历史分页格式无效');
    }
    if (page.messages.some((message) => !message || message.session_id !== sessionId)) {
      throw new Error('会话历史消息与请求的会话不匹配');
    }
    if (page.messages.some((message) => (
      typeof message.id !== 'string' || typeof message.created_at !== 'string' || typeof message.content !== 'string'
    ))) {
      throw new Error('会话历史消息格式无效');
    }
    messages.push(...page.messages);
    if (page.next_cursor === '') return mergeSessionMessages([], messages);
    if (seenCursors.has(page.next_cursor)) throw new Error('会话历史分页游标重复');
    seenCursors.add(page.next_cursor);
    cursor = page.next_cursor;
  }
}

const compareASCII = (a: string, b: string): number => a < b ? -1 : a > b ? 1 : 0;

export interface TranscriptSnapshot {
  cursor: string;
  messages: SessionMessage[];
}

// Publish the new cursor with its rows, only after every page succeeds.
// A retention reset during pagination discards even the in-flight pages.
export async function syncSessionMessages(sessionId: string, previous: TranscriptSnapshot, signal: AbortSignal): Promise<TranscriptSnapshot> {
  let { cursor, messages } = previous;
  const updates: SessionMessage[] = [];
  const seen = new Set<string>();
  while (true) {
    const page = await api.syncMessages(sessionId, cursor, signal);
    signal.throwIfAborted();
    if ('next_cursor' in page) {
      // A cached frontend can briefly talk to an older service during rollout.
      return { cursor: '0.0', messages: await loadSessionMessages(sessionId, { signal }) };
    }
    if (!/^\d+\.\d+$/.test(page.sync) || typeof page.more !== 'boolean' || typeof page.reset !== 'boolean' ||
        !Array.isArray(page.messages) || page.messages.some(m => m.session_id !== sessionId || typeof m.id !== 'string' || typeof m.created_at !== 'string' || typeof m.content !== 'string')) {
      throw new Error('会话历史增量格式无效');
    }
    if (page.reset) {
      messages = [];
      updates.length = 0;
    }
    updates.push(...page.messages);
    if (!page.more) return { cursor: page.sync, messages: updates.length ? mergeSessionMessages(messages, updates) : messages };
    if (cursor === page.sync || seen.has(page.sync)) throw new Error('会话历史分页游标重复');
    cursor = page.sync;
    seen.add(cursor);
  }
}

// Incoming records replace the same persistent ID, never equal text. The
// contract's fixed UTC, nine-digit timestamps sort without losing nanoseconds.
export function mergeSessionMessages(
  existing: readonly SessionMessage[], incoming: readonly SessionMessage[],
): SessionMessage[] {
  const byId = new Map<string, SessionMessage>();
  for (const message of existing) byId.set(message.id, message);
  for (const message of incoming) byId.set(message.id, message);
  return [...byId.values()].sort((a, b) => compareASCII(a.created_at, b.created_at) || compareASCII(a.id, b.id));
}
