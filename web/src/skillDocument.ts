// The CLI supports a small single-line frontmatter format. Keep the original
// header intact when editing the body, including unknown keys and whitespace.
export function splitDocument(content: string) {
  const match = /\n---\r?(?:\n|$)/.exec(content);
  if (!match) throw new Error('找不到 SKILL.md 元数据结束行');
  const end = match.index + match[0].length;
  return { header: content.slice(0, end), body: content.slice(end) };
}

function scalar(value: string) {
  const text = value.trim();
  // The CLI strips one surrounding pair of quotes, with no escape processing.
  if (/^['"]/.test(text)) return `"${text}"`;
  return text;
}

export function newDocument(name: string, description: string, body: string) {
  return `---\nname: ${name}\ndescription: ${scalar(description)}\n---\n${body}`;
}

export function editDocument(content: string, oldDescription: string, description: string, body: string) {
  const original = splitDocument(content);
  let header = original.header;
  if (description !== oldDescription) {
    header = header.replace(/^([^\S\r\n]*description[^\S\r\n]*:[^\S\r\n]*)[^\r\n]*/m, (_, prefix: string) => prefix + scalar(description));
  }
  if (body === original.body) return header + body;
  if (!header.endsWith('\n')) header += header.includes('\r\n') ? '\r\n' : '\n';
  if (header.includes('\r\n') && !/(?<!\r)\n/.test(original.body)) body = body.replace(/\r?\n/g, '\r\n');
  return header + body;
}
