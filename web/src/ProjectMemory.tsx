import { useEffect, useState } from 'react';
import type { MemoryDraft, ProjectMemory } from './projectMemoryApi';
import { memoryApi } from './projectMemoryApi';
import { errorText, shortId } from './format';

const kindLabel = { fact: '事实', decision: '决定' };

function MemoryForm({ initial, onSave, onCancel }: {
  initial: MemoryDraft;
  onSave: (draft: MemoryDraft) => Promise<void>;
  onCancel: () => void;
}) {
  const [kind, setKind] = useState(initial.kind);
  const [content, setContent] = useState(initial.content);
  const [saving, setSaving] = useState(false);
  const length = Array.from(content.trim()).length;
  const [error, setError] = useState('');
  return <form className="memory-form" aria-label="确认项目记忆" onSubmit={async event => {
    event.preventDefault();
    setSaving(true);
    setError('');
    try { await onSave({ kind, content: content.trim() }); }
    catch (reason) { setError(errorText(reason)); }
    finally { setSaving(false); }
  }}>
    <label>类型 <select value={kind} disabled={saving} onChange={event => setKind(event.target.value as MemoryDraft['kind'])}>
      <option value="fact">事实</option><option value="decision">决定</option>
    </select></label>
    <textarea aria-label="记忆内容" placeholder="写下供本项目后续对话参考的内容，请勿填写密钥。" value={content} rows={3}
      disabled={saving} onChange={event => setContent(event.target.value)} autoFocus />
    {error && <p role="alert" className="settings-error">{error}</p>}
    <div className="memory-actions">
      <span>{length} / 500</span>
      <button type="button" className="button small" disabled={saving} onClick={onCancel}>取消</button>
      <button type="submit" className="button small primary" disabled={saving || length === 0 || length > 500}>{saving ? '保存中…' : '确认保存'}</button>
    </div>
  </form>;
}

function selectedReplyText(button: HTMLButtonElement) {
  const selection = window.getSelection();
  const reply = button.closest('article')?.querySelector('.turn-final');
  if (!selection?.rangeCount || selection.isCollapsed || !reply?.contains(selection.getRangeAt(0).commonAncestorContainer)) return '';
  return selection.toString().trim();
}

export function ConfirmMemory({ sessionId, runId }: { sessionId: string; runId: string }) {
  const [draft, setDraft] = useState<MemoryDraft | null>(null);
  const [saved, setSaved] = useState(false);
  if (draft) return <MemoryForm initial={draft} onCancel={() => setDraft(null)} onSave={async value => {
    await memoryApi.confirm(value, sessionId, runId);
    setDraft(null);
    setSaved(true);
  }} />;
  return <div className="memory-promotion">
    <button type="button" className="link-button" onMouseDown={event => {
      // Keep the selected passage when this button would take pointer focus.
      if (selectedReplyText(event.currentTarget)) event.preventDefault();
    }} onClick={event => {
      setDraft({ kind: 'fact', content: selectedReplyText(event.currentTarget) });
      setSaved(false);
    }}>存为项目记忆</button>
    {saved && <span role="status">已保存，可在设置中查看</span>}
  </div>;
}

function MemoryList() {
  const [items, setItems] = useState<ProjectMemory[]>([]);
  const [revision, setRevision] = useState(0);
  const [loading, setLoading] = useState(true);
  const [editing, setEditing] = useState('');
  const [deleting, setDeleting] = useState('');
  const [removing, setRemoving] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    const controller = new AbortController();
    memoryApi.list(controller.signal).then(rows => {
      if (!controller.signal.aborted) setItems(rows);
    }).catch(reason => {
      if (!controller.signal.aborted) setError(errorText(reason));
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [revision]);
  const reload = () => {
    setLoading(true);
    setError('');
    setRevision(n => n + 1);
  };
  const remove = async (id: string) => {
    setRemoving(true);
    setError('');
    try {
      await memoryApi.remove(id);
      setItems(previous => previous.filter(row => row.id !== id));
      setDeleting('');
    } catch (reason) { setError(errorText(reason)); }
    finally { setRemoving(false); }
  };

  return <>
    {loading && <p role="status">正在加载记忆…</p>}
    {error && <p className="settings-error" role="alert">{error} <button type="button" className="link-button" onClick={reload}>重试</button></p>}
    {!loading && !error && items.length === 0 && <p>还没有项目记忆。在历史答复下选择「存为项目记忆」。</p>}
    <ul className="memory-list">
      {items.map(row => <li key={row.id}>
        <span className="chip">{kindLabel[row.kind]}</span>
        {editing === row.id ? <MemoryForm initial={row} onCancel={() => setEditing('')} onSave={async draft => {
          await memoryApi.correct(row.id, draft);
          setItems(previous => [{ ...row, ...draft }, ...previous.filter(item => item.id !== row.id)]);
          setEditing(current => current === row.id ? '' : current);
        }} /> : <p className="memory-text">{row.content}</p>}
        <div className="memory-actions">
          {row.source_available
            ? <a className="link-button" title={row.source_session_id + ' / ' + row.source_run_id}
                href={'?' + new URLSearchParams({ session: row.source_session_id, run: row.source_run_id })}>来源对话 · {shortId(row.source_run_id)}</a>
            : <span title={row.source_session_id + ' / ' + row.source_run_id}>来源运行已清理 · {shortId(row.source_run_id)}</span>}
          {editing !== row.id && deleting !== row.id && <>
            <button type="button" className="link-button" disabled={removing} onClick={() => { setDeleting(''); setEditing(row.id); }}>修改</button>
            <button type="button" className="link-button" disabled={removing} onClick={() => { setEditing(''); setDeleting(row.id); }}>删除</button>
          </>}
          {deleting === row.id && <>
            <button type="button" className="button small" disabled={removing} onClick={() => setDeleting('')}>取消</button>
            <button type="button" className="button small" disabled={removing} onClick={() => void remove(row.id)}>确认删除</button>
          </>}
        </div>
      </li>)}
    </ul>
  </>;
}

export function ProjectMemories() {
  const [open, setOpen] = useState(false);
  return <section className="project-memories" aria-label="项目记忆">
    <h2><button type="button" aria-expanded={open} onClick={() => setOpen(!open)}>项目记忆 <span aria-hidden="true">{open ? '▾' : '▸'}</span></button></h2>
    {open && <MemoryList />}
  </section>;
}
