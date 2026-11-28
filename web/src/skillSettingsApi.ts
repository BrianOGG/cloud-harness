import { req } from './api';
import type { SkillSummary } from './skills';

export type SkillScope = 'project' | 'global';
export interface ScopedSkill extends SkillSummary {
  scope: SkillScope;
  effective: boolean;
}
export interface SkillDocument {
  name: string;
  description: string;
  scope: SkillScope;
  content: string;
  truncated: boolean;
  revision?: string;
}

const base = '/v1/skill-definitions';
const path = (scope: SkillScope, name: string) => `${base}/${scope}/${encodeURIComponent(name)}`;

export const skillSettingsApi = {
  list: (signal: AbortSignal) => req<ScopedSkill[]>(base, { signal }),
  open: ({ scope, name }: ScopedSkill) => req<SkillDocument>(path(scope, name)),
  create: (scope: SkillScope, content: string) => req<SkillDocument>(`${base}/${scope}`, {
    method: 'POST', body: JSON.stringify({ content }),
  }),
  update: ({ scope, name, revision }: SkillDocument, content: string) => req<SkillDocument>(path(scope, name), {
    method: 'PUT', body: JSON.stringify({ content, revision }),
  }),
  remove: ({ scope, name, revision }: SkillDocument) => req<void>(path(scope, name), {
    method: 'DELETE', body: JSON.stringify({ revision }),
  }),
};
