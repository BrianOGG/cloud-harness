CREATE TABLE idempotency_keys (
  idempotency_key TEXT NOT NULL,
  run_id TEXT NOT NULL,
  tool_name TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (idempotency_key, run_id, tool_name)
);
