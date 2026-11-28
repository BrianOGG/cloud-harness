import { useEffect, useState } from 'react';
import { runChanges } from './runChangesApi';
import type { FilePatch, ReviewedPath, RunChangeList } from './runChangesApi';

const statuses: Record<string, string> = { A: '新增', D: '删除', M: '修改', T: '类型' };
const errorText = (err: unknown) => err instanceof Error ? err.message : '请求失败，请刷新重试';

export function RunChanges({ runId }: { runId: string }) {
  const [revision, setRevision] = useState(0);
  const [list, setList] = useState<RunChangeList>();
  const [file, setFile] = useState<ReviewedPath>();
  const [patch, setPatch] = useState<FilePatch>();
  const [error, setError] = useState('');
  const [receipt, setReceipt] = useState('');
  const [confirm, setConfirm] = useState(false);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    const ctl = new AbortController();
    runChanges.list(runId, ctl.signal).then(data => {
      if (!ctl.signal.aborted) setList(data);
    }).catch(err => {
      if (!ctl.signal.aborted) setError(errorText(err));
    });
    return () => ctl.abort();
  }, [runId, revision]);

  useEffect(() => {
    if (!file) return;
    const ctl = new AbortController();
    runChanges.diff(runId, file, ctl.signal).then(data => {
      if (!ctl.signal.aborted) setPatch(data);
    }).catch(err => {
      if (!ctl.signal.aborted) setError(errorText(err));
    });
    return () => ctl.abort();
  }, [runId, file]);

  function refresh() {
    setList(undefined);
    setFile(undefined);
    setPatch(undefined);
    setConfirm(false);
    setError('');
    setRevision(value => value + 1);
  }

  function select(next: ReviewedPath) {
    setConfirm(false);
    setError('');
    setReceipt('');
    setPatch(undefined);
    setFile(file?.after === next.after && file?.path === next.path ? undefined : next);
  }

  async function restore() {
    if (!confirm || !file || saving) return;
    setSaving(true);
    setConfirm(false);
    setError('');
    try {
      const data = await runChanges.restore(runId, file);
      setReceipt(`已还原 ${file.path}。安全快照 ${data.safety_snapshot}`);
      refresh();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  return <section className="run-changes" aria-label="文件更改">
    <header><h3>文件更改</h3><button type="button" disabled={saving} onClick={refresh}>刷新</button></header>
    {!list && !error && <p>正在读取快照…</p>}
    {list?.incomplete && <p>部分快照不完整或与其他运行重叠。</p>}
    {list && !list.batches.some(batch => batch.files.length) && <p>暂无可审查的文件更改</p>}
    {list?.batches.filter(batch => batch.files.length > 0).map(batch => <div key={batch.after} className="changes-batch">
      <p>第 {batch.number} 批 · {batch.files.length} 个文件</p>
      {batch.files.map(item => {
        const selected = file?.after === batch.after && file.path === item.path;
        return <div key={item.path} className="changes-row">
          <button type="button" className="changes-file" aria-expanded={selected} disabled={saving}
            onClick={() => select({ before: batch.before, after: batch.after, path: item.path })}>
            <span>{statuses[item.status] || item.status}</span><code>{item.path}</code>
          </button>
          {selected && <div className="changes-detail">
            {!patch && !error && <p>正在读取差异…</p>}
            {patch && <>
              <pre aria-label="文件差异" tabIndex={0}>{patch.diff}</pre>
              {patch.truncated && <p>差异仅展示前 64 KiB</p>}
              {list.terminal && (confirm ? <div className="changes-confirm">
                <p>将 {item.path} 还原到第 {batch.number} 批执行前，并保存安全快照？</p>
                <button type="button" onClick={() => setConfirm(false)}>取消</button>
                <button type="button" onClick={() => void restore()}>确认还原</button>
              </div> : <button type="button" disabled={saving} onClick={() => setConfirm(true)}>{saving ? '正在还原…' : '还原此文件'}</button>)}
            </>}
          </div>}
        </div>;
      })}
    </div>)}
    {error && <p role="alert">{error}</p>}
    {receipt && <p role="status">{receipt}</p>}
  </section>;
}
