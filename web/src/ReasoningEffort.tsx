import { useEffect, useId, useRef, useState } from 'react';
import type { ModelOption, ReasoningEffort } from './api';
import { Icon } from './Icon';

const efforts = [
  { id: 'default', label: '默认' },
  { id: 'off', label: '关' },
  { id: 'low', label: '低' },
  { id: 'medium', label: '中' },
  { id: 'high', label: '高' },
] as const;

export function ReasoningEffortMenu({ value, onChange, disabled, unsupported, model }: { model?: ModelOption; value?: ReasoningEffort; onChange: (value: ReasoningEffort) => void; disabled?: boolean; unsupported?: boolean }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const menuId = useId();
  const toggle = model?.reasoning_type === 'toggle';
  const values = model?.reasoning_values || [];
  const available = (effort: string) => {
    if (effort === 'default' || model?.reasoning_type !== 'effort') return true;
    if (effort === 'off') return values.includes('none') || values.includes('off');
    if (effort === 'low') return values.includes('low') || values.includes('minimal');
    return values.includes(effort);
  };
  const options: { id: ReasoningEffort; label: string }[] = toggle ? [{ id: 'off', label: '关' }, { id: 'default', label: '开' }] : [...efforts];
  const selected = toggle ? (value === 'off' ? 'off' : 'default') : value;
  const current = options.find((effort) => effort.id === selected && available(effort.id)) || options.find((effort) => effort.id === 'default')!;
  const unavailable = disabled || unsupported;
  if (open && unavailable) setOpen(false);
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent) => { if (!ref.current?.contains(event.target as Node)) setOpen(false); };
    const escape = (event: globalThis.KeyboardEvent) => { if (event.key === 'Escape') { setOpen(false); button.current?.focus(); } };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('keydown', escape);
    return () => { document.removeEventListener('pointerdown', outside); document.removeEventListener('keydown', escape); };
  }, [open]);
  return <div className="reasoning-effort" ref={ref}>
    <button ref={button} type="button" className="value-chip header-reasoning" aria-label="思考强度" aria-expanded={open && !unavailable} aria-controls={menuId} title="下一轮生效" disabled={unavailable} onClick={() => setOpen(!open)}><span>思考：{unsupported ? '不支持' : current.label}</span><Icon name="down" size={12} /></button>
    {open && !unavailable && <div id={menuId} className="reasoning-effort-popover" role="group" aria-label="选择思考强度">
      {options.map((effort) => <button type="button" disabled={!available(effort.id)} key={effort.id} className="reasoning-effort-option" aria-pressed={current.id === effort.id} onClick={() => {
        setOpen(false);
        if (effort.id !== current.id) onChange(effort.id);
        button.current?.focus();
      }}><span>{effort.label}</span>{current.id === effort.id && <Icon name="check" size={12} />}</button>)}
    </div>}
  </div>;
}
