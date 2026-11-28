import { useEffect, useId, useRef, useState } from 'react';
import type { KeyboardEvent, RefObject } from 'react';
import { api } from './api';
import { skillQuery, type SkillSummary } from './skills';

interface Props {
  inputRef: RefObject<HTMLTextAreaElement | null>;
  value: string;
  placeholder: string;
  enabled?: boolean;
  onChange: (value: string) => void;
  onKeyDown: (event: KeyboardEvent<HTMLTextAreaElement>) => void;
}

export function SkillInput({ inputRef, value, placeholder, enabled = true, onChange, onKeyDown }: Props) {
  const listId = useId();
  const [focused, setFocused] = useState(false);
  const [dismissed, setDismissed] = useState(false);
  const [catalog, setCatalog] = useState<SkillSummary[]>([]);
  const [status, setStatus] = useState<'loading' | 'ready' | 'error'>('loading');
  const [active, setActive] = useState(0);
  const selectedRow = useRef<HTMLButtonElement>(null);
  const query = skillQuery(value);
  const open = enabled && focused && !dismissed && query !== null;
  const matches = open && status === 'ready' ? catalog.filter(skill => skill.name.toLowerCase().includes(query)) : [];
  const current = matches[active];

  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    api.listSkills(controller.signal).then(skills => {
      if (controller.signal.aborted) return;
      if (!Array.isArray(skills) || !skills.every(skill => typeof skill?.name === 'string' && typeof skill.description === 'string')) {
        setStatus('error');
        return;
      }
      setCatalog(skills);
      setStatus('ready');
    }).catch(() => {
      if (!controller.signal.aborted) setStatus('error');
    });
    return () => controller.abort();
  }, [open]);

  useEffect(() => {
    selectedRow.current?.scrollIntoView?.({ block: 'nearest' });
  }, [current?.name]);

  const select = (skill: SkillSummary) => {
    onChange(`/skill ${skill.name} `);
    inputRef.current?.focus();
  };
  const handleKey = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.nativeEvent.isComposing || event.nativeEvent.keyCode === 229) return;
    if (event.key === 'Enter' && event.repeat && !event.shiftKey) {
      event.preventDefault();
      return;
    }
    if (open && !event.shiftKey && !event.altKey && !event.ctrlKey && !event.metaKey) {
      if (event.key === 'Escape') {
        event.preventDefault();
        setDismissed(true);
        return;
      }
      if (current && (event.key === 'ArrowDown' || event.key === 'ArrowUp')) {
        event.preventDefault();
        const step = event.key === 'ArrowDown' ? 1 : -1;
        setActive((active + step + matches.length) % matches.length);
        return;
      }
      if (current && (event.key === 'Enter' || event.key === 'Tab')) {
        event.preventDefault();
        select(current);
        return;
      }
      if (status === 'loading' && event.key === 'Enter') {
        event.preventDefault();
        return;
      }
    }
    onKeyDown(event);
  };

  return <>
    <textarea ref={inputRef} rows={1} value={value} placeholder={placeholder} aria-label={placeholder}
      aria-autocomplete={enabled ? 'list' : undefined} aria-controls={open ? listId : undefined}
      aria-activedescendant={current ? `${listId}-${active}` : undefined}
      onFocus={() => { setFocused(true); setDismissed(false); setActive(0); setStatus('loading'); }}
      onBlur={() => setFocused(false)} onKeyDown={handleKey}
      onChange={event => {
        if (!open) setStatus('loading');
        setActive(0);
        setDismissed(false);
        onChange(event.target.value);
      }} />
    {open && <div className="skill-popover">
      <div id={listId} role="listbox" aria-label="选择技能" aria-busy={status === 'loading'}>
        {matches.map((skill, index) => <button type="button" key={skill.name} id={`${listId}-${index}`}
          role="option" aria-selected={index === active} ref={index === active ? selectedRow : undefined}
          tabIndex={-1} onMouseDown={event => event.preventDefault()} onClick={() => select(skill)}>
          <strong>{skill.name}</strong><span>{skill.description}</span>
        </button>)}
      </div>
      {!matches.length && <p role="status">{status === 'loading' ? '正在加载技能…' : status === 'error' ? '技能列表暂不可用，重新输入 / 可重试。' : catalog.length ? '没有匹配的技能' : '暂无技能'}</p>}
    </div>}
  </>;
}
