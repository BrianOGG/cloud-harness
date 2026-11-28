import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { errorText } from './format';
import { editDocument, newDocument, splitDocument } from './skillDocument';
import { skillSettingsApi, type ScopedSkill, type SkillDocument, type SkillScope } from './skillSettingsApi';
import { skillCatalogChanged } from './skills';

const scopeLabel = { project: '项目', global: '全局' };
type Editor = { kind: 'new' } | { kind: 'import'; content: string } | { kind: 'edit'; doc: SkillDocument };

function SkillEditor({ editor, onDone, onCancel }: {
  editor: Editor;
  onDone: (message: string) => void;
  onCancel: () => void;
}) {
  const doc = editor.kind === 'edit' ? editor.doc : undefined;
  const [scope, setScope] = useState<SkillScope>(doc?.scope || 'project');
  const [name, setName] = useState(doc?.name || '');
  const [description, setDescription] = useState(doc?.description || '');
  const [body, setBody] = useState(() => doc && !doc.truncated ? splitDocument(doc.content).body : '');
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  async function save(event: FormEvent) {
    event.preventDefault();
    if (busy || doc?.truncated) return;
    setBusy(true);
    setError('');
    try {
      let saved: SkillDocument;
      if (editor.kind === 'edit') {
        const content = editDocument(editor.doc.content, editor.doc.description, description, body);
        saved = await skillSettingsApi.update(editor.doc, content);
      } else {
        const content = editor.kind === 'import' ? editor.content : newDocument(name, description, body);
        saved = await skillSettingsApi.create(scope, content);
      }
      onDone(`已保存${scopeLabel[saved.scope]}技能 ${saved.name}`);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    if (!doc || !confirm || busy) return;
    setBusy(true);
    setError('');
    try {
      await skillSettingsApi.remove(doc);
      onDone(`已删除${scopeLabel[doc.scope]}技能 ${doc.name}`);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  }

  return <form className="skill-settings-form" onSubmit={save}>
    <h3>{doc ? doc.name : editor.kind === 'import' ? '导入 SKILL.md' : '新建技能'}</h3>
    <fieldset disabled={busy}>
      <label>作用域<select value={scope} disabled={!!doc} onChange={e => setScope(e.target.value as SkillScope)}>
        <option value="project">项目 · 当前工作区</option><option value="global">全局 · 所有项目</option>
      </select></label>
      {doc?.truncated ? <>
        <p>文件超过 64 KiB，以下为只读预览。请在磁盘上编辑或删除。</p>
        <pre>{doc.content}</pre>
      </> : editor.kind === 'import' ? <label>文件内容<textarea readOnly rows={10} value={editor.content} /></label> : <>
        <label>名称<input value={name} readOnly={!!doc} required pattern="[A-Za-z0-9_\-]+" placeholder="review-code"
          onChange={e => setName(e.target.value)} /></label>
        <label>简介<input value={description} required onChange={e => setDescription(e.target.value)} /></label>
        <label>Markdown 指令<textarea rows={8} value={body} required={!doc} onChange={e => setBody(e.target.value)} /></label>
        {doc && <details><summary>原始元数据</summary><pre>{splitDocument(doc.content).header}</pre></details>}
      </>}
      {error && <p className="settings-error" role="alert">{error}</p>}
      {confirm && doc ? <div role="group" aria-label="确认删除">
        <p>删除{scopeLabel[doc.scope]}技能 {doc.name} 的 SKILL.md？目录中的其他文件会保留。</p>
        <div className="skill-settings-actions">
          <button type="button" className="button" onClick={() => setConfirm(false)}>取消删除</button>
          <button type="button" className="button danger" onClick={() => void remove()}>确认删除</button>
        </div>
      </div> : <div className="skill-settings-actions">
        {!doc?.truncated && <button type="submit" className="button primary">{busy ? '正在保存…' : '保存'}</button>}
        <button type="button" className="button" onClick={onCancel}>返回列表</button>
        {doc && !doc.truncated && <button type="button" className="button danger" onClick={() => setConfirm(true)}>删除</button>}
      </div>}
    </fieldset>
  </form>;
}

export function SkillSettings() {
  const [rows, setRows] = useState<ScopedSkill[]>([]);
  const [version, setVersion] = useState(0);
  const [loading, setLoading] = useState(true);
  const [listError, setListError] = useState('');
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [editor, setEditor] = useState<Editor | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const controller = new AbortController();
    skillSettingsApi.list(controller.signal).then(items => {
      if (!controller.signal.aborted) {
        setRows(items);
        setListError('');
      }
    }).catch(e => {
      if (!controller.signal.aborted) setListError(errorText(e));
    }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [version]);

  function reload() {
    setLoading(true);
    setVersion(v => v + 1);
  }
  function begin() {
    setNotice('');
    setError('');
  }
  async function open(row: ScopedSkill) {
    begin();
    setBusy(true);
    try {
      const doc = await skillSettingsApi.open(row);
      setEditor({ kind: 'edit', doc });
    } catch (e) { setError(errorText(e)); }
    finally { setBusy(false); }
  }
  async function importFile(file: File) {
    begin();
    setBusy(true);
    try {
      if (file.size > 65536) throw new Error('SKILL.md 不能超过 64 KiB');
      const content = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(await file.arrayBuffer());
      setEditor({ kind: 'import', content });
    } catch (e) { setError(errorText(e)); }
    finally { setBusy(false); }
  }
  function done(message: string) {
    window.dispatchEvent(new Event(skillCatalogChanged));
    setEditor(null);
    setNotice(message);
    reload();
  }

  return <section className="skill-settings" aria-label="技能管理">
    <h2>技能</h2>
    <p>在对话中输入 / 选择技能。项目技能优先于同名全局技能。</p>
    {editor ? <SkillEditor editor={editor} onDone={done} onCancel={() => { setEditor(null); reload(); }} /> : <>
      <div className="skill-settings-actions">
        <button type="button" className="button primary" disabled={busy} onClick={() => { begin(); setEditor({ kind: 'new' }); }}>新建技能</button>
        <button type="button" className="button" disabled={busy} onClick={() => fileInput.current?.click()}>导入 SKILL.md</button>
        <input type="file" accept=".md,text/markdown" ref={fileInput} hidden onChange={e => {
          const file = e.target.files?.[0];
          e.target.value = '';
          if (file) void importFile(file);
        }} />
        <button type="button" className="button" disabled={busy || loading} onClick={reload}>刷新</button>
      </div>
      {error && <p className="settings-error" role="alert">{error}</p>}
      {notice && <p role="status">{notice}</p>}
      {loading ? <p role="status">正在读取技能…</p> : listError ? <p className="settings-error" role="alert">{listError}</p> :
        !rows.length ? <p>暂无技能</p> : <ul>{rows.map(row => <li key={row.scope + '/' + row.name}>
          <button type="button" disabled={busy} onClick={() => void open(row)} aria-label={`打开${scopeLabel[row.scope]}技能 ${row.name}`}>
            <strong>{row.name}</strong><small>{scopeLabel[row.scope]}{!row.effective && ' · 被项目覆盖'}</small>
            <span>{row.description}</span>
          </button>
        </li>)}</ul>}
    </>}
  </section>;
}
