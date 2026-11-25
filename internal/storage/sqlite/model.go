package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func (db *DB) SaveModelResponse(ctx context.Context, rec storage.StoredResponse) error {
	usage, err := json.Marshal(rec.Usage)
	if err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if rec.StepID == "" {
		rec.StepID = domain.NewID("stp_")
	}
	var idx int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(step_index), -1) + 1 FROM steps WHERE run_id = ?`, rec.RunID).Scan(&idx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO steps(id, run_id, step_index, model_response_id, action, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		rec.StepID, rec.RunID, idx, rec.ID, rec.Action, formatTime(rec.CreatedAt)); err != nil {
		return err
	}
	complete := 0
	if rec.Complete {
		complete = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_responses(id, run_id, step_id, provider, provider_response_id, complete, usage_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.RunID, rec.StepID, rec.Provider, rec.ProviderResponseID, complete, string(usage), formatTime(rec.CreatedAt)); err != nil {
		return err
	}
	for _, item := range rec.Items {
		body, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_response_items(id, response_id, ordinal, kind, content_json, provider_call_id)
			VALUES(?, ?, ?, ?, ?, ?)`,
			domain.NewID("itm_"), rec.ID, item.Ordinal, item.Kind, string(body), item.ProviderCallID); err != nil {
			return err
		}
	}
	if rec.Action != domain.ActionCompact && rec.Action != domain.ActionPlan && rec.Action != "ORACLE" {
		var sessionID, turnID string
		if err := tx.QueryRowContext(ctx, `SELECT session_id, turn_id FROM runs WHERE id = ?`, rec.RunID).Scan(&sessionID, &turnID); err != nil {
			return err
		}
		var text strings.Builder
		for _, item := range rec.Items {
			if item.Kind == domain.ItemText {
				text.WriteString(item.Text)
			}
		}
		if text.Len() > 0 {
			if err := insertMessage(ctx, tx, storage.Message{
				ID: "msg_response_" + rec.ID, SessionID: sessionID, RunID: rec.RunID, TurnID: turnID,
				Role: "assistant", Source: "model_response", Kind: "response", Status: "completed", Content: text.String(), CreatedAt: rec.CreatedAt,
			}); err != nil {
				return err
			}
		}
	}
	if len(rec.Opaque) > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provider_continuations(response_id, provider, opaque_payload, retention, access_scope)
			VALUES(?, ?, ?, 'run', 'provider_replay')`, rec.ID, rec.Provider, rec.Opaque); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) ListVisibleResponses(ctx context.Context, runID string) ([]storage.StoredResponse, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, run_id, step_id, provider, provider_response_id, complete, usage_json, created_at
		FROM model_responses WHERE run_id = ? ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	var out []storage.StoredResponse
	for rows.Next() {
		var rec storage.StoredResponse
		var usage, created string
		var complete int
		if err := rows.Scan(&rec.ID, &rec.RunID, &rec.StepID, &rec.Provider, &rec.ProviderResponseID, &complete, &usage, &created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		rec.Complete = complete == 1
		_ = json.Unmarshal([]byte(usage), &rec.Usage)
		rec.CreatedAt, _ = parseTime(created)
		out = append(out, rec)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	// The pool has one connection. Load items only after the cursor is released.
	for i := range out {
		items, err := db.listItems(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Items = items
	}
	return out, nil
}

func (db *DB) listItems(ctx context.Context, responseID string) ([]domain.VisibleItem, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT content_json FROM model_response_items WHERE response_id = ? ORDER BY ordinal ASC`, responseID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.VisibleItem
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item domain.VisibleItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// LoadContinuation reads the restricted opaque blob. Trace and search must not call it.
func (db *DB) LoadContinuation(ctx context.Context, responseID string) ([]byte, error) {
	var blob []byte
	err := db.sql.QueryRowContext(ctx, `SELECT opaque_payload FROM provider_continuations WHERE response_id = ?`, responseID).Scan(&blob)
	if err != nil {
		return nil, mapErr(err, responseID)
	}
	return blob, nil
}

func (db *DB) SaveBatch(ctx context.Context, batch storage.StoredBatch) error {
	proto, err := marshalError(batch.ProtocolError)
	if err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tool_batches(batch_id, model_response_id, run_id, status, action, next_ordinal, protocol_json)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		batch.ID, batch.ModelResponseID, batch.RunID, batch.Status, batch.Action, batch.NextOrdinal, proto); err != nil {
		return err
	}
	for _, inv := range batch.Invocations {
		args := inv.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		result := ""
		if inv.Result != nil {
			b, err := json.Marshal(inv.Result)
			if err != nil {
				return err
			}
			result = string(b)
		}
		dispatched := 0
		if inv.Dispatched {
			dispatched = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tool_invocations(id, batch_id, ordinal, provider_call_id, run_id, tool_name, args_json, class, status, result_json, version, dispatched)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
			inv.ID, batch.ID, inv.Ordinal, inv.ProviderCallID, batch.RunID, inv.Name, string(args), inv.Class, inv.Status, result, dispatched); err != nil {
			return err
		}
		if inv.Name == domain.ToolAskUser {
			var argsBody struct {
				Question string `json:"question"`
			}
			if err := json.Unmarshal(args, &argsBody); err != nil {
				return err
			}
			if argsBody.Question != "" {
				var sessionID, turnID, created string
				if err := tx.QueryRowContext(ctx, `
					SELECT r.session_id, r.turn_id, mr.created_at
					FROM runs r JOIN model_responses mr ON mr.id = ? WHERE r.id = ?`,
					batch.ModelResponseID, batch.RunID).Scan(&sessionID, &turnID, &created); err != nil {
					return err
				}
				questionTime, err := parseTime(created)
				if err != nil {
					return err
				}
				if err := insertMessage(ctx, tx, storage.Message{
					ID: "msg_question_" + inv.ID, SessionID: sessionID, RunID: batch.RunID, TurnID: turnID,
					Role: "assistant", Source: "ask_user", Kind: "question", Status: "waiting", Content: argsBody.Question, CreatedAt: questionTime,
				}); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func (db *DB) UpdateBatch(ctx context.Context, batch storage.StoredBatch) error {
	proto, err := marshalError(batch.ProtocolError)
	if err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE tool_batches SET status = ?, action = ?, next_ordinal = ?, protocol_json = ? WHERE batch_id = ?`,
		batch.Status, batch.Action, batch.NextOrdinal, proto, batch.ID); err != nil {
		return err
	}
	for _, inv := range batch.Invocations {
		result := ""
		if inv.Result != nil {
			raw, err := json.Marshal(inv.Result)
			if err != nil {
				return err
			}
			result = string(raw)
		}
		dispatched := 0
		if inv.Dispatched {
			dispatched = 1
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tool_invocations
			SET status = ?, result_json = ?, dispatched = ?, version = version + 1
			WHERE id = ? AND batch_id = ?`,
			inv.Status, result, dispatched, inv.ID, batch.ID); err != nil {
			return err
		}
		if inv.Name == domain.ToolAskUser && inv.Result != nil && len(inv.Result.Data) > 0 {
			var answerBody struct {
				Answer string `json:"answer"`
			}
			if json.Unmarshal(inv.Result.Data, &answerBody) == nil && answerBody.Answer != "" {
				var sessionID, turnID string
				if err := tx.QueryRowContext(ctx, `SELECT session_id, turn_id FROM runs WHERE id = ?`, batch.RunID).Scan(&sessionID, &turnID); err != nil {
					return err
				}
				if err := insertMessage(ctx, tx, storage.Message{
					ID: "msg_answer_" + inv.ID, SessionID: sessionID, RunID: batch.RunID, TurnID: turnID,
					Role: "user", Source: "ask_user_answer", Kind: "answer", Status: "completed", Content: answerBody.Answer, CreatedAt: db.now(),
				}); err != nil {
					return err
				}
			}
		}
		if inv.Status == domain.InvDenied {
			var sessionID, turnID string
			if err := tx.QueryRowContext(ctx, `SELECT session_id, turn_id FROM runs WHERE id = ?`, batch.RunID).Scan(&sessionID, &turnID); err != nil {
				return err
			}
			if err := insertMessage(ctx, tx, storage.Message{
				ID: "msg_approval_" + inv.ID + "_denied", SessionID: sessionID, RunID: batch.RunID, TurnID: turnID,
				Role: "system", Source: "approval", Kind: "approval_decision", Status: "denied", Content: "Approval denied for " + inv.Name, CreatedAt: db.now(),
			}); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (db *DB) SaveTurnSummary(ctx context.Context, turnID, summary, sourceRefs string) error {
	if sourceRefs == "" {
		sourceRefs = "[]"
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO turn_summaries(turn_id, version, summary, source_refs) VALUES(?, 1, ?, ?)
		ON CONFLICT(turn_id) DO UPDATE SET version = version + 1, summary = excluded.summary, source_refs = excluded.source_refs`,
		turnID, summary, sourceRefs)
	return err
}

func (db *DB) GetTurnSummary(ctx context.Context, turnID string) (string, error) {
	var summary string
	err := db.sql.QueryRowContext(ctx, `SELECT summary FROM turn_summaries WHERE turn_id = ?`, turnID).Scan(&summary)
	if err != nil {
		return "", mapErr(err, turnID)
	}
	return summary, nil
}

func (db *DB) SaveMemory(ctx context.Context, sessionID, stateJSON, sourceRefs string) error {
	if sourceRefs == "" {
		sourceRefs = "[]"
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM session_memories WHERE session_id = ?`, sessionID).Scan(&version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_memories(session_id, version, state_json, source_refs) VALUES(?, ?, ?, ?)`,
		sessionID, version, stateJSON, sourceRefs); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) LoadBatches(ctx context.Context, runID string) ([]storage.StoredBatch, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT batch_id, model_response_id, run_id, status, action, next_ordinal, protocol_json
		FROM tool_batches WHERE run_id = ? ORDER BY rowid ASC`, runID)
	if err != nil {
		return nil, err
	}
	var out []storage.StoredBatch
	for rows.Next() {
		var b storage.StoredBatch
		var proto string
		var status, action string
		if err := rows.Scan(&b.ID, &b.ModelResponseID, &b.RunID, &status, &action, &b.NextOrdinal, &proto); err != nil {
			_ = rows.Close()
			return nil, err
		}
		b.Status = domain.BatchStatus(status)
		b.Action = domain.Action(action)
		if proto != "" {
			var de domain.Error
			if err := json.Unmarshal([]byte(proto), &de); err != nil {
				_ = rows.Close()
				return nil, err
			}
			b.ProtocolError = &de
		}
		out = append(out, b)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		inv, err := db.loadInvocations(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Invocations = inv
	}
	return out, nil
}

func (db *DB) loadInvocations(ctx context.Context, batchID string) ([]domain.Invocation, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, ordinal, provider_call_id, tool_name, args_json, class, status, result_json, dispatched, version
		FROM tool_invocations WHERE batch_id = ? ORDER BY ordinal ASC`, batchID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Invocation
	for rows.Next() {
		var inv domain.Invocation
		var args, class, status, result string
		var dispatched int
		if err := rows.Scan(&inv.ID, &inv.Ordinal, &inv.ProviderCallID, &inv.Name, &args, &class, &status, &result, &dispatched, &inv.Version); err != nil {
			return nil, err
		}
		inv.Arguments = json.RawMessage(args)
		inv.Class = domain.Action(class)
		inv.Status = domain.InvStatus(status)
		inv.Dispatched = dispatched == 1
		if result != "" && result != "null" {
			var cr domain.CallResult
			if err := json.Unmarshal([]byte(result), &cr); err != nil {
				return nil, err
			}
			inv.Result = &cr
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (db *DB) SaveApproval(ctx context.Context, a domain.Approval) error {
	if a.ID == "" {
		a.ID = domain.NewID("apr_")
	}
	until := ""
	if !a.ValidUntil.IsZero() {
		until = formatTime(a.ValidUntil)
	}
	created := a.CreatedAt
	if created.IsZero() {
		created = db.now()
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO approvals(id, run_id, invocation_id, tool, resource_scope, state, valid_until, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET state = excluded.state, resource_scope = excluded.resource_scope, valid_until = excluded.valid_until`,
		a.ID, a.RunID, a.InvocationID, a.Tool, a.ResourceScope, a.State, until, formatTime(created))
	if err != nil {
		return err
	}
	var sessionID, turnID string
	if err := db.sql.QueryRowContext(ctx, `SELECT session_id, turn_id FROM runs WHERE id = ?`, a.RunID).Scan(&sessionID, &turnID); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertMessage(ctx, tx, storage.Message{
		ID: "msg_approval_" + a.ID + "_requested", SessionID: sessionID, RunID: a.RunID, TurnID: turnID,
		Role: "system", Source: "approval", Kind: "approval_requested", Status: "pending", Content: "Approval requested for " + a.Tool, CreatedAt: created,
	}); err != nil {
		return err
	}
	if a.State != "pending" {
		if err := insertMessage(ctx, tx, storage.Message{
			ID: "msg_approval_" + a.ID + "_decision", SessionID: sessionID, RunID: a.RunID, TurnID: turnID,
			Role: "system", Source: "approval", Kind: "approval_decision", Status: a.State, Content: "Approval " + a.State + " for " + a.Tool, CreatedAt: created,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) UpdateApprovalState(ctx context.Context, runID, invocationID, state string) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE approvals SET state = ? WHERE run_id = ? AND invocation_id = ?`,
		state, runID, invocationID)
	if err != nil {
		return err
	}
	var approvalID, tool, sessionID, turnID string
	if err := db.sql.QueryRowContext(ctx, `
		SELECT a.id, a.tool, r.session_id, r.turn_id
		FROM approvals a JOIN runs r ON r.id = a.run_id
		WHERE a.run_id = ? AND a.invocation_id = ? ORDER BY a.created_at DESC LIMIT 1`, runID, invocationID).
		Scan(&approvalID, &tool, &sessionID, &turnID); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertMessage(ctx, tx, storage.Message{
		ID: "msg_approval_" + approvalID + "_decision", SessionID: sessionID, RunID: runID, TurnID: turnID,
		Role: "system", Source: "approval", Kind: "approval_decision", Status: state, Content: "Approval " + state + " for " + tool, CreatedAt: db.now(),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) ListApprovals(ctx context.Context, runID string) ([]domain.Approval, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, run_id, invocation_id, tool, resource_scope, state, valid_until, created_at
		FROM approvals WHERE run_id = ? ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Approval
	for rows.Next() {
		var a domain.Approval
		var until, created string
		if err := rows.Scan(&a.ID, &a.RunID, &a.InvocationID, &a.Tool, &a.ResourceScope, &a.State, &until, &created); err != nil {
			return nil, err
		}
		if until != "" {
			a.ValidUntil, _ = parseTime(until)
		}
		a.CreatedAt, _ = parseTime(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) SaveGrant(ctx context.Context, g domain.Grant, now time.Time) error {
	until := ""
	if !g.ValidUntil.IsZero() {
		until = formatTime(g.ValidUntil)
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO scoped_grants(id, subject, run_id, session_id, invocation_id, tool, op, resource_scope, valid_until, state, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.Subject, g.RunID, g.SessionID, g.InvocationID, g.Tool, g.Op, g.ResourceScope, until, g.State, formatTime(now))
	return err
}

func (db *DB) ListGrants(ctx context.Context, runID, sessionID string, now time.Time) ([]domain.Grant, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, subject, run_id, session_id, invocation_id, tool, op, resource_scope, valid_until, state
		FROM scoped_grants
		WHERE state = 'active' AND (run_id = ? OR (run_id = '' AND session_id = ?))`, runID, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Grant
	for rows.Next() {
		var g domain.Grant
		var until string
		if err := rows.Scan(&g.ID, &g.Subject, &g.RunID, &g.SessionID, &g.InvocationID, &g.Tool, &g.Op, &g.ResourceScope, &until, &g.State); err != nil {
			return nil, err
		}
		if until != "" {
			g.ValidUntil, _ = parseTime(until)
			if !g.ValidUntil.After(now) {
				continue
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
