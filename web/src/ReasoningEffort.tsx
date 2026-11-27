import { useEffect, useRef, useState } from 'react';
import type { KeyboardEvent, PointerEvent } from 'react';
import type { ModelOption, ReasoningEffort } from './api';

const efforts = [
  { id: 'default', label: '默认' },
  { id: 'off', label: '关' },
  { id: 'low', label: '低' },
  { id: 'medium', label: '中' },
  { id: 'high', label: '高' },
] as const;

export function ReasoningEffortSlider({ value, onChange, disabled, model }: { model?: ModelOption; value?: ReasoningEffort; onChange: (value: ReasoningEffort) => void; disabled?: boolean }) {
  const slider = useRef<HTMLDivElement>(null);
  const track = useRef<HTMLDivElement>(null);
  const drag = useRef<number | null>(null);
  const [preview, setPreview] = useState<{ context: string; value: ReasoningEffort } | null>(null);
  const unsupported = model?.thinking === 'unsupported';
  const unavailable = Boolean(disabled || unsupported);
  const known = model?.thinking === 'supported';
  const toggle = known && model?.reasoning_type === 'toggle';
  const values = model?.reasoning_values || [];
  const available = (effort: ReasoningEffort) => {
    if (effort === 'default' || !known || model?.reasoning_type !== 'effort') return true;
    if (effort === 'off') return values.includes('none') || values.includes('off');
    if (effort === 'low') return values.includes('low') || values.includes('minimal');
    return values.includes(effort);
  };
  const options: { id: ReasoningEffort; label: string }[] = toggle ? [{ id: 'off', label: '关' }, { id: 'default', label: '开' }] : [...efforts];
  const selected = toggle ? (value === 'off' ? 'off' : 'default') : value;
  const current = options.find((effort) => effort.id === selected && available(effort.id)) || options.find((effort) => effort.id === 'default')!;
  const context = JSON.stringify([unavailable, model?.id, model?.thinking, model?.reasoning_type, values, value]);
  if (preview && preview.context !== context) setPreview(null);
  const shown = options.find((effort) => effort.id === preview?.value && preview.context === context && available(effort.id)) || current;
  const index = options.indexOf(shown);
  const position = index / (options.length - 1) * 100 + '%';
  useEffect(() => { drag.current = null; }, [context]);
  const commit = (effort: ReasoningEffort) => { if (!unavailable && effort !== current.id) onChange(effort); };
  const nearest = (clientX: number) => {
    const bounds = track.current!.getBoundingClientRect();
    const raw = Math.max(0, Math.min(1, bounds.width > 0 ? (clientX - bounds.left) / bounds.width : 0)) * (options.length - 1);
    // Ties choose the lower available notch, for both clicks and drags.
    return options.reduce((best, option, i) => available(option.id) && Math.abs(i - raw) < Math.abs(options.indexOf(best) - raw) ? option : best, options.find((option) => available(option.id))!);
  };
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (unavailable || !['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End'].includes(event.key)) return;
    event.preventDefault();
    const enabled = options.filter((option) => available(option.id));
    const at = enabled.indexOf(current);
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? enabled.length - 1 : at + (event.key === 'ArrowLeft' || event.key === 'ArrowDown' ? -1 : 1);
    commit(enabled[Math.max(0, Math.min(enabled.length - 1, next))].id);
  };
  const begin = (event: PointerEvent<HTMLDivElement>) => {
    if (unavailable || event.button !== 0 || event.isPrimary === false) return;
    event.preventDefault(); slider.current?.focus();
    const next = nearest(event.clientX);
    drag.current = event.pointerId;
    setPreview({ context, value: next.id });
    event.currentTarget.setPointerCapture(event.pointerId);
  };
  const move = (event: PointerEvent<HTMLDivElement>) => {
    if (unavailable || drag.current !== event.pointerId) return;
    setPreview({ context, value: nearest(event.clientX).id });
  };
  const end = (event: PointerEvent<HTMLDivElement>) => {
    if (drag.current !== event.pointerId) return;
    const next = nearest(event.clientX);
    drag.current = null; setPreview(null);
    event.currentTarget.releasePointerCapture(event.pointerId);
    commit(next.id);
  };
  const cancel = (event: PointerEvent<HTMLDivElement>) => {
    if (drag.current !== event.pointerId) return;
    drag.current = null; setPreview(null);
  };
  return <div ref={slider} className="value-chip reasoning-effort" role="slider" aria-label="思考强度" aria-orientation="horizontal" aria-valuemin={0} aria-valuemax={options.length - 1} aria-valuenow={index} aria-valuetext={unsupported ? '不支持' : shown.label} aria-disabled={unavailable} title="下一轮生效" tabIndex={unavailable ? -1 : 0} data-dragging={preview !== null} onKeyDown={onKeyDown}>
    <div ref={track} className="reasoning-track" aria-hidden="true" onPointerDown={begin} onPointerMove={move} onPointerUp={end} onPointerCancel={cancel} onLostPointerCapture={cancel}>
      <span className="reasoning-rail" /><span className="reasoning-fill" style={{ width: position }} />
      {options.map((option, i) => <span key={option.id} className="reasoning-notch" data-effort={option.id} data-available={available(option.id)} data-current={shown.id === option.id} style={{ left: i / (options.length - 1) * 100 + '%' }}><span className="reasoning-dot" /><span className="reasoning-notch-label">{option.label}</span></span>)}
      <span className="reasoning-thumb" style={{ left: position }} />
    </div>
    <span className="reasoning-current" aria-hidden="true">{unsupported ? '不支持' : shown.label}</span>
  </div>;
}
