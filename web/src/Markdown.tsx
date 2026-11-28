import { isValidElement, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Icon } from './Icon';

function plainText(node: ReactNode): string {
  if (typeof node === 'string' || typeof node === 'number') return String(node);
  if (Array.isArray(node)) return node.map(plainText).join('');
  if (isValidElement<{ children?: ReactNode }>(node)) return plainText(node.props.children);
  return '';
}

interface CopyChoice { label: string; text: string }

function CopyAction({ label, choices }: { label: string; choices: () => CopyChoice[] }) {
  const [options, setOptions] = useState<CopyChoice[]>([]);
  const [feedback, setFeedback] = useState('');
  const trigger = useRef<HTMLButtonElement>(null);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);

  const copy = async (value: string) => {
    trigger.current?.focus();
    setOptions([]);
    try {
      await navigator.clipboard.writeText(value);
      setFeedback('已复制');
    } catch {
      setFeedback('复制失败，请手动选择文本');
    }
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setFeedback(''), 2200);
  };

  return <div className="copy-action" onBlur={event => {
    if (!event.currentTarget.contains(event.relatedTarget)) setOptions([]);
  }} onKeyDown={event => {
    if (options.length && event.key === 'Escape') {
      event.stopPropagation();
      setOptions([]);
      trigger.current?.focus();
    }
  }}>
    <button type="button" className="icon-button" ref={trigger} title={feedback || label} aria-label={label}
      aria-expanded={options.length ? true : undefined} onClick={() => {
        if (options.length) { setOptions([]); return; }
        const targets = choices();
        if (targets.length === 1) void copy(targets[0].text);
        else {
          trigger.current?.focus();
          setFeedback('');
          setOptions(targets);
        }
      }}><Icon name={feedback === '已复制' ? 'check' : 'copy'} size={14} /></button>
    <span className="copy-feedback" role="status">{feedback}</span>
    {options.length > 0 && <div className="copy-options" role="group" aria-label="选择复制内容">
      {options.map(option => <button type="button" key={option.label} onClick={() => void copy(option.text)}>{option.label}</button>)}
    </div>}
  </div>;
}

function CodeBlock({ children, copy = true }: { children?: ReactNode; copy?: boolean }) {
  const child = Array.isArray(children) ? children[0] : children;
  const className = isValidElement<{ className?: string }>(child) ? child.props.className || '' : '';
  const language = /language-([\w+-]+)/.exec(className)?.[1] || '';
  return (
    <div className="code-block">
      {(language || copy) && <div className="code-block-bar"><span>{language}</span>{copy && <CopyAction label="复制代码" choices={() => [{ label: '代码', text: plainText(children).replace(/\n$/, '') }]} />}</div>}
      <pre>{children}</pre>
    </div>
  );
}

function ReplyCodeBlock({ children }: { children?: ReactNode }) {
  return <CodeBlock copy={false}>{children}</CodeBlock>;
}

// Model output is rendered as Markdown. Raw HTML in it stays text.
export function Markdown({ text, copyReply = false }: { text: string; copyReply?: boolean }) {
  const body = useRef<HTMLDivElement>(null);
  // Read exactly the blocks rendered by the Markdown parser, including indented code.
  const choices = () => {
    const blocks = Array.from(body.current!.querySelectorAll('.code-block pre > code'), (code, i) => ({
      label: [`代码 ${i + 1}`, code.className.replace(/^language-/, '')].filter(Boolean).join(' · '),
      text: (code.textContent || '').replace(/\n$/, ''),
    }));
    if (blocks.length === 1 && body.current!.querySelector(':scope > .code-block:only-child')) return blocks;
    return [{ label: '完整回复', text }, ...blocks];
  };
  return (
    <><div className="markdown" ref={body}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          pre: copyReply ? ReplyCodeBlock : CodeBlock,
          a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer noopener">{children}</a>,
          table: ({ children }) => <div className="table-scroll"><table>{children}</table></div>,
        }}
      >
        {text}
      </ReactMarkdown>
    </div>{copyReply && <CopyAction label="复制回复" choices={choices} />}</>
  );
}
