-- Keep answer provenance independent of the lifetime of transcript projections.
-- Only the latest complete non-internal action can supply the final answer.
ALTER TABLE runs ADD COLUMN final_answer_response_id TEXT NOT NULL DEFAULT '';

UPDATE runs
SET final_answer_response_id = COALESCE((
  SELECT CASE WHEN s.action = 'ANSWER'
    AND EXISTS (SELECT 1 FROM model_response_items i
                WHERE i.response_id = mr.id AND i.kind = 'text'
                  AND length(json_extract(i.content_json, '$.text')) > 0)
    AND NOT EXISTS (SELECT 1 FROM model_response_items i
                    WHERE i.response_id = mr.id
                      AND json_extract(i.content_json, '$.tool_call.name') = 'finish_turn')
    THEN mr.id ELSE '' END
  FROM model_responses mr JOIN steps s ON s.id = mr.step_id
  WHERE mr.run_id = runs.id AND mr.complete = 1
    AND s.action NOT IN ('COMPACT', 'PLAN', 'ORACLE')
  ORDER BY s.step_index DESC LIMIT 1
), '');

-- Repair only retained projections. Never recreate facts removed by pruning.
DELETE FROM messages
WHERE source = 'run' AND kind = 'response'
  AND EXISTS (SELECT 1 FROM runs r WHERE r.id = messages.run_id
              AND messages.id = 'msg_answer_' || r.id
              AND r.final_answer_response_id <> '');

-- Run-field fallbacks have no event source. Terminal status events keep their
-- original spelling and timestamps.
UPDATE messages SET event = '' WHERE source = 'run' AND kind = 'response';

-- Persist the same user-visible command summary as the approval surface,
-- retaining the original message identity and request time.
UPDATE messages
SET content = (
  SELECT json_extract(i.args_json, '$.command')
  FROM approvals a JOIN tool_invocations i
    ON i.id = a.invocation_id AND i.run_id = a.run_id AND i.tool_name = a.tool
  WHERE messages.id = 'msg_approval_' || a.id || '_requested'
)
WHERE source = 'approval' AND kind = 'approval_requested'
  AND EXISTS (
    SELECT 1 FROM approvals a JOIN tool_invocations i
      ON i.id = a.invocation_id AND i.run_id = a.run_id AND i.tool_name = a.tool
    WHERE messages.id = 'msg_approval_' || a.id || '_requested'
      AND a.tool = 'run_command' AND json_type(i.args_json, '$.command') = 'text'
      AND json_extract(i.args_json, '$.command') <> ''
  );
