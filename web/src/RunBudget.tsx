import type { Budget } from './api';
import { getBudgetCounters } from './budget';

export function RunBudget({ budget }: { budget: Budget }) {
  return (
    <div className="budget-grid" aria-label="运行预算">
      {getBudgetCounters(budget).map(({ label, used, max, icon, usedText, maxText, details, title }) => {
        const ratio = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;
        return (
          <div className="budget-stat" key={label} title={title}>
            <div className="budget-stat-top"><span className="budget-icon">{icon}</span><span>{label}</span></div>
            <div className="budget-value"><strong>{usedText}</strong><span> / {maxText}</span></div>
            <div className="budget-track"><span style={{ width: `${ratio}%` }} /></div>
            {details && <div className="budget-details">{details.map((detail) => <span key={detail}>{detail}</span>)}</div>}
          </div>
        );
      })}
    </div>
  );
}
