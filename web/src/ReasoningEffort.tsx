import { useEffect, useId, useRef, useState } from 'react';
import type { ReasoningEffort } from './api';
import { Icon } from './Icon';

const efforts = [
  { id: 'default', label: '默认' },
  { id: 'off', label: '关' },
  { id: 'low', label: '低' },
  { id: 'medium', label: '中' },
  { id: 'high', label: '高' },
] as const;

export function ReasoningEffortMenu({ value, onChange, disabled, unsupported }: { value?: ReasoningEffort; onChange: (value: ReasoningEffort) => void; disabled?: boolean; unsupported?: boolean }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const menuId = useId();
  const current = efforts.find((effort) => effort.id === value) || efforts[0];
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
      {efforts.map((effort) => <button type="button" key={effort.id} className="reasoning-effort-option" aria-pressed={current.id === effort.id} onClick={() => {
        setOpen(false);
        if (effort.id !== current.id) onChange(effort.id);
        button.current?.focus();
      }}><span>{effort.label}</span>{current.id === effort.id && <Icon name="check" size={12} />}</button>)}
    </div>}
  </div>;
}
