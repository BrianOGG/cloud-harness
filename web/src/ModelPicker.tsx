import { useEffect, useId, useRef, useState } from 'react';
import type { KeyboardEvent } from 'react';
import type { ModelOption } from './api';
import { Icon } from './Icon';

export function ModelPicker({ models, selectedModel, loading, disabled, onChange }: {
  models: ModelOption[];
  selectedModel?: ModelOption;
  loading?: boolean;
  disabled?: boolean;
  onChange: (id: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const ref = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const search = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLDivElement>(null);
  const listId = useId();
  const expanded = open && !disabled;
  const searchable = models.length >= 8;
  const filtered = models.filter((model) => [model.name, model.model, model.id, model.provider_name].join(' ').toLowerCase().includes(query.trim().toLowerCase()));
  // Provider IDs stay distinct, including a provider literally named "default".
  const groups = [...new Set(filtered.map((model) => model.provider_id || ''))];
  if (open && disabled) setOpen(false);
  useEffect(() => {
    if (!expanded) return;
    const outside = (event: PointerEvent) => { if (!ref.current?.contains(event.target as Node)) setOpen(false); };
    const escape = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape') { event.preventDefault(); setOpen(false); button.current?.focus(); }
    };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('keydown', escape);
    if (search.current) search.current.focus();
    else (list.current?.querySelector<HTMLButtonElement>('[aria-selected="true"]') || list.current?.querySelector<HTMLButtonElement>('[role="option"]'))?.focus();
    return () => { document.removeEventListener('pointerdown', outside); document.removeEventListener('keydown', escape); };
  }, [expanded]);
  const navigate = (event: KeyboardEvent<HTMLDivElement>) => {
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    if (event.target === search.current && ['Home', 'End'].includes(event.key)) return;
    event.preventDefault();
    const options = [...(list.current?.querySelectorAll<HTMLButtonElement>('[role="option"]') || [])];
    if (options.length === 0) return;
    const index = options.indexOf(document.activeElement as HTMLButtonElement);
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? options.length - 1 : event.key === 'ArrowDown' ? (index + 1) % options.length : (index < 0 ? options.length - 1 : (index - 1 + options.length) % options.length);
    options[next]?.focus();
  };
  return <div className="composer-model" ref={ref} onBlur={(event) => { if (event.relatedTarget && !event.currentTarget.contains(event.relatedTarget as Node)) setOpen(false); }}>
    <button ref={button} type="button" className="value-chip composer-model-button" aria-label="会话模型" aria-haspopup="listbox" aria-expanded={expanded} aria-controls={expanded ? listId : undefined} aria-busy={loading} title="下一轮模型" disabled={disabled} data-model-id={selectedModel?.id} onClick={() => { setQuery(''); setOpen(!expanded); }} onKeyDown={(event) => {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') { event.preventDefault(); setQuery(''); setOpen(true); }
    }}><span>{selectedModel?.name || (loading ? '加载中…' : models.length === 0 ? '未配置模型' : '模型不可用')}</span><Icon name="down" size={12} /></button>
    {expanded && <div className="composer-model-popover" onKeyDown={navigate}>
      {searchable && <input ref={search} type="search" aria-label="搜索会话模型" placeholder="搜索模型或服务" value={query} onChange={(event) => setQuery(event.target.value)} />}
      <div id={listId} ref={list} className="composer-model-list" role="listbox" aria-label="选择会话模型">
        {groups.map((group, index) => <div key={group} role="group" aria-labelledby={listId + '-' + index}>
          <div className="model-group-label" id={listId + '-' + index}>{models.find((model) => (model.provider_id || '') === group)?.provider_name || (group === '' ? '默认服务' : group)}</div>
          {filtered.filter((model) => (model.provider_id || '') === group).map((model) => <button key={model.id} type="button" role="option" tabIndex={-1} className="composer-model-option" data-model-id={model.id} aria-selected={model.id === selectedModel?.id} title={model.name} onClick={() => {
            setOpen(false);
            if (model.id !== selectedModel?.id) onChange(model.id);
            button.current?.focus();
          }}><span>{model.name}</span>{model.id === selectedModel?.id && <Icon name="check" size={12} />}</button>)}
        </div>)}
        {filtered.length === 0 && <p className="model-search-empty" role="status">没有匹配的模型</p>}
      </div>
    </div>}
  </div>;
}
