import { ArrowRightOutlined, ReloadOutlined } from '@ant-design/icons';
import { useEffect, useState } from 'react';
import { api } from './api';
import type { DoctorCheck, StatsView } from './api';

type Result<T> = { data: T; error?: never } | { error: string; data?: never };

const numbers = new Intl.NumberFormat('zh-CN');
const currency = new Intl.NumberFormat('zh-CN', { style: 'currency', currency: 'USD', maximumFractionDigits: 4 });
const timestamps = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'short', timeStyle: 'short' });
const checkLabels: Record<string, string> = {
  config: '配置', mcp: 'MCP 服务', database: '数据库', workspace: '工作区', project_rules: '项目规则',
  policy: '策略', git: 'Git', shadow_repository: '快照仓库', worktrees: '隔离工作区', provider: '模型服务',
  failed_runs: '失败运行', overall: '总览',
};
const checkStates: Record<string, { label: string; order: number }> = {
  fail: { label: '失败', order: 0 }, warn: { label: '警告', order: 1 }, ok: { label: '通过', order: 2 },
};
const failureLabels: Record<string, string> = {
  'run.failed': '运行失败', 'hook.warning': 'Hook 警告', 'invocation.FAILED': '工具失败',
};

function dateLabel(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? '时间未知' : timestamps.format(date);
}

function StatTile({ label, value, note, tone }: { label: string; value: string | number; note: string; tone?: string }) {
  return (
    <article className={`stat-tile${tone ? ` ${tone}` : ''}`}>
      <span>{label}</span>
      <strong>{typeof value === 'number' ? numbers.format(value) : value}</strong>
      <small>{note}</small>
    </article>
  );
}

function Summary({ stats }: { stats: StatsView }) {
  const { budget, hooks, policy, usage } = stats;
  const estimate = usage.cost;
  const estimated = typeof estimate === 'number' && Number.isFinite(estimate);
  const cost = estimated ? currency.format(estimate) : (stats.model_calls ? '未配置价格' : '暂无用量');
  const decisions = [
    { kind: 'allow', label: '允许', count: policy.allow },
    { kind: 'ask', label: '询问', count: policy.ask },
    { kind: 'deny', label: '拒绝', count: policy.deny },
  ];

  return (
    <>
      <section className="stat-grid" aria-label="运行统计">
        <StatTile label="会话" value={stats.sessions} note={`${numbers.format(stats.runs)} 次运行 · ${numbers.format(stats.events)} 个持久事件`} />
        <StatTile label="活跃运行" value={stats.active_runs} note={`${numbers.format(stats.failed_runs)} 次失败运行`} tone={stats.active_runs ? 'tile-live' : undefined} />
        <StatTile label="模型调用 · 近 30 天" value={stats.model_calls} note={`${numbers.format(usage.input_tokens)} 输入 · ${numbers.format(usage.output_tokens)} 输出 tokens`} />
        <StatTile label="估算费用 · 近 30 天" value={cost} note={`${(usage.cache_hit_rate * 100).toFixed(1)}% 缓存命中`} tone={estimated ? undefined : 'tile-unavailable'} />
      </section>

      <section className="observability-strip" aria-label="预算、策略和 Hook">
        <div className="strip-section">
          <div className="strip-title"><strong>预算用量</strong><span>根运行合计</span></div>
          <div className="strip-values">
            <span>模型 <strong>{numbers.format(budget.model_used)}</strong> / {numbers.format(budget.model_max)}</span>
            <span>工具 <strong>{numbers.format(budget.tool_used)}</strong> / {numbers.format(budget.tool_max)}</span>
          </div>
        </div>
        <div className="strip-section">
          <div className="strip-title"><strong>策略决策</strong><span>{numbers.format(policy.allow + policy.ask + policy.deny)} 次</span></div>
          <div className="policy-track" aria-hidden="true">
            {decisions.filter(({ count }) => count > 0).map(({ kind, count }) => <span className={`policy-${kind}`} style={{ flexGrow: count }} key={kind} />)}
          </div>
          <div className="policy-counts">
            {decisions.map(({ kind, label, count }) => <span className={`policy-count policy-${kind}`} key={kind}>{label} <b>{numbers.format(count)}</b></span>)}
          </div>
        </div>
        <div className="strip-section">
          <div className="strip-title"><strong>Hook 执行</strong><span>{numbers.format(hooks.total)} 次</span></div>
          <div className="strip-values">
            <span>警告 <strong>{numbers.format(hooks.warnings)}</strong></span>
            <span>拦截 <strong>{numbers.format(hooks.blocked)}</strong></span>
          </div>
        </div>
      </section>
    </>
  );
}

function DoctorRow({ check, onOpenRun }: { check: DoctorCheck; onOpenRun: (runId: string) => void }) {
  const state = checkStates[check.status];
  const runId = check.run_id?.trim();
  return (
    <div className={`doctor-row${state ? ` doctor-${check.status}` : ''}`}>
      <span className={`check-state${state ? ` check-${check.status}` : ''}`}>{state?.label ?? '未知'}</span>
      <div className="doctor-copy">
        <strong>{checkLabels[check.name] || check.name.replaceAll('_', ' ').trim() || '检查项'}</strong>
        <span>{check.detail.trim() || '无详细信息'}</span>
      </div>
      {runId && <button className="link-button" type="button" onClick={() => onOpenRun(runId)}>查看运行 <ArrowRightOutlined aria-hidden="true" /></button>}
    </div>
  );
}

export function Observability({ onOpenRun }: { onOpenRun: (runId: string) => void }) {
  const [stats, setStats] = useState<Result<StatsView>>();
  const [doctor, setDoctor] = useState<Result<DoctorCheck[]>>();
  const [loading, setLoading] = useState(true);
  const [revision, setRevision] = useState(0);

  function refresh() {
    setLoading(true);
    setRevision((previous) => previous + 1);
  }

  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;

    async function load<T>(request: Promise<T>, publish: (result: Result<T>) => void) {
      try {
        const data = await request;
        if (!signal.aborted) publish({ data });
      } catch (error) {
        if (!signal.aborted) publish({ error: error instanceof Error && error.message ? error.message : '连接 serve 失败，请重试。' });
      }
    }

    void Promise.all([load(api.getStats(signal), setStats), load(api.getDoctor(signal), setDoctor)]).then(() => {
      if (!signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [revision]);

  const overall = doctor?.data?.find((check) => check.name === 'overall');
  const checks = doctor?.data?.filter((check) => check.name !== 'overall').sort((a, b) => (checkStates[a.status]?.order ?? 3) - (checkStates[b.status]?.order ?? 3));
  const failures = stats?.data?.recent_errors;

  return (
    <div className="page observability-page">
      <div className="page-heading observability-heading">
        <h2>运行状况</h2>
        <button type="button" className="button secondary" onClick={refresh} disabled={loading}>
          <ReloadOutlined spin={loading} aria-hidden="true" /> {loading ? '正在刷新…' : '刷新数据'}
        </button>
      </div>

      {!stats ? <p className="observability-loading" role="status">正在读取统计数据…</p> : stats.error ? (
        <div className="observability-error" role="alert">
          <strong>统计数据加载失败</strong><span>{stats.error}</span>
          <button type="button" className="button secondary small" onClick={refresh} disabled={loading}>重试</button>
        </div>
      ) : stats.data && <Summary stats={stats.data} />}

      <div className="observation-columns">
        <section className="observability-panel doctor-panel" aria-labelledby="doctor-heading">
          <div className="observability-panel-heading">
            <h2 id="doctor-heading">系统检查</h2>
            {checks && <span className="count-pill">{checks.length}</span>}
          </div>
          {!doctor ? <p className="panel-message" role="status">正在运行诊断…</p> : doctor.error ? (
            <div className="panel-message error-message" role="alert">
              <strong>检查结果加载失败</strong><span>{doctor.error}</span>
              <button type="button" className="button secondary small" onClick={refresh} disabled={loading}>重试</button>
            </div>
          ) : doctor.data?.length ? (
            <>
              {overall && <div className="doctor-overall"><DoctorRow check={overall} onOpenRun={onOpenRun} /></div>}
              <ul className="doctor-list">
                {checks?.map((check) => <li key={check.name}><DoctorRow check={check} onOpenRun={onOpenRun} /></li>)}
              </ul>
            </>
          ) : <p className="panel-message">暂无诊断检查项。</p>}
        </section>

        <section className="observability-panel errors-panel" aria-labelledby="failures-heading">
          <div className="observability-panel-heading">
            <div><h2 id="failures-heading">最近错误</h2><p>失败运行与执行异常</p></div>
            {failures && <span className="count-pill">{failures.length}</span>}
          </div>
          {!stats ? <p className="panel-message" role="status">正在读取运行记录…</p> : stats.error ? (
            <p className="panel-message">统计数据不可用。</p>
          ) : failures?.length ? (
            <ul className="failure-list">
              {failures.map((failure, index) => (
                <li className="failure-row" key={`${failure.run_id}-${failure.type}-${failure.at}-${index}`}>
                  <span className={`failure-marker${failure.type === 'hook.warning' ? ' warning-marker' : ''}`} aria-hidden="true" />
                  <div className="failure-copy">
                    <div className="failure-title">
                      <strong>{failureLabels[failure.type] || failure.type || '执行异常'}</strong>
                      <time dateTime={failure.at}>{dateLabel(failure.at)}</time>
                    </div>
                    <p>{failure.message.trim() || '暂无错误详情'}</p>
                    {failure.run_id?.trim() && <>
                      <code title={failure.run_id}>{failure.run_id}</code>
                      <button className="link-button" type="button" onClick={() => onOpenRun(failure.run_id)}>打开运行 <ArrowRightOutlined aria-hidden="true" /></button>
                    </>}
                  </div>
                </li>
              ))}
            </ul>
          ) : <div className="panel-message clean-message"><span aria-hidden="true">✓</span><strong>暂无运行错误</strong></div>}
        </section>
      </div>
    </div>
  );
}
