import { useEffect, useRef, useState } from 'react';
import { api } from './api';
import type { CatalogModel, CustomProvider, ProviderProtocol } from './api';
import { errorText } from './format';
import { Icon } from './Icon';

const protocolNames: Record<ProviderProtocol, string> = { openai: 'OpenAI 兼容', anthropic: 'Anthropic', gemini: 'Gemini' };

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
  const refreshCatalog = async (id: string) => {
    setChecks((previous) => ({ ...previous, [id]: { busy: true } }));
    try {
      const result = await api.refreshProviderModels(id);
      setProviders((previous) => previous.map((p) => p.id === id ? result : p));
      await onModelsChanged();
      setChecks((previous) => ({ ...previous, [id]: { message: '已刷新模型列表' } }));
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
      setNotice('模型服务已删除；会话中原来的选择不会被自动替换。');
      await changed();
    } catch (reason) { setError(errorText(reason)); }
    finally { setRemoving(false); }
  };
  const filtered = providers.filter((p) => (p.name + ' ' + p.model).toLowerCase().includes(query.toLowerCase()));
  return <section className="page-inner settings-page" aria-label="模型服务设置">
    <div className="page-head"><div><h2>模型服务</h2><p>模型服务为全局 / 用户配置；返回会话后选择模型和下一轮设置。</p></div><button type="button" className="button" onClick={onBack}>返回会话</button></div>
    <div className="settings-toolbar"><input type="search" aria-label="搜索模型服务" placeholder="搜索名称或模型" value={query} onChange={(event) => setQuery(event.target.value)} /><button type="button" className="button" disabled={loading} onClick={() => void reload()}><Icon name="refresh" size={14} />刷新</button><button type="button" className="button primary" onClick={() => { setEditing('new'); setNotice(''); }}><Icon name="plus" size={14} />添加模型服务</button></div>
    {error && <p className="settings-error" role="alert">{error}</p>}
    {notice && <p role="status" className="settings-notice">{notice}</p>}
    {loading && <p role="status">正在加载模型服务…</p>}
    {!loading && !error && providers.length === 0 && <div className="page-empty"><strong>还没有自定义模型服务</strong><span>添加服务后，返回会话选择模型；现有草稿会保留。</span></div>}
    {!loading && providers.length > 0 && filtered.length === 0 && <p>没有匹配的模型服务。</p>}
    <ul className="provider-list">
      {filtered.map((p) => <li key={p.id} data-provider-id={p.id} className="provider-card">
        <div className="provider-heading"><strong>{p.name}</strong><span className="chip">{protocolNames[p.protocol || 'openai']} · {(p.models || [p.model]).length} 个模型</span></div>
        <p>{p.model}</p><p className="provider-url">{p.base_url}</p><p>{p.has_api_key ? '密钥已配置 · ••••••' : '未配置密钥'}</p>
        <div className="settings-actions"><button type="button" className="button" onClick={() => { setEditing(p); setNotice(''); }}>编辑</button><button type="button" className="button" disabled={checks[p.id]?.busy || removing} onClick={() => void refreshCatalog(p.id)}>{checks[p.id]?.busy ? '刷新中…' : '刷新模型列表'}</button><button type="button" className="button" disabled={removing} onClick={() => setDeleting(p.id)}>删除</button></div>
        {checks[p.id]?.message && <p role="status">{checks[p.id].message}</p>}
        {checks[p.id]?.error && <p role="alert" className="settings-error">{checks[p.id].error}</p>}
        {deleting === p.id && <div className="provider-delete" role="group" aria-label="确认删除模型服务"><p>删除「{p.name}」？使用此服务的当前模型可能变得不可用，之后需要在会话 header 重新选择。</p><div className="settings-actions"><button type="button" className="button" disabled={removing} onClick={() => setDeleting(null)}>取消</button><button type="button" className="button" disabled={removing} onClick={() => void remove(p.id)}>{removing ? '删除中…' : '确认删除'}</button></div></div>}
      </li>)}
    </ul>
    {editing && <ProviderForm key={editing === 'new' ? 'new' : editing.id} provider={editing === 'new' ? undefined : editing} onCancel={() => setEditing(null)} onSave={async (value) => {
      await api.saveProvider(value);
      setEditing(null); setNotice('已保存。返回会话后，请在 header 自行选择模型。');
      setChecks((previous) => { const next = { ...previous }; delete next[value.id]; return next; });
      await changed();
    }} />}
  </section>;
}

function ProviderForm({ provider, onSave, onCancel }: { provider?: CustomProvider; onSave: (value: Parameters<typeof api.saveProvider>[0]) => Promise<void>; onCancel: () => void }) {
  const form = useRef<HTMLFormElement>(null);
  const [name, setName] = useState(provider?.name || '');
  const [baseURL, setBaseURL] = useState(provider?.base_url || '');
  const [model, setModel] = useState(provider?.model || '');
  const [key, setKey] = useState('');
  const [protocol, setProtocol] = useState<ProviderProtocol>(provider?.protocol || 'openai');
  const [source, setSource] = useState<'auto' | 'manual'>(provider?.protocol_source || 'auto');
  const [catalog, setCatalog] = useState<CatalogModel[]>(provider?.catalog || []);
  const [fetchedAt, setFetchedAt] = useState(provider?.catalog_fetched_at);
  const [enabled, setEnabled] = useState<string[]>(provider?.models || (provider?.model ? [provider.model] : []));
  const [detecting, setDetecting] = useState(false);
  const [detected, setDetected] = useState(false);
  const [detectError, setDetectError] = useState('');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const generation = useRef({ version: 0 });
  useEffect(() => {
    const requests = generation.current;
    form.current?.scrollIntoView({ block: 'nearest' }); form.current?.querySelector('input')?.focus({ preventScroll: true });
    return () => { requests.version++; };
  }, []);
  const invalidate = () => { generation.current.version++; setDetected(false); setDetecting(false); setDetectError(''); };
  const detect = async () => {
    if (!baseURL.trim() || (!key && !provider?.has_api_key) || saving) return;
    const identity = ++generation.current.version;
    setDetecting(true); setDetectError('');
    try {
      const result = await api.detectProvider({ base_url: baseURL, ...(key ? { api_key: key } : {}), ...(provider ? { provider_id: provider.id } : {}), ...(source === 'manual' ? { protocol } : {}) });
      if (identity !== generation.current.version) return;
      setProtocol(result.protocol); setCatalog(result.models); setFetchedAt(result.fetched_at); setDetected(true);
      if (!model && result.models[0]) { setModel(result.models[0].id); setEnabled((previous) => [...new Set([...previous, result.models[0].id])]); }
    } catch { if (identity === generation.current.version) setDetectError('获取模型列表失败，可手填模型名并保存。'); }
    finally { if (identity === generation.current.version) setDetecting(false); }
  };
  const choices = [...new Map([...catalog, ...enabled.map((id) => ({ id })), ...(model ? [{ id: model }] : [])].map((item) => [item.id, item])).values()];
  return <form ref={form} className="provider-form" aria-label={provider ? '编辑模型服务' : '添加模型服务'} onSubmit={async (event) => {
    event.preventDefault(); generation.current.version++; setDetecting(false); setSaving(true); setError('');
    try {
      await onSave({ id: provider?.id || crypto.randomUUID(), name, base_url: baseURL, model, protocol, protocol_source: source, models: [...new Set([model, ...enabled])], catalog, ...(fetchedAt ? { catalog_fetched_at: fetchedAt } : {}), ...(key ? { api_key: key } : {}) });
      setKey('');
    } catch (reason) { setError(errorText(reason)); }
    finally { setSaving(false); }
  }}>
    <h3>{provider ? '编辑模型服务' : '添加模型服务'}</h3>
    <div className="provider-form-fields">
      <label htmlFor="provider-name">显示名称<input id="provider-name" value={name} maxLength={100} required onChange={(event) => setName(event.target.value)} /></label>
      <label htmlFor="provider-url">Base URL<input id="provider-url" type="url" value={baseURL} required placeholder="https://api.example.com/v1" onChange={(event) => { invalidate(); setBaseURL(event.target.value); setCatalog([]); setFetchedAt(undefined); }} onBlur={() => void detect()} /><small>必须 HTTPS，仅本机回环地址允许 HTTP。</small></label>
      <label htmlFor="provider-key">API key<input id="provider-key" type="password" autoComplete="new-password" value={key} required={!provider} placeholder={provider?.has_api_key ? '已保存；留空保留原值' : '输入 API key'} onChange={(event) => { invalidate(); setKey(event.target.value); }} onBlur={() => void detect()} /></label>
      <div className="settings-actions provider-protocol"><label htmlFor="provider-protocol">协议<select id="provider-protocol" value={protocol} onChange={(event) => { invalidate(); setProtocol(event.target.value as ProviderProtocol); setSource('manual'); setCatalog([]); setFetchedAt(undefined); }}>{Object.entries(protocolNames).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label><button type="button" className="button" disabled={detecting || saving || !baseURL || (!key && !provider?.has_api_key)} onClick={() => void detect()}>{detecting ? '识别中…' : '识别'}</button>{detected && <span role="status">{source === 'auto' ? '已识别' : '已选择'}：{protocolNames[protocol]}</span>}</div>
      <label htmlFor="provider-model">模型名 / 手动模型 ID<input id="provider-model" list="provider-model-catalog" value={model} maxLength={200} required onChange={(event) => { generation.current.version++; setDetecting(false); setModel(event.target.value); }} /><datalist id="provider-model-catalog">{catalog.map((item) => <option key={item.id} value={item.id}>{item.display_name || item.id}</option>)}</datalist></label>
      {choices.length > 0 && <fieldset className="provider-models"><legend>显示在会话模型菜单</legend>{choices.map((item) => <label key={item.id}><input type="checkbox" checked={item.id === model || enabled.includes(item.id)} disabled={item.id === model} onChange={(event) => setEnabled((previous) => event.target.checked ? [...new Set([...previous, item.id])] : previous.filter((id) => id !== item.id))} />{item.id}{item.id === model && <small>默认</small>}</label>)}</fieldset>}
    </div>
    {detectError && <p role="status" className="settings-error">{detectError}</p>}
    {error && <p role="alert" className="settings-error">{error}</p>}
    <div className="settings-actions"><button type="submit" className="button primary" disabled={saving}>{saving ? '保存中…' : '保存模型服务'}</button><button type="button" className="button" disabled={saving} onClick={onCancel}>取消</button></div>
  </form>;
}
