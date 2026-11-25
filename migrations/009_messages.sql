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
  status TEXT NOT NULL,
  content TEXT NOT NULL,
  event TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE INDEX idx_messages_session_order ON messages(session_id, created_at, id);

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_user_' || t.id, t.session_id,
       COALESCE((SELECT r.id FROM runs r WHERE r.turn_id = t.id AND r.session_id = t.session_id AND r.parent_run_id = '' ORDER BY r.created_at, r.id LIMIT 1), ''),
       t.id, 'user', 'turn', 'input', 'recorded', t.user_input, t.created_at
FROM turns t;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_response_' || mr.id, r.session_id, r.id, r.turn_id, 'assistant', 'model_response',
       'response', 'recorded',
       (SELECT group_concat(json_extract(ordered.content_json, '$.text'), '')
        FROM (SELECT i.content_json FROM model_response_items i
              WHERE i.response_id = mr.id AND i.kind = 'text' ORDER BY i.ordinal ASC) ordered),
       mr.created_at
FROM model_responses mr
JOIN runs r ON r.id = mr.run_id
JOIN steps s ON s.id = mr.step_id
WHERE mr.complete = 1 AND s.action NOT IN ('COMPACT', 'PLAN', 'ORACLE')
  AND EXISTS (SELECT 1 FROM model_response_items i WHERE i.response_id = mr.id AND i.kind = 'text');

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_answer_' || r.id, r.session_id, r.id, r.turn_id, 'assistant', 'run', 'response', 'recorded', r.final_answer, r.updated_at
FROM runs r
WHERE r.status = 'COMPLETED' AND r.final_answer <> ''
  AND NOT EXISTS (
    SELECT 1 FROM model_responses latest
    JOIN steps latest_step ON latest_step.id = latest.step_id
    JOIN messages m ON m.id = 'msg_response_' || latest.id
    WHERE latest.run_id = r.id AND latest.complete = 1
      AND latest_step.action NOT IN ('COMPACT', 'PLAN', 'ORACLE')
      AND latest_step.step_index = (
        SELECT MAX(current_step.step_index)
        FROM model_responses current_response
        JOIN steps current_step ON current_step.id = current_response.step_id
        WHERE current_response.run_id = r.id AND current_response.complete = 1
      )
  );

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_question_' || i.id, r.session_id, r.id, r.turn_id, 'assistant', 'ask_user', 'question', 'recorded', json_extract(i.args_json, '$.question'), mr.created_at
FROM tool_invocations i
JOIN tool_batches b ON b.batch_id = i.batch_id
JOIN model_responses mr ON mr.id = b.model_response_id
JOIN runs r ON r.id = i.run_id
WHERE i.tool_name = 'ask_user' AND json_extract(i.args_json, '$.question') IS NOT NULL;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_answer_' || i.id, r.session_id, r.id, r.turn_id, 'user', 'ask_user_answer', 'answer', 'recorded', json_extract(i.result_json, '$.data.answer'), r.updated_at
FROM tool_invocations i
JOIN runs r ON r.id = i.run_id
WHERE i.tool_name = 'ask_user' AND i.status = 'SUCCEEDED'
  AND json_extract(i.result_json, '$.ok') = 1
  AND json_extract(i.result_json, '$.data.answer') IS NOT NULL;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_approval_' || a.id || '_requested', r.session_id, r.id, r.turn_id, 'system', 'approval', 'approval_requested', 'recorded', 'Approval requested for ' || a.tool, a.created_at
FROM approvals a
JOIN runs r ON r.id = a.run_id;

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, created_at)
SELECT 'msg_approval_' || a.id || '_decision', r.session_id, r.id, r.turn_id, 'system', 'approval', 'approval_decision', 'recorded',
       CASE a.state WHEN 'approved' THEN 'approve' WHEN 'denied' THEN 'deny' ELSE 'deny' END, r.updated_at
FROM approvals a
JOIN runs r ON r.id = a.run_id
WHERE a.state IN ('approved', 'denied') OR EXISTS (
  SELECT 1 FROM tool_invocations i WHERE i.run_id = a.run_id AND i.id = a.invocation_id AND i.status = 'DENIED'
);

INSERT INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, event, created_at)
SELECT 'msg_status_' || r.id, r.session_id, r.id, r.turn_id, 'system', 'run', 'run_status',
       CASE r.status WHEN 'FAILED' THEN 'failed' WHEN 'CANCELLED' THEN 'cancelled' ELSE 'recorded' END,
       CASE WHEN r.status IN ('FAILED', 'CANCELLED') AND COALESCE(json_extract(r.error_json, '$.message'), '') <> ''
            THEN json_extract(r.error_json, '$.message') ELSE r.status END,
       COALESCE((SELECT e.type FROM events e WHERE e.run_id = r.id AND lower(e.type) = lower('run.' || r.status) ORDER BY e.seq DESC LIMIT 1), ''),
       r.updated_at
FROM runs r
WHERE r.status IN ('COMPLETED', 'FAILED', 'CANCELLED');
