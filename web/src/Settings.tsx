import { useEffect, useRef, useState } from 'react';
import { api } from './api';
import type { CustomProvider } from './api';
import { errorText } from './format';
import { Icon } from './Icon';

export function Settings({ onModelsChanged, onBack }: { onModelsChanged: () => Promise<void>; onBack: () => void }) {
  const [providers, setProviders] = useState<CustomProvider[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [query, setQuery] = useState('');
  const [editing, setEditing] = useState<CustomProvider | 'new' | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [removing, setRemoving] = useState(false);
  const [checks, setChecks] = useState<Record<string, { busy?: boolean; error?: string; message?: string }>>({});
  useEffect(() => {
    let cancelled = false;
    api.listProviders().then((items) => { if (!cancelled) setProviders(items); })
      .catch((reason: unknown) => { if (!cancelled) setError(errorText(reason)); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, []);
  const reload = async () => {
    setLoading(true); setError('');
    try { setProviders(await api.listProviders()); }
    catch (reason) { setError(errorText(reason)); }
    finally { setLoading(false); }
  };
  const changed = async () => {
    await Promise.all([reload(), onModelsChanged()]);
  };
  const testConnection = async (id: string) => {
    setChecks((previous) => ({ ...previous, [id]: { busy: true } }));
    try {
      const result = await api.testProvider(id);
      setChecks((previous) => ({ ...previous, [id]: { message: result.model_available ? '连接成功，模型已列出' : '连接成功，未列出此模型；请核对模型 ID' } }));
    } catch (reason) {
      setChecks((previous) => ({ ...previous, [id]: { error: errorText(reason) } }));
    }
  };
  const remove = async (id: string) => {
    setRemoving(true); setError(''); setNotice('');
    try {
      await api.deleteProvider(id);
      if (editing !== 'new' && editing?.id === id) setEditing(null);
      setDeleting(null);
      setNotice('已删除，请在会话顶部重新选择模型。');
      await changed();
    } catch (reason) { setError(errorText(reason)); }
    finally { setRemoving(false); }
  };
  const filtered = providers.filter((p) => (p.name + ' ' + p.model).toLowerCase().includes(query.toLowerCase()));
  return <section className="page-inner settings-page" aria-label="模型服务设置">
    <div className="page-head"><div><h2>模型服务</h2><p>OpenAI 兼容服务供所有会话使用；保存后在会话顶部选择模型。</p></div><button type="button" className="button" onClick={onBack}>返回会话</button></div>
    <div className="settings-toolbar"><input type="search" aria-label="搜索模型服务" placeholder="搜索名称或模型" value={query} onChange={(event) => setQuery(event.target.value)} /><button type="button" className="button" disabled={loading} onClick={() => void reload()}><Icon name="refresh" size={14} />刷新</button><button type="button" className="button primary" onClick={() => { setEditing('new'); setNotice(''); }}><Icon name="plus" size={14} />添加模型服务</button></div>
    {error && <p className="settings-error" role="alert">{error}</p>}
    {notice && <p role="status" className="settings-notice">{notice}</p>}
    {loading && <p role="status">正在加载模型服务…</p>}
    {!loading && !error && providers.length === 0 && <div className="page-empty"><strong>还没有自定义模型服务</strong></div>}
    {!loading && providers.length > 0 && filtered.length === 0 && <p>没有匹配的模型服务。</p>}
    <ul className="provider-list">
      {filtered.map((p) => <li key={p.id} data-provider-id={p.id} className="provider-card">
        <div className="provider-heading"><strong>{p.name}</strong><span className="chip">{p.has_api_key ? '密钥已配置' : '未配置密钥'}</span></div>
        <p>{p.model}</p><p className="provider-url">{p.base_url}</p>
        <div className="settings-actions"><button type="button" className="button" onClick={() => { setEditing(p); setNotice(''); }}>编辑</button><button type="button" className="button" disabled={checks[p.id]?.busy || removing} onClick={() => void testConnection(p.id)}>{checks[p.id]?.busy ? '检测中…' : '连接检测'}</button><button type="button" className="button" disabled={removing} onClick={() => setDeleting(p.id)}>删除</button></div>
        {checks[p.id]?.message && <p role="status">{checks[p.id].message}</p>}
        {checks[p.id]?.error && <p role="alert" className="settings-error">{checks[p.id].error}</p>}
        {deleting === p.id && <div className="provider-delete" role="group" aria-label="确认删除模型服务"><p>删除「{p.name}」？当前模型可能不可用，请在会话顶部重新选择。</p><div className="settings-actions"><button type="button" className="button" disabled={removing} onClick={() => setDeleting(null)}>取消</button><button type="button" className="button" disabled={removing} onClick={() => void remove(p.id)}>{removing ? '删除中…' : '确认删除'}</button></div></div>}
      </li>)}
    </ul>
    {editing && <ProviderForm key={editing === 'new' ? 'new' : editing.id} provider={editing === 'new' ? undefined : editing} onCancel={() => setEditing(null)} onSave={async (value) => {
      await api.saveProvider(value);
      setEditing(null); setNotice('已保存，请在会话顶部选择模型。');
      setChecks((previous) => { const next = { ...previous }; delete next[value.id]; return next; });
      await changed();
    }} />}
    <p className="settings-scope">连接检测仅检查模型列表，不测试推理；也可手动填写模型 ID。</p>
  </section>;
}

function ProviderForm({ provider, onSave, onCancel }: { provider?: CustomProvider; onSave: (value: Parameters<typeof api.saveProvider>[0]) => Promise<void>; onCancel: () => void }) {
  const form = useRef<HTMLFormElement>(null);
  const [name, setName] = useState(provider?.name || '');
  const [baseURL, setBaseURL] = useState(provider?.base_url || '');
  const [model, setModel] = useState(provider?.model || '');
  const [key, setKey] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => { form.current?.scrollIntoView({ block: 'nearest' }); form.current?.querySelector('input')?.focus({ preventScroll: true }); }, []);
  return <form ref={form} className="provider-form" aria-label={provider ? '编辑模型服务' : '添加模型服务'} onSubmit={async (event) => {
    event.preventDefault(); setSaving(true); setError('');
    try {
      await onSave({ id: provider?.id || crypto.randomUUID(), name, base_url: baseURL, model, ...(key ? { api_key: key } : {}) });
      setKey('');
    } catch (reason) { setError(errorText(reason)); }
    finally { setSaving(false); }
  }}>
    <h3>{provider ? '编辑模型服务' : '添加模型服务'}</h3>
    <div className="provider-form-fields">
      <label htmlFor="provider-name">显示名称<input id="provider-name" value={name} maxLength={100} required onChange={(event) => setName(event.target.value)} /></label>
      <label htmlFor="provider-url">Base URL<input id="provider-url" type="url" value={baseURL} required placeholder="https://api.example.com/v1" onChange={(event) => setBaseURL(event.target.value)} /><small>必须 HTTPS，仅 loopback 允许 HTTP；不接受 URL 中的用户名、密码或查询参数。</small></label>
      <label htmlFor="provider-model">模型 ID<input id="provider-model" value={model} maxLength={200} required onChange={(event) => setModel(event.target.value)} /></label>
      <label htmlFor="provider-key">API key<input id="provider-key" type="password" autoComplete="new-password" value={key} required={!provider} placeholder={provider?.has_api_key ? '已保存；留空保留原值' : '输入 API key'} onChange={(event) => setKey(event.target.value)} /><small>{provider ? '留空保留已保存的密钥，填写则替换；删除服务会同时移除其凭据。' : '密钥只发送给服务端保存，不在列表或 API 响应中回显。'}</small></label>
    </div>
    {error && <p role="alert" className="settings-error">{error}</p>}
    <div className="settings-actions"><button type="submit" className="button primary" disabled={saving}>{saving ? '保存中…' : '保存模型服务'}</button><button type="button" className="button" disabled={saving} onClick={onCancel}>取消</button></div>
  </form>;
}
