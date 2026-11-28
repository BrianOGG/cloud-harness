import { req } from './api';

export interface MemoryDraft {
  kind: 'fact' | 'decision';
  content: string;
}

export interface ProjectMemory extends MemoryDraft {
  id: string;
  source_session_id: string;
  source_run_id: string;
  source_available: boolean;
  updated_at: string;
}

const path = '/v1/project-memories';
const item = (id: string) => path + '/' + encodeURIComponent(id);

export const memoryApi = {
  list: (signal: AbortSignal) => req<ProjectMemory[]>(path, { signal }),
  confirm: (draft: MemoryDraft, session: string, run: string) => req<ProjectMemory>(path, {
    method: 'POST', body: JSON.stringify({ ...draft, source_session_id: session, source_run_id: run }),
  }),
  correct: (id: string, draft: MemoryDraft) => req<void>(item(id), { method: 'PUT', body: JSON.stringify(draft) }),
  remove: (id: string) => req<void>(item(id), { method: 'DELETE' }),
};
