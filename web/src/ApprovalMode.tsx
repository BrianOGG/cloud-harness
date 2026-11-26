import { useEffect, useRef, useState } from 'react';
import { Icon } from './Icon';
import { approvalModeLabel, approvalModes, commandScope } from './approval';

export function ApprovalModeMenu({ value, onChange, disabled }: { value: string; onChange: (value: string) => void; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent) => { if (!ref.current?.contains(event.target as Node)) setOpen(false); };
    const escape = (event: globalThis.KeyboardEvent) => { if (event.key === 'Escape') { setOpen(false); button.current?.focus(); } };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('keydown', escape);
    return () => { document.removeEventListener('pointerdown', outside); document.removeEventListener('keydown', escape); };
  }, [open]);
  return <div className="approval-mode" ref={ref}>
    <button ref={button} type="button" className="value-chip" aria-label="审批模式" aria-expanded={open} disabled={disabled} title="下一轮审批模式；不改变正在运行或等待的回合" onClick={() => setOpen(!open)}><Icon name="shield" size={13} /><span>下一轮 · {approvalModeLabel(value)}</span><Icon name="down" size={12} /></button>
    {open && <div className="approval-mode-popover" role="group" aria-label="选择下一轮审批模式">
      <p>仅下一轮生效，不改变正在运行或等待的回合，也不会自动重放或放行当前调用。</p>
      {approvalModes.map((mode) => <button type="button" key={mode.id} className="mode-option" aria-pressed={value === mode.id} disabled={disabled} onClick={() => { onChange(mode.id); setOpen(false); button.current?.focus(); }}><strong>{mode.label}</strong><span>{mode.description}</span></button>)}
      <p>{commandScope}</p>
    </div>}
  </div>;
}
