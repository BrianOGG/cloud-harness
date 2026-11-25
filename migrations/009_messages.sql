-- Durable user-visible transcript projection. Source rows remain authoritative;
-- this table gives the API stable IDs and a restart-safe keyset order.
CREATE TABLE messages (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  run_id TEXT NOT NULL DEFAULT '',
  turn_id TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system')),
  source TEXT NOT NULL,
  kind TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL,
  event TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE INDEX idx_messages_session_order ON messages(session_id, created_at, id);

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_user_' || t.id, t.session_id, COALESCE(r.id, ''), t.id, 'user', 'turn', 'input', t.status, t.user_input, t.created_at
FROM turns t
LEFT JOIN runs r ON r.turn_id = t.id;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_response_' || mr.id, r.session_id, r.id, r.turn_id, 'assistant', 'model_response',
       'response', 'completed', group_concat(json_extract(i.content_json, '$.text'), ''), mr.created_at
FROM model_responses mr
JOIN runs r ON r.id = mr.run_id
JOIN steps s ON s.id = mr.step_id
JOIN model_response_items i ON i.response_id = mr.id
WHERE i.kind = 'text' AND s.action NOT IN ('COMPACT', 'PLAN', 'ORACLE')
GROUP BY mr.id, r.session_id, r.id, r.turn_id, mr.created_at;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_answer_' || r.id, r.session_id, r.id, r.turn_id, 'assistant', 'run', 'response', 'completed', r.final_answer, r.updated_at
FROM runs r
WHERE r.final_answer <> ''
  AND NOT EXISTS (
    SELECT 1 FROM messages m
    WHERE m.run_id = r.id AND m.role = 'assistant' AND m.kind = 'response' AND m.content = r.final_answer
  );

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_question_' || i.id, r.session_id, r.id, r.turn_id, 'assistant', 'ask_user', 'question', 'waiting', json_extract(i.args_json, '$.question'), mr.created_at
FROM tool_invocations i
JOIN tool_batches b ON b.batch_id = i.batch_id
JOIN model_responses mr ON mr.id = b.model_response_id
JOIN runs r ON r.id = i.run_id
WHERE i.tool_name = 'ask_user' AND json_extract(i.args_json, '$.question') IS NOT NULL;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_answer_' || i.id, r.session_id, r.id, r.turn_id, 'user', 'ask_user_answer', 'answer', 'completed', json_extract(i.result_json, '$.data.answer'), r.updated_at
FROM tool_invocations i
JOIN runs r ON r.id = i.run_id
WHERE i.tool_name = 'ask_user' AND json_extract(i.result_json, '$.data.answer') IS NOT NULL;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_approval_' || a.id || '_requested', r.session_id, r.id, r.turn_id, 'system', 'approval', 'approval_requested', 'pending', 'Approval requested for ' || a.tool, a.created_at
FROM approvals a
JOIN runs r ON r.id = a.run_id;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_approval_' || a.id || '_decision', r.session_id, r.id, r.turn_id, 'system', 'approval', 'approval_decision', a.state, 'Approval ' || a.state || ' for ' || a.tool, a.created_at
FROM approvals a
JOIN runs r ON r.id = a.run_id
WHERE a.state <> 'pending';
