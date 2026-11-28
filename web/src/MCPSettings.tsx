import { useEffect, useState } from 'react';
import { errorText } from './format';
import { mcpApi, type MCPServer } from './mcpApi';

const states: Record<string, string> = { running: '已连接', failed: '连接失败', auth_required: '需要授权', disconnected: '已断开' };

export function MCPSettings() {
  const [servers, setServers] = useState<MCPServer[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(true);
  const [query, setQuery] = useState('');
  useEffect(() => {
    const abort = new AbortController();
    mcpApi.list(abort.signal).then((items) => { if (!abort.signal.aborted) setServers(items); })
      .catch((reason: unknown) => { if (!abort.signal.aborted) setError(errorText(reason)); })
      .finally(() => { if (!abort.signal.aborted) setBusy(false); });
    return () => abort.abort();
  }, []);
  async function update(action?: () => Promise<unknown>) {
    setBusy(true); setError('');
    try { await action?.(); }
    catch (reason) { setError(errorText(reason)); }
    try { setServers(await mcpApi.list()); }
    catch (reason) { setError(errorText(reason)); }
    finally { setBusy(false); }
  }
  return <section className="mcp-settings" aria-label="MCP 服务">
    <div className="page-head"><h2>MCP 服务</h2><button className="button" disabled={busy} onClick={() => void update()}>刷新状态</button></div>
    {error && <p role="alert" className="settings-error">{error}</p>}
    {!servers.length && !error && <p className="muted" role="status">{busy ? '正在读取服务…' : '暂无服务。在配置文件的 servers 中添加 MCP 服务。'}</p>}
    {servers.some((s) => s.tools?.length) && <div className="settings-toolbar"><input aria-label="搜索 MCP 工具" placeholder="搜索工具名称" type="search" value={query} onChange={(e) => setQuery(e.target.value)} /></div>}
    <ul className="provider-list">{servers.map((s, index) => {
      const matches = s.tools?.filter((name) => name.toLowerCase().includes(query.trim().toLowerCase())) || [];
      return <li className="provider-card" key={s.name + index}>
        <div className="provider-heading"><strong>{s.name}</strong><span className="chip">{states[s.state] || s.state}</span></div>
        <p className="muted">{s.transport === 'stdio' ? '本地 stdio' : 'Streamable HTTP'} · {s.readonly ? '只读' : '可写'}{s.auth === 'oauth' ? ' · OAuth' : s.auth === 'token_env' ? ' · 环境令牌' : ''}</p>
        {s.error && <p className="settings-error">{s.error}</p>}
        {s.warnings?.map((warning, i) => <p className="muted" key={i}>{warning}</p>)}
        {!!s.tools?.length && <details open={query ? true : undefined}><summary>工具列表（{query ? matches.length + '/' : ''}{s.tools.length}）</summary><MCPInventory key={query} names={matches} /></details>}
        <div className="settings-actions">
          <button className="button" disabled={busy} onClick={() => void update(() => mcpApi.connect(s.name))}>{s.state === 'running' ? '重新连接' : '连接'}</button>
          {s.state !== 'disconnected' && <button className="button" disabled={busy} onClick={() => void update(() => mcpApi.disconnect(s.name))}>断开</button>}
          {s.auth === 'oauth' && <button className="button" disabled={busy} onClick={() => void update(async () => {
            const { url } = await mcpApi.authorize(s.name); window.location.assign(url);
          })}>{s.state === 'running' ? '重新授权' : '授权'}</button>}
        </div>
      </li>;
    })}</ul>
  </section>;
}

function MCPInventory({ names }: { names: string[] }) {
  const [count, setCount] = useState(50);
  return <><ul className="mcp-inventory">{names.slice(0, count).map((name) => <li key={name}>{name}</li>)}</ul>
    {count < names.length && <button className="button" onClick={() => setCount(count + 50)}>显示更多</button>}</>;
}
