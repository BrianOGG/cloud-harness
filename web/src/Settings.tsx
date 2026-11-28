import { useEffect, useRef, useState } from 'react';
import { api } from './api';
import type { CatalogModel, CustomProvider, ProviderProtocol } from './api';
import { errorText } from './format';
import { Icon } from './Icon';
import { ProjectMemories } from './ProjectMemory';

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
  const testConnection = async (id: string) => {
    setChecks((previous) => ({ ...previous, [id]: { busy: true } }));
    try {
      const result = await api.testProvider(id);
      setChecks((previous) => ({ ...previous, [id]: { message: result.model_available ? '连接成功，模型已列出' : '连接成功，未列出此模型；请核对模型 ID' } }));
    } catch (reason) {
      setChecks((previous) => ({ ...previous, [id]: { error: errorText(reason) } }));
    }
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
      setNotice('已删除模型服务。');
      await changed();
    } catch (reason) { setError(errorText(reason)); }
    finally { setRemoving(false); }
  };
  const filtered = providers.filter((p) => (p.name + ' ' + p.model).toLowerCase().includes(query.toLowerCase()));
  return <section className="page-inner settings-page" aria-label="模型服务设置">
    <button type="button" className="link-button settings-back" onClick={onBack}><Icon name="right" size={13} />返回会话</button>
    <div className="page-head"><h2>模型服务</h2><button type="button" className="button primary" onClick={() => { setEditing('new'); setNotice(''); }}><Icon name="plus" size={14} />添加模型服务</button></div>
    <div className="settings-toolbar"><input type="search" aria-label="搜索模型服务" placeholder="搜索名称或模型" value={query} onChange={(event) => setQuery(event.target.value)} /><button type="button" className="button" disabled={loading} onClick={() => void reload()}><Icon name="refresh" size={14} />刷新</button></div>
    {error && <p className="settings-error" role="alert">{error}</p>}
    {notice && <p role="status" className="settings-notice">{notice}</p>}
    {loading && <p role="status">正在加载模型服务…</p>}
    {!loading && !error && providers.length === 0 && !editing && <div className="page-empty"><Icon name="panel" size={22} /><strong>还没有自定义模型服务</strong></div>}
    {!loading && providers.length > 0 && filtered.length === 0 && <p>没有匹配的模型服务。</p>}
    {editing && <ProviderForm key={editing === 'new' ? 'new' : editing.id} provider={editing === 'new' ? undefined : editing} onCancel={() => setEditing(null)} onSave={async (value) => {
      await api.saveProvider(value);
      setEditing(null); setNotice('已保存。');
      setChecks((previous) => { const next = { ...previous }; delete next[value.id]; return next; });
      await changed();
    }} />}
    <ul className="provider-list">
      {filtered.map((p) => <li key={p.id} className="provider-card">
        <div className="provider-heading"><strong>{p.name}</strong><span className="chip">{protocolNames[p.protocol || 'openai']} · {(p.models || [p.model]).length} 个模型</span></div>
        <dl className="provider-info">
          <div><dt>地址</dt><dd>{p.base_url}</dd></div>
          <div><dt>默认模型</dt><dd>{p.model}</dd></div>
          <div><dt>密钥</dt><dd>{p.has_api_key ? '已配置' : '未配置'}</dd></div>
        </dl>
        <div className="settings-actions"><button type="button" className="button" onClick={() => { setEditing(p); setNotice(''); }}>编辑</button><button type="button" className="button" disabled={checks[p.id]?.busy || removing} onClick={() => void testConnection(p.id)}>{checks[p.id]?.busy ? '检测中…' : '连接检测'}</button><button type="button" className="button" disabled={checks[p.id]?.busy || removing} onClick={() => void refreshCatalog(p.id)}>{checks[p.id]?.busy ? '刷新中…' : '刷新模型列表'}</button><button type="button" className="button" disabled={removing} onClick={() => setDeleting(p.id)}>删除</button></div>
        {checks[p.id]?.message && <p role="status">{checks[p.id].message}</p>}
        {checks[p.id]?.error && <p role="alert" className="settings-error">{checks[p.id].error}</p>}
        {deleting === p.id && <div className="provider-delete" role="group" aria-label="确认删除模型服务"><p>删除「{p.name}」？当前模型可能不可用，请重新选择。</p><div className="settings-actions"><button type="button" className="button" disabled={removing} onClick={() => setDeleting(null)}>取消</button><button type="button" className="button" disabled={removing} onClick={() => void remove(p.id)}>{removing ? '删除中…' : '确认删除'}</button></div></div>}
      </li>)}
    </ul>
    <ProjectMemories />
  </section>;
}

function ProviderForm({ provider, onSave, onCancel }: { provider?: CustomProvider; onSave: (value: Parameters<typeof api.saveProvider>[0]) => Promise<void>; onCancel: () => void }) {
  const form = useRef<HTMLFormElement>(null);
  const [name, setName] = useState(provider?.name || '');
  const [baseURL, setBaseURL] = useState(provider?.base_url || '');
  const [model, setModel] = useState(provider?.model || '');
  const [modelQuery, setModelQuery] = useState('');
  const [key, setKey] = useState('');
  const [protocol, setProtocol] = useState<ProviderProtocol>(provider?.protocol || 'openai');
  const [source, setSource] = useState<'auto' | 'manual'>(provider?.protocol_source || 'auto');
  const [catalog, setCatalog] = useState<CatalogModel[]>(provider?.catalog || []);
  const [fetchedAt, setFetchedAt] = useState(provider?.catalog_fetched_at);
  const [enabled, setEnabled] = useState<string[]>([...new Set([...(provider?.models || []), ...(provider?.model ? [provider.model] : [])])]);
  const [detecting, setDetecting] = useState(false);
  const [detected, setDetected] = useState(false);
  const [detectError, setDetectError] = useState('');
  const [alternatives, setAlternatives] = useState<ProviderProtocol[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const generation = useRef({ version: 0 });
  useEffect(() => {
    const requests = generation.current;
    form.current?.scrollIntoView({ block: 'nearest' }); form.current?.querySelector('input')?.focus({ preventScroll: true });
    return () => { requests.version++; };
  }, []);
  const invalidate = () => { generation.current.version++; setDetected(false); setDetecting(false); setDetectError(''); setAlternatives([]); };
  const detect = async () => {
    if (!baseURL.trim() || (!key && !provider?.has_api_key) || saving) return;
    const identity = ++generation.current.version;
    setDetecting(true); setDetectError('');
    try {
      const result = await api.detectProvider({ base_url: baseURL, ...(key ? { api_key: key } : {}), ...(provider ? { provider_id: provider.id } : {}), ...(source === 'manual' ? { protocol } : {}) });
      if (identity !== generation.current.version) return;
      setProtocol(result.protocol); setCatalog(result.models); setFetchedAt(result.fetched_at); setDetected(true); setAlternatives(result.alternatives || []);
      const first = result.models.find((item) => !isOtherModel(item));
      if (!model && first) { setModel(first.id); setEnabled((previous) => [...new Set([...previous, first.id])]); }
    } catch { if (identity === generation.current.version) setDetectError('获取模型列表失败，可手填模型名并保存。'); }
    finally { if (identity === generation.current.version) setDetecting(false); }
  };
  const choices: CatalogModel[] = [...new Map<string, CatalogModel>([...enabled.map((id) => ({ id })), ...(model ? [{ id: model }] : []), ...catalog].map((item) => [item.id, item] as [string, CatalogModel])).values()];
  const filteredChoices = choices.filter((item) => (item.id + ' ' + (item.display_name || '')).toLowerCase().includes(modelQuery.trim().toLowerCase()));
  const visibleChoices = filteredChoices.filter((item) => !isOtherModel(item) || enabled.includes(item.id) || item.id === model);
  const otherChoices = filteredChoices.filter((item) => isOtherModel(item) && !enabled.includes(item.id) && item.id !== model);
  const addModel = () => {
    const id = modelQuery.trim();
    if (!id || id.length > 200) return;
    generation.current.version++; setDetecting(false); setError('');
    setEnabled((previous) => [...new Set([...previous, id])]);
    if (!model) setModel(id);
    setModelQuery('');
  };
  const renderModel = (item: CatalogModel) => <div className="provider-model-row" key={item.id} data-model-id={item.id}>
    <label title={item.id}><input className="model-choice" type="checkbox" aria-label={`启用 ${item.id}`} checked={enabled.includes(item.id) || item.id === model} disabled={item.id === model} onChange={(event) => {
      generation.current.version++; setDetecting(false);
      setEnabled((previous) => event.target.checked ? [...new Set([...previous, item.id])] : previous.filter((id) => id !== item.id));
      if (event.target.checked && !model) setModel(item.id);
    }} /><span>{item.display_name || item.id}{item.display_name && item.display_name !== item.id && <small>{item.id}</small>}</span>{isOtherModel(item) && <small>其他</small>}</label>
    <label className="provider-model-default"><input className="model-choice" type="radio" name="default-model" aria-label={`默认模型 ${item.id}`} checked={item.id === model} disabled={!enabled.includes(item.id) && item.id !== model} onChange={() => { generation.current.version++; setDetecting(false); setModel(item.id); }} />默认</label>
  </div>;
  return <form ref={form} className="provider-form" aria-label={provider ? '编辑模型服务' : '添加模型服务'} onSubmit={async (event) => {
    event.preventDefault();
    if (!model || !enabled.includes(model)) { setError('请先勾选模型并设为默认。'); return; }
    generation.current.version++; setDetecting(false); setSaving(true); setError('');
    try {
      await onSave({ id: provider?.id || crypto.randomUUID(), name, base_url: baseURL, model, protocol, protocol_source: source, models: [...new Set([model, ...enabled])], catalog, ...(fetchedAt ? { catalog_fetched_at: fetchedAt } : {}), ...(key ? { api_key: key } : {}) });
      setKey('');
    } catch (reason) { setError(errorText(reason)); }
    finally { setSaving(false); }
  }}>
    <h3>{provider ? '编辑模型服务' : '添加模型服务'}</h3>
    <div className="provider-form-fields">
      <label htmlFor="provider-name">显示名称<input id="provider-name" value={name} maxLength={100} required onChange={(event) => setName(event.target.value)} /></label>
      <label htmlFor="provider-url">Base URL<input id="provider-url" type="url" title="仅支持 HTTPS 或本机回环 HTTP" value={baseURL} required placeholder="https://api.example.com/v1" onChange={(event) => { invalidate(); setBaseURL(event.target.value); setCatalog([]); setFetchedAt(undefined); }} onBlur={() => void detect()} /></label>
      <label htmlFor="provider-key">API key<input id="provider-key" type="password" autoComplete="new-password" value={key} required={!provider} placeholder={provider?.has_api_key ? '已保存；留空保留原值' : '输入 API key'} onChange={(event) => { invalidate(); setKey(event.target.value); }} onBlur={() => void detect()} /></label>
      <div className="settings-actions provider-protocol"><label htmlFor="provider-protocol">协议<select id="provider-protocol" title={alternatives.length ? `该服务也兼容 ${alternatives.map((item) => protocolNames[item]).join('、')}；可手动更改` : '可手动更改自动识别结果'} value={protocol} onChange={(event) => { invalidate(); setProtocol(event.target.value as ProviderProtocol); setSource('manual'); setCatalog([]); setFetchedAt(undefined); }}>{Object.entries(protocolNames).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label><button type="button" className="button" title="识别协议并读取模型目录" disabled={detecting || saving || !baseURL || (!key && !provider?.has_api_key)} onClick={() => void detect()}>{detecting ? '识别中…' : '识别'}</button>{detected && <span role="status">{source === 'auto' ? '已识别' : '已选择'}：{protocolNames[protocol]}</span>}</div>
      <div className="provider-model-picker" role="group" aria-label="模型列表">
        <div className="provider-model-search"><label htmlFor="provider-model-search">模型<input id="provider-model-search" type="search" placeholder="搜索名称或输入模型 ID" value={modelQuery} maxLength={200} onChange={(event) => setModelQuery(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); addModel(); } }} /></label><button type="button" className="button" disabled={!modelQuery.trim()} onClick={addModel}>添加并勾选</button></div>
        <div className="provider-model-list">
          {visibleChoices.map(renderModel)}
          {otherChoices.length > 0 && <details className="provider-other-models"><summary>其他（{otherChoices.length}）</summary>{otherChoices.map(renderModel)}</details>}
          {filteredChoices.length === 0 && <p className="muted">{modelQuery ? '没有匹配的模型，可添加此 ID。' : '识别模型目录或输入模型 ID 添加。'}</p>}
        </div>
      </div>
    </div>
    {detectError && <p role="status" className="settings-error">{detectError}</p>}
    {error && <p role="alert" className="settings-error">{error}</p>}
    <div className="settings-actions"><button type="submit" className="button primary" disabled={saving}>{saving ? '保存中…' : '保存模型服务'}</button><button type="button" className="button" disabled={saving} onClick={onCancel}>取消</button></div>
  </form>;
}

function isOtherModel(item: CatalogModel): boolean {
  if (item.category) return item.category === 'other';
  return /tts|asr|realtime|real-time|whisper|embedding|embed-|image|video|router|rerank|transcrib|speech/i.test(item.id + ' ' + (item.display_name || ''));
}
