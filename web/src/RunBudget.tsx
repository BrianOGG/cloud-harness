import type { Budget } from './api';
import { getBudgetCounters } from './budget';

export function RunBudget({ budget }: { budget: Budget }) {
  return (
    <div className="budget-grid" aria-label="运行预算">
      {getBudgetCounters(budget).map(({ label, used, max, icon }) => {
        const ratio = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;
        return (
          <div className="budget-stat" key={label}>
            <div className="budget-stat-top"><span className="budget-icon">{icon}</span><span>{label}</span></div>
            <div className="budget-value"><strong>{used}</strong><span> / {max}</span></div>
            <div className="budget-track"><span style={{ width: `${ratio}%` }} /></div>
          </div>
        );
      })}
    </div>
  );
}
