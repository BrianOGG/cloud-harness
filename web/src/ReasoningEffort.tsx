import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { KeyboardEvent, PointerEvent } from 'react';
import { createPortal } from 'react-dom';
import type { ModelOption, ReasoningEffort } from './api';
import { Icon } from './Icon';
import { ModelPickerList } from './ModelPicker';

const standardEfforts = [
  { id: 'default', label: '默认' },
  { id: 'off', label: '关' },
  { id: 'low', label: '低' },
  { id: 'medium', label: '中' },
  { id: 'high', label: '高' },
] as const;
type EffortOption = { id: ReasoningEffort; label: string };

function getEffortState(model: ModelOption | undefined, value: ReasoningEffort | undefined) {
  const unsupported = model?.thinking === 'unsupported';
  const known = model?.thinking === 'supported';
  const toggle = known && model?.reasoning_type === 'toggle';
  const values = model?.reasoning_values || [];
  const options: EffortOption[] = toggle ? [{ id: 'off', label: '关' }, { id: 'default', label: '开' }] : [...standardEfforts];
  const available = (effort: ReasoningEffort) => {
    if (effort === 'default' || !known || model?.reasoning_type !== 'effort') return true;
    if (effort === 'off') return values.includes('none') || values.includes('off');
    if (effort === 'low') return values.includes('low') || values.includes('minimal');
    return values.includes(effort);
  };
  const selected = toggle ? (value === 'off' ? 'off' : 'default') : value;
  const current = options.find((effort) => effort.id === selected && available(effort.id)) || options.find((effort) => effort.id === 'default')!;
  return { unsupported, options, available, current };
}

function stopCenter(index: number, count: number, width: number) {
  const last = Math.max(14, width - 14);
  if (index === 0) return 14;
  if (index === count - 1) return last;
  if (count === 5) return width * [0, 0.27, 0.5, 0.73, 1][index];
  return 14 + (last - 14) * index / (count - 1);
}

export function ReasoningEffortSlider({ value, onChange, disabled, model }: { model?: ModelOption; value?: ReasoningEffort; onChange: (value: ReasoningEffort) => void; disabled?: boolean }) {
  const track = useRef<HTMLDivElement>(null);
  const drag = useRef<number | null>(null);
  const [trackWidth, setTrackWidth] = useState(226);
  const [preview, setPreview] = useState<{ context: string; value: ReasoningEffort; center: number; trackWidth: number; dragging: boolean } | null>(null);
  const { unsupported, options, available, current } = getEffortState(model, value);
  const unavailable = Boolean(disabled || unsupported);
  const context = JSON.stringify([unavailable, model?.id, model?.thinking, model?.reasoning_type, model?.reasoning_values, value]);
  const draft = preview?.context === context ? preview : null;
  const shown = options.find((effort) => effort.id === draft?.value && available(effort.id)) || current;
  const index = options.indexOf(shown);
  const width = trackWidth;
  const center = draft?.dragging ? draft.center : stopCenter(index, options.length, width);
  const fillWidth = Math.min(draft?.trackWidth || width, center + 14);

  const attachTrack = useCallback((node: HTMLDivElement | null) => {
    track.current = node;
    if (node) {
      const measured = node.getBoundingClientRect().width || 226;
      setTrackWidth((currentWidth) => currentWidth === measured ? currentWidth : measured);
    }
  }, []);

  useEffect(() => {
    const node = track.current;
    if (!node || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      const measured = node.getBoundingClientRect().width || 226;
      setTrackWidth((currentWidth) => currentWidth === measured ? currentWidth : measured);
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    drag.current = null;
  }, [context]);

  const commit = (effort: ReasoningEffort) => { if (!unavailable && effort !== current.id) onChange(effort); };
  const nearest = (clientX: number) => {
    const bounds = track.current!.getBoundingClientRect();
    const trackWidth = bounds.width || 226;
    const x = Math.max(14, Math.min(trackWidth - 14, clientX - bounds.left));
    return options.reduce((best, option, i) => available(option.id) && Math.abs(stopCenter(i, options.length, trackWidth) - x) < Math.abs(stopCenter(options.indexOf(best), options.length, trackWidth) - x) ? option : best, options.find((option) => available(option.id))!);
  };
  const previewAt = (clientX: number, dragging: boolean) => {
    const bounds = track.current!.getBoundingClientRect();
    const trackWidth = bounds.width || 226;
    const x = Math.max(14, Math.min(trackWidth - 14, clientX - bounds.left));
    const next = options.reduce((best, option, i) => available(option.id) && Math.abs(stopCenter(i, options.length, trackWidth) - x) < Math.abs(stopCenter(options.indexOf(best), options.length, trackWidth) - x) ? option : best, options.find((option) => available(option.id))!);
    setPreview({ context, value: next.id, center: x, trackWidth, dragging });
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
    event.preventDefault(); track.current?.focus();
    drag.current = event.pointerId;
    previewAt(event.clientX, true);
    event.currentTarget.setPointerCapture(event.pointerId);
  };
  const move = (event: PointerEvent<HTMLDivElement>) => {
    if (unavailable || drag.current !== event.pointerId) return;
    previewAt(event.clientX, true);
  };
  const end = (event: PointerEvent<HTMLDivElement>) => {
    if (drag.current !== event.pointerId) return;
    const next = nearest(event.clientX);
    drag.current = null;
    setPreview(null);
    event.currentTarget.releasePointerCapture(event.pointerId);
    commit(next.id);
  };
  const cancel = (event: PointerEvent<HTMLDivElement>) => {
    if (drag.current !== event.pointerId) return;
    drag.current = null;
    setPreview(null);
  };
  const displayLabel = unsupported ? '不支持' : shown.label;

  return <div className="reasoning-slider-wrap">
    <div className="reasoning-title" data-unsupported={unsupported} aria-live="polite">{displayLabel}</div>
    <div ref={attachTrack} className="reasoning-track" role="slider" aria-label="思考强度" aria-orientation="horizontal" aria-valuemin={0} aria-valuemax={options.length - 1} aria-valuenow={index} aria-valuetext={displayLabel} aria-disabled={unavailable} title="下一轮生效" tabIndex={unavailable ? -1 : 0} data-dragging={draft?.dragging === true} onKeyDown={onKeyDown} onPointerDown={begin} onPointerMove={move} onPointerUp={end} onPointerCancel={cancel} onLostPointerCapture={cancel}>
      <span className="reasoning-fill" aria-hidden="true" style={{ width: fillWidth, transition: draft?.dragging ? 'none' : undefined }} />
      {options.map((option, i) => <span key={option.id} className="reasoning-notch" data-effort={option.id} data-available={available(option.id)} data-current={shown.id === option.id} style={{ left: stopCenter(i, options.length, width) }}><span className="reasoning-dot" /></span>)}
      <span className="reasoning-thumb" aria-hidden="true" style={{ left: center, transition: draft?.dragging ? 'none' : undefined }} />
    </div>
  </div>;
}

export function ReasoningEffortPicker({ models, selectedModel, modelsLoading, modelSelectionDisabled, value, onModelChange, onChange, disabled }: {
  models: ModelOption[];
  selectedModel?: ModelOption;
  modelsLoading?: boolean;
  modelSelectionDisabled?: boolean;
  value?: ReasoningEffort;
  onModelChange: (id: string) => void;
  onChange: (value: ReasoningEffort) => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [view, setView] = useState<'slider' | 'models'>('slider');
  const [position, setPosition] = useState({ top: 12, left: 12 });
  const trigger = useRef<HTMLButtonElement>(null);
  const popover = useRef<HTMLDivElement>(null);
  const modelLink = useRef<HTMLButtonElement>(null);
  const returnFocus = useRef(false);
  const { current, unsupported } = getEffortState(selectedModel, value);
  const triggerDisabled = Boolean(modelSelectionDisabled || (disabled && models.length === 0));
  const modelName = selectedModel?.name || (modelsLoading ? '加载中…' : '模型不可用');
  const label = selectedModel && !unsupported ? modelName + ' · ' + current.label : modelName;

  const placePopover = () => {
    const anchor = trigger.current?.getBoundingClientRect();
    const panel = popover.current?.getBoundingClientRect();
    if (!anchor || !panel) return;
    const viewportWidth = document.documentElement.clientWidth || window.innerWidth;
    const viewportHeight = document.documentElement.clientHeight || window.innerHeight;
    const width = Math.min(258, Math.max(0, viewportWidth - 24));
    const left = Math.max(12, Math.min(viewportWidth - width - 12, anchor.right - width));
    const below = anchor.bottom + 8;
    const top = below + panel.height <= viewportHeight - 12 ? below : Math.max(12, anchor.top - panel.height - 8);
    setPosition({ top, left });
  };
  useLayoutEffect(() => { if (open) placePopover(); }, [open, view, models.length, modelsLoading]);
  useLayoutEffect(() => {
    if (view === 'slider' && returnFocus.current) {
      returnFocus.current = false;
      modelLink.current?.focus();
    }
  }, [view]);
  useEffect(() => {
    if (!open) return;
    const outside = (event: globalThis.PointerEvent) => {
      const target = event.target as Node;
      if (!popover.current?.contains(target) && !trigger.current?.contains(target)) setOpen(false);
    };
    const escape = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape') { event.preventDefault(); setOpen(false); trigger.current?.focus(); }
    };
    const reposition = () => placePopover();
    document.addEventListener('pointerdown', outside);
    document.addEventListener('keydown', escape);
    window.addEventListener('resize', reposition);
    window.addEventListener('scroll', reposition, true);
    return () => {
      document.removeEventListener('pointerdown', outside);
      document.removeEventListener('keydown', escape);
      window.removeEventListener('resize', reposition);
      window.removeEventListener('scroll', reposition, true);
    };
  }, [open]);

  return <div className="reasoning-picker">
    <button ref={trigger} type="button" className="reasoning-trigger" data-model-id={selectedModel?.id} data-model-name={selectedModel?.name} aria-label="选择思考强度" aria-haspopup="dialog" aria-expanded={open} disabled={triggerDisabled} onClick={() => {
      if (open) setOpen(false);
      else { setView('slider'); setOpen(true); }
    }} title={label}><span>{label}</span><Icon name="down" size={12} /></button>
    {open && createPortal(<div ref={popover} className="reasoning-popover" role="dialog" aria-label={view === 'slider' ? '思考强度' : '选择模型'} style={{ top: position.top, left: position.left }}>
      {view === 'slider' ? <>
        <button type="button" className="reasoning-reset" aria-label="重置思考强度" title="重置为默认" onClick={() => onChange('default')}><Icon name="refresh" size={15} /></button>
        <ReasoningEffortSlider model={selectedModel} value={value} disabled={disabled} onChange={onChange} />
        <button ref={modelLink} type="button" className="reasoning-model-link" disabled={modelSelectionDisabled || models.length === 0} onClick={() => setView('models')}><span>{selectedModel?.name || (modelsLoading ? '加载中…' : '未配置模型')}</span><Icon name="right" size={13} /></button>
      </> : <div className="reasoning-model-view">
        <div className="reasoning-model-heading"><button type="button" onClick={() => { returnFocus.current = true; setView('slider'); }}>‹ 返回</button><strong>选择模型</strong></div>
        {modelsLoading && models.length === 0 ? <p className="reasoning-model-status" role="status">加载中…</p> : models.length === 0 ? <p className="reasoning-model-status" role="status">没有已启用的模型</p> : <ModelPickerList models={models} selectedModel={selectedModel} ariaLabel="选择模型" disabled={modelSelectionDisabled} onChange={(id) => {
          if (id !== selectedModel?.id) onModelChange(id);
          returnFocus.current = true;
          setView('slider');
        }} />}
      </div>}
    </div>, document.body)}
  </div>;
}
