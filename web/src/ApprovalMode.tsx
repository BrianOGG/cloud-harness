import { useEffect, useId, useRef, useState } from 'react';
import { Icon } from './Icon';
import { approvalModeLabel, approvalModes } from './approval';

const modeIcons: Record<string, string> = {
  ask: 'question',
  safe: 'eye',
  'workspace-write': 'folder-edit',
  'full-auto': 'bolt',
};

export function ApprovalModeMenu({ value, onChange, disabled }: { value: string; onChange: (value: string) => void; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const dialog = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  const descriptionId = useId();
  useEffect(() => {
    if (!confirming) return;
    const modal = dialog.current!;
    const trigger = button.current;
    modal.showModal();
    return () => { modal.close(); trigger?.focus(); };
  }, [confirming]);
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent) => { if (!ref.current?.contains(event.target as Node)) setOpen(false); };
    const escape = (event: globalThis.KeyboardEvent) => { if (event.key === 'Escape') { setOpen(false); button.current?.focus(); } };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('keydown', escape);
    return () => { document.removeEventListener('pointerdown', outside); document.removeEventListener('keydown', escape); };
  }, [open]);
  return <div className="approval-mode" ref={ref}>
    <button ref={button} type="button" className="approval-mode-trigger" data-mode={value} aria-label="审批模式" aria-expanded={open} disabled={disabled} title={`${approvalModeLabel(value)} · 下一轮生效`} onClick={() => setOpen(!open)}>
      <Icon name={modeIcons[value] || 'question'} size={16} /><span>{approvalModeLabel(value)}</span><Icon name="down" size={11} />
    </button>
    {open && <div className="approval-mode-popover" role="group" aria-label="选择下一轮审批模式">
      {approvalModes.map((mode) => <button type="button" key={mode.id} className="mode-option" data-mode={mode.id} aria-pressed={value === mode.id} disabled={disabled} onClick={() => {
        setOpen(false);
        if (mode.id === value) { button.current?.focus(); return; }
        if (mode.id === 'full-auto') { setAcknowledged(false); setConfirming(true); return; }
        onChange(mode.id); button.current?.focus();
      }}><Icon name={modeIcons[mode.id]} /><strong>{mode.label}</strong>{value === mode.id && <Icon name="check" size={13} />}</button>)}
    </div>}
    {confirming && <dialog ref={dialog} className="full-auto-confirmation" aria-modal="true" aria-labelledby={titleId} aria-describedby={descriptionId} onCancel={() => setConfirming(false)} onClose={() => setConfirming(false)}>
      <h2 id={titleId}>确认启用完全自主？</h2>
      <p id={descriptionId}>启用后，下一轮将自动执行命令（可读写工作区外文件）和工作区内文件写入，不再逐次询问；误操作或提示注入可能导致修改文件、执行命令或对外发送数据；既有拒绝规则仍生效，但这不是安全保障。仅在你信任当前任务和环境时使用。</p>
      <label><input type="checkbox" checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} />我已了解风险，并愿意继续</label>
      <div className="full-auto-actions">
        <button type="button" className="button" autoFocus onClick={() => setConfirming(false)}>取消</button>
        <button type="button" className="button primary" disabled={disabled || !acknowledged} onClick={() => {
          if (!acknowledged || disabled) return;
          onChange('full-auto'); setConfirming(false);
        }}>启用完全自主</button>
      </div>
    </dialog>}
  </div>;
}
