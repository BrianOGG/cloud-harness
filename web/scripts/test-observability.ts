import assert from 'node:assert/strict';
import { api } from '../src/api';

Object.defineProperty(globalThis, 'localStorage', { value: { getItem: () => 'observability-test-token' }, configurable: true });
Object.defineProperty(globalThis, 'window', { value: globalThis, configurable: true });

const populatedStats = {
  sessions: 2,
  runs: 3,
  events: 9,
  active_runs: 1,
  failed_runs: 1,
  model_calls: 4,
  usage: { input_tokens: 10, output_tokens: 5, cached_tokens: 2, cache_hit_rate: 0.2, cost: null },
  budget: { model_used: 4, model_max: 100, tool_used: 2, tool_max: 100 },
  policy: { allow: 2, ask: 1, deny: 1 },
  hooks: { total: 3, warnings: 1, blocked: 0 },
  recent_errors: [{ run_id: 'run_failed', session_id: 'ses_1', type: 'run.failed', message: 'planned failure', at: '2026-11-19T00:00:00Z' }],
};
const populatedDoctor = [
  { name: 'config', status: 'ok', detail: 'valid' },
  { name: 'failed_runs', status: 'fail', detail: '1 failed run', run_id: 'run_failed' },
  { name: 'overall', status: 'fail', detail: '1 failure, 0 warnings' },
];

let mode: 'populated' | 'empty' | 'error' = 'populated';
Object.defineProperty(globalThis, 'fetch', {
  value: async (url: string | URL | Request) => {
    const path = String(url);
    if (mode === 'error') return new Response(JSON.stringify({ error: 'stats unavailable' }), { status: 503, statusText: 'Unavailable' });
    if (path.endsWith('/v1/stats')) {
      const value = mode === 'empty' ? { ...populatedStats, sessions: 0, runs: 0, events: 0, active_runs: 0, failed_runs: 0, model_calls: 0, recent_errors: [] } : populatedStats;
      return new Response(JSON.stringify(value), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
    const value = mode === 'empty' ? [{ name: 'failed_runs', status: 'ok', detail: 'no failed runs in recent history' }] : populatedDoctor;
    return new Response(JSON.stringify(value), { status: 200, headers: { 'Content-Type': 'application/json' } });
  },
  configurable: true,
});

assert.equal((await api.getStats()).failed_runs, 1);
assert.equal((await api.getDoctor()).find((item) => item.name === 'failed_runs')?.run_id, 'run_failed');

mode = 'empty';
const emptyStats = await api.getStats();
assert.equal(emptyStats.sessions, 0);
assert.deepEqual(emptyStats.recent_errors, []);
assert.equal((await api.getDoctor())[0].status, 'ok');

mode = 'error';
await assert.rejects(() => api.getStats(), /stats unavailable/);
await assert.rejects(() => api.getDoctor(), /stats unavailable/);

console.log('observability API populated, empty, and error states: ok');
