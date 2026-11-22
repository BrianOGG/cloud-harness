import { isValidElement, useState } from 'react';
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

export function CopyButton({ text, label = '复制' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(text).then(() => {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    });
  };
  return (
    <button type="button" className="ghost-button" onClick={copy} aria-label={label} title={label}>
      <Icon name={copied ? 'check' : 'copy'} size={14} />
      <span>{copied ? '已复制' : label}</span>
    </button>
  );
}

function CodeBlock({ children }: { children?: ReactNode }) {
  const child = Array.isArray(children) ? children[0] : children;
  const className = isValidElement<{ className?: string }>(child) ? child.props.className || '' : '';
  const language = /language-([\w+-]+)/.exec(className)?.[1] || '';
  return (
    <div className="code-block">
      <div className="code-block-bar"><span>{language || 'text'}</span><CopyButton text={plainText(children).replace(/\n$/, '')} /></div>
      <pre>{children}</pre>
    </div>
  );
}

// Model output is rendered as Markdown. Raw HTML in it stays text.
export function Markdown({ text }: { text: string }) {
  return (
    <div className="markdown">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          pre: CodeBlock,
          a: ({ href, children }) => <a href={href} target="_blank" rel="noreferrer noopener">{children}</a>,
          table: ({ children }) => <div className="table-scroll"><table>{children}</table></div>,
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
