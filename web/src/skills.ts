export const skillCatalogChanged = 'cloud-harness:skills-changed';

export interface SkillSummary {
  name: string;
  description: string;
}

// Only the explicit CLI-style command invokes a skill. Other slash-prefixed
// messages (including paths) can be sent as ordinary text.
export function skillInvocation(draft: string) {
  const match = /^\/skill[ \t]+([A-Za-z0-9_-]+)(?:\s([\s\S]*))?$/.exec(draft);
  return match ? { name: match[1], task: match[2] || '' } : null;
}

export function skillQuery(draft: string) {
  const match = /^\/(?:skill[ \t]+)?([A-Za-z0-9_-]*)$/.exec(draft);
  return match ? match[1].toLowerCase() : null;
}
