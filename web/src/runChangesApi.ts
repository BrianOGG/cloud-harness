import { req } from './api';

export interface BatchChanges {
  number: number;
  before: string;
  after: string;
  files: { path: string; status: string }[];
}
export interface RunChangeList {
  batches: BatchChanges[];
  incomplete: boolean;
  terminal: boolean;
}
export interface ReviewedPath {
  before: string;
  after: string;
  path: string;
}
export interface FilePatch { diff: string; truncated: boolean }

const base = (id: string) => `/v1/runs/${encodeURIComponent(id)}/changes`;

export const runChanges = {
  list: (id: string, signal: AbortSignal) => req<RunChangeList>(base(id), { signal }),
  diff: (id: string, selection: ReviewedPath, signal: AbortSignal) =>
    req<FilePatch>(`${base(id)}/diff?${new URLSearchParams({ ...selection })}`, { signal }),
  restore: (id: string, selection: ReviewedPath) =>
    req<{ safety_snapshot: string }>(`${base(id)}/restore`, { method: 'POST', body: JSON.stringify({ ...selection, confirm: true }) }),
};
