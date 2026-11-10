-- Cloud Harness schema v1. Later milestones upgrade this; they do not replace it.

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE turns (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  user_input TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE runs (
  id TEXT PRIMARY KEY,
  parent_run_id TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL REFERENCES sessions(id),
  turn_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  wait_reason TEXT NOT NULL DEFAULT '',
  budget_json TEXT NOT NULL,
  version INTEGER NOT NULL,
  final_answer TEXT NOT NULL DEFAULT '',
  error_json TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE run_leases (
  run_id TEXT PRIMARY KEY REFERENCES runs(id),
  owner_id TEXT NOT NULL,
  lease_version INTEGER NOT NULL,
  expires_at TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL
);

CREATE TABLE steps (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id),
  step_index INTEGER NOT NULL,
  model_response_id TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (run_id, step_index)
);

CREATE TABLE model_responses (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id),
  step_id TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL,
  provider_response_id TEXT NOT NULL DEFAULT '',
  complete INTEGER NOT NULL,
  usage_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE model_response_items (
  id TEXT PRIMARY KEY,
  response_id TEXT NOT NULL REFERENCES model_responses(id),
  ordinal INTEGER NOT NULL,
  kind TEXT NOT NULL,
  content_json TEXT NOT NULL,
  provider_call_id TEXT NOT NULL DEFAULT '',
  UNIQUE (response_id, ordinal)
);

CREATE TABLE provider_continuations (
  response_id TEXT PRIMARY KEY REFERENCES model_responses(id),
  provider TEXT NOT NULL,
  opaque_payload BLOB NOT NULL,
  retention TEXT NOT NULL,
  access_scope TEXT NOT NULL
);

CREATE TABLE tool_batches (
  batch_id TEXT PRIMARY KEY,
  model_response_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL REFERENCES runs(id),
  status TEXT NOT NULL,
  action TEXT NOT NULL,
  next_ordinal INTEGER NOT NULL,
  protocol_json TEXT NOT NULL DEFAULT ''
);

CREATE TABLE tool_invocations (
  id TEXT PRIMARY KEY,
  batch_id TEXT NOT NULL REFERENCES tool_batches(batch_id),
  ordinal INTEGER NOT NULL,
  provider_call_id TEXT NOT NULL,
  run_id TEXT NOT NULL REFERENCES runs(id),
  tool_name TEXT NOT NULL,
  args_json TEXT NOT NULL,
  class TEXT NOT NULL,
  status TEXT NOT NULL,
  idempotency_key TEXT NOT NULL DEFAULT '',
  result_json TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL,
  dispatched INTEGER NOT NULL DEFAULT 0,
  UNIQUE (batch_id, ordinal),
  UNIQUE (batch_id, provider_call_id)
);

CREATE TABLE events (
  event_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id),
  seq INTEGER NOT NULL,
  batch_id TEXT NOT NULL DEFAULT '',
  invocation_id TEXT NOT NULL DEFAULT '',
  type TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  persistent INTEGER NOT NULL CHECK (persistent = 1),
  UNIQUE (run_id, seq)
);

CREATE TABLE checkpoints (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id),
  version INTEGER NOT NULL,
  snapshot_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE approvals (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  invocation_id TEXT NOT NULL DEFAULT '',
  tool TEXT NOT NULL,
  resource_scope TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  valid_until TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE scoped_grants (
  id TEXT PRIMARY KEY,
  subject TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  invocation_id TEXT NOT NULL DEFAULT '',
  tool TEXT NOT NULL,
  op TEXT NOT NULL,
  resource_scope TEXT NOT NULL DEFAULT '',
  valid_until TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE turn_summaries (
  turn_id TEXT PRIMARY KEY,
  version INTEGER NOT NULL,
  summary TEXT NOT NULL,
  source_refs TEXT NOT NULL
);

CREATE TABLE session_memories (
  session_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  state_json TEXT NOT NULL,
  source_refs TEXT NOT NULL,
  PRIMARY KEY (session_id, version)
);

CREATE TABLE history_docs (
  source_id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  raw_text TEXT NOT NULL,
  metadata_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE history_exact (
  term TEXT NOT NULL,
  source_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  PRIMARY KEY (term, source_id, kind)
);

CREATE VIRTUAL TABLE history_fts USING fts5(
  source_id UNINDEXED,
  session_id UNINDEXED,
  tokens,
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE INDEX idx_runs_session ON runs(session_id, created_at);
CREATE INDEX idx_events_run ON events(run_id, seq);
CREATE INDEX idx_history_exact_term ON history_exact(term);
CREATE INDEX idx_history_docs_session ON history_docs(session_id);
