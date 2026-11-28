package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func (db *DB) CreateSession(ctx context.Context, s storage.Session) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO sessions(id, created_at, updated_at) VALUES(?, ?, ?)`, s.ID, formatTime(s.CreatedAt), formatTime(s.UpdatedAt))
	return err
}

func (db *DB) GetSession(ctx context.Context, id string) (storage.Session, error) {
	var s storage.Session
	var created, updated string
	err := db.sql.QueryRowContext(ctx, `SELECT id, created_at, updated_at FROM sessions WHERE id = ?`, id).Scan(&s.ID, &created, &updated)
	if err != nil {
		return storage.Session{}, mapErr(err, id)
	}
	s.CreatedAt, _ = parseTime(created)
	s.UpdatedAt, _ = parseTime(updated)
	return s, nil
}

func (db *DB) ListSessions(ctx context.Context) ([]storage.SessionSummary, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT s.id, s.created_at, s.updated_at, COUNT(r.id), latest.status, latest.created_at
		FROM sessions s
		LEFT JOIN runs r ON r.session_id = s.id
		LEFT JOIN runs latest ON latest.id = (
			SELECT r2.id FROM runs r2
			WHERE r2.session_id = s.id
			ORDER BY r2.created_at DESC, r2.id DESC
			LIMIT 1
		)
		GROUP BY s.id, s.created_at, s.updated_at, latest.status, latest.created_at
		ORDER BY s.updated_at DESC, s.id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]storage.SessionSummary, 0)
	for rows.Next() {
		var session storage.SessionSummary
		var created, updated string
		var latestStatus, latestAt sql.NullString
		if err := rows.Scan(&session.ID, &created, &updated, &session.RunCount, &latestStatus, &latestAt); err != nil {
			return nil, err
		}
		session.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		session.UpdatedAt, err = parseTime(updated)
		if err != nil {
			return nil, err
		}
		if latestStatus.Valid {
			session.LatestRunStatus = domain.RunStatus(latestStatus.String)
		}
		if latestAt.Valid {
			t, err := parseTime(latestAt.String)
			if err != nil {
				return nil, err
			}
			session.LatestRunAt = &t
		}
		out = append(out, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (db *DB) CreateTurn(ctx context.Context, t storage.Turn) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO turns(id, session_id, user_input, status, created_at) VALUES(?, ?, ?, ?, ?)`,
		t.ID, t.SessionID, t.UserInput, t.Status, formatTime(t.CreatedAt)); err != nil {
		return err
	}
	if err := insertMessage(ctx, tx, storage.Message{
		ID: "msg_user_" + t.ID, SessionID: t.SessionID, TurnID: t.ID,
		Role: "user", Source: "turn", Kind: "input", Status: "recorded", Content: t.UserInput, CreatedAt: t.CreatedAt,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) CreateRun(ctx context.Context, r storage.Run) error {
	return db.CreateRunWithPreferences(ctx, r, nil)
}

func (db *DB) CreateRunWithPreferences(ctx context.Context, r storage.Run, prefs *storage.SessionPreferences) error {
	budget, err := json.Marshal(r.Budget)
	if err != nil {
		return err
	}
	errJSON, err := marshalError(r.Error)
	if err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO runs(id, parent_run_id, session_id, turn_id, workspace_id, status, wait_reason, budget_json, version, final_answer, error_json, created_at, updated_at, worktree_base_sha, worktree_branch, worktree_path, worktree_state)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ParentRunID, r.SessionID, r.TurnID, r.WorkspaceID, r.Status, r.WaitReason, string(budget), r.Version, r.FinalAnswer, errJSON, formatTime(r.CreatedAt), formatTime(r.UpdatedAt), r.WorktreeBaseSHA, r.WorktreeBranch, r.WorktreePath, r.WorktreeState); err != nil {
		return err
	}
	if r.ParentRunID == "" && r.TurnID != "" {
		if _, err := tx.ExecContext(ctx, "UPDATE messages SET run_id = COALESCE((SELECT root.id FROM runs root WHERE root.turn_id = ? AND root.session_id = ? AND root.parent_run_id = '' ORDER BY root.created_at, root.id LIMIT 1), '') WHERE id = ? AND session_id = ? AND turn_id = ?", r.TurnID, r.SessionID, "msg_user_"+r.TurnID, r.SessionID, r.TurnID); err != nil {
			return err
		}
	}
	if prefs != nil {
		if err := db.saveSessionPreferences(ctx, tx, *prefs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) GetRun(ctx context.Context, id string) (storage.Run, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, parent_run_id, session_id, turn_id, workspace_id, status, wait_reason, budget_json, version, final_answer, error_json, created_at, updated_at, worktree_base_sha, worktree_branch, worktree_path, worktree_state
		FROM runs WHERE id = ?`, id)
	return scanRun(row, id)
}

func (db *DB) ListRuns(ctx context.Context, sessionID string) ([]storage.Run, error) {
	q := `SELECT id, parent_run_id, session_id, turn_id, workspace_id, status, wait_reason, budget_json, version, final_answer, error_json, created_at, updated_at, worktree_base_sha, worktree_branch, worktree_path, worktree_state FROM runs`
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID == "" {
		rows, err = db.sql.QueryContext(ctx, q+` ORDER BY created_at ASC LIMIT 200`)
	} else {
		rows, err = db.sql.QueryContext(ctx, q+` WHERE session_id = ? ORDER BY created_at ASC, id ASC`, sessionID)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []storage.Run
	for rows.Next() {
		r, err := scanRun(rows, "")
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRunsByStatus reads all matching runs across sessions without the interactive
// ListRuns limit. It does not change run state or acquire leases.
func (db *DB) ListRunsByStatus(ctx context.Context, status domain.RunStatus) ([]storage.Run, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, parent_run_id, session_id, turn_id, workspace_id, status, wait_reason, budget_json, version, final_answer, error_json, created_at, updated_at, worktree_base_sha, worktree_branch, worktree_path, worktree_state
		FROM runs WHERE status = ? ORDER BY created_at ASC, id ASC`, status)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]storage.Run, 0)
	for rows.Next() {
		r, err := scanRun(rows, "")
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) ListExpiredRunningRuns(ctx context.Context, now time.Time) ([]storage.Run, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT r.id, r.parent_run_id, r.session_id, r.turn_id, r.workspace_id, r.status, r.wait_reason, r.budget_json, r.version, r.final_answer, r.error_json, r.created_at, r.updated_at, r.worktree_base_sha, r.worktree_branch, r.worktree_path, r.worktree_state
		FROM runs r
		LEFT JOIN run_leases l ON l.run_id = r.id
		WHERE r.status = ? AND (l.run_id IS NULL OR l.expires_at <= ?)
		ORDER BY r.created_at ASC`, domain.RunRunning, formatTime(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []storage.Run
	for rows.Next() {
		r, err := scanRun(rows, "")
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) CompareAndSwapRun(ctx context.Context, next storage.Run, expectedVersion int64) error {
	budget, err := json.Marshal(next.Budget)
	if err != nil {
		return err
	}
	errJSON, err := marshalError(next.Error)
	if err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
		UPDATE runs
		SET status = ?, wait_reason = ?, budget_json = ?, final_answer = ?, error_json = ?, version = ?, updated_at = ?
		WHERE id = ? AND version = ?`,
		next.Status, next.WaitReason, string(budget), next.FinalAnswer, errJSON, expectedVersion+1, formatTime(next.UpdatedAt), next.ID, expectedVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.E(domain.CodeConflict, "run version conflict").WithRef(next.ID)
	}
	return nil
}

func (db *DB) Charge(ctx context.Context, runID string, kind domain.ChargeKind, n int) error {
	var parentID string
	err := db.sql.QueryRowContext(ctx, `SELECT parent_run_id FROM runs WHERE id = ?`, runID).Scan(&parentID)
	if err != nil {
		return mapErr(err, runID)
	}
	if parentID != "" {
		return db.ChargePair(ctx, parentID, runID, kind, n)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT budget_json FROM runs WHERE id = ?`, runID).Scan(&raw); err != nil {
		return mapErr(err, runID)
	}
	var b domain.Budget
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return err
	}
	next, err := b.Charge(kind, n)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET budget_json = ?, updated_at = ? WHERE id = ?`, string(encoded), formatTime(db.now()), runID); err != nil {
		return err
	}
	return tx.Commit()
}

// ChargePair applies one nested charge to the parent and the child in a single
// transaction. If either budget would be exceeded, neither row changes.
func (db *DB) ChargePair(ctx context.Context, parentID, childID string, kind domain.ChargeKind, n int) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	load := func(id string) (domain.Budget, error) {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT budget_json FROM runs WHERE id = ?`, id).Scan(&raw); err != nil {
			return domain.Budget{}, mapErr(err, id)
		}
		var b domain.Budget
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return domain.Budget{}, err
		}
		return b, nil
	}
	parent, err := load(parentID)
	if err != nil {
		return err
	}
	child, err := load(childID)
	if err != nil {
		return err
	}
	nextParent, nextChild, err := domain.ChargeNested(parent, child, kind, n)
	if err != nil {
		return err
	}
	when := formatTime(db.now())
	write := func(id string, b domain.Budget) error {
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE runs SET budget_json = ?, updated_at = ? WHERE id = ?`, string(raw), when, id)
		return err
	}
	if err := write(parentID, nextParent); err != nil {
		return err
	}
	if err := write(childID, nextChild); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) AcquireLease(ctx context.Context, runID, owner string, now, expires time.Time) (storage.Lease, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return storage.Lease{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var cur storage.Lease
	var exp, beat string
	err = tx.QueryRowContext(ctx, `SELECT owner_id, lease_version, expires_at, heartbeat_at FROM run_leases WHERE run_id = ?`, runID).
		Scan(&cur.OwnerID, &cur.LeaseVersion, &exp, &beat)
	version := int64(1)
	if err == nil {
		cur.ExpiresAt, _ = parseTime(exp)
		if cur.OwnerID != owner && cur.ExpiresAt.After(now) {
			return storage.Lease{}, domain.E(domain.CodeLease, "run lease is held").WithRef(runID)
		}
		version = cur.LeaseVersion + 1
		if _, err := tx.ExecContext(ctx, `UPDATE run_leases SET owner_id = ?, lease_version = ?, expires_at = ?, heartbeat_at = ? WHERE run_id = ?`,
			owner, version, formatTime(expires), formatTime(now), runID); err != nil {
			return storage.Lease{}, err
		}
	} else if err == sql.ErrNoRows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_leases(run_id, owner_id, lease_version, expires_at, heartbeat_at) VALUES(?, ?, ?, ?, ?)`,
			runID, owner, version, formatTime(expires), formatTime(now)); err != nil {
			return storage.Lease{}, err
		}
	} else {
		return storage.Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return storage.Lease{}, err
	}
	return storage.Lease{RunID: runID, OwnerID: owner, LeaseVersion: version, ExpiresAt: expires, HeartbeatAt: now}, nil
}

func (db *DB) HeartbeatLease(ctx context.Context, runID, owner string, version int64, now, expires time.Time) error {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE run_leases SET heartbeat_at = ?, expires_at = ?
		WHERE run_id = ? AND owner_id = ? AND lease_version = ? AND expires_at > ?`,
		formatTime(now), formatTime(expires), runID, owner, version, formatTime(now))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.E(domain.CodeLease, "lease heartbeat rejected").WithRef(runID)
	}
	return nil
}

func (db *DB) RequireLease(ctx context.Context, runID, owner string, version int64, now time.Time) error {
	var n int
	err := db.sql.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM run_leases
		WHERE run_id = ? AND owner_id = ? AND lease_version = ? AND expires_at > ?`,
		runID, owner, version, formatTime(now)).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.E(domain.CodeLease, "lease is not valid for this owner").WithRef(runID)
	}
	return nil
}

func (db *DB) CommitRunTransition(ctx context.Context, next storage.Run, expectedVersion int64, ev storage.Event) error {
	if !ev.Persistent {
		return domain.E(domain.CodeInvalidArgument, "run transition requires a persistent event")
	}
	budget, err := json.Marshal(next.Budget)
	if err != nil {
		return err
	}
	errJSON, err := marshalError(next.Error)
	if err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE runs
		SET status = ?, wait_reason = ?, budget_json = ?, final_answer = ?, error_json = ?, version = ?, updated_at = ?
		WHERE id = ? AND version = ?`,
		next.Status, next.WaitReason, string(budget), next.FinalAnswer, errJSON, expectedVersion+1, formatTime(next.UpdatedAt), next.ID, expectedVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.E(domain.CodeConflict, "run version conflict").WithRef(next.ID)
	}
	ev.RunID = next.ID
	ev.Persistent = true
	seq, err := insertEvent(ctx, tx, ev)
	if err != nil {
		return err
	}
	if err := insertTransitionMessages(ctx, tx, next, ev); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	db.eventCommitted(next.ID, seq)
	return nil
}

func insertTransitionMessages(ctx context.Context, tx *sql.Tx, run storage.Run, ev storage.Event) error {
	created := ev.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	switch run.Status {
	case domain.RunCompleted:
		if strings.TrimSpace(run.FinalAnswer) != "" {
			var answerResponseID string
			if err := tx.QueryRowContext(ctx, `SELECT final_answer_response_id FROM runs WHERE id = ?`, run.ID).Scan(&answerResponseID); err != nil {
				return err
			}
			if answerResponseID == "" {
				if err := insertMessage(ctx, tx, storage.Message{
					ID: "msg_answer_" + run.ID, SessionID: run.SessionID, RunID: run.ID, TurnID: run.TurnID,
					Role: "assistant", Source: "run", Kind: "response", Status: "recorded", Content: run.FinalAnswer, CreatedAt: created,
				}); err != nil {
					return err
				}
			}
		}
		return insertTerminalStatusMessage(ctx, tx, run, ev, created)
	case domain.RunFailed, domain.RunCancelled:
		return insertTerminalStatusMessage(ctx, tx, run, ev, created)
	default:
		return nil
	}
}

func insertTerminalStatusMessage(ctx context.Context, tx *sql.Tx, run storage.Run, ev storage.Event, created time.Time) error {
	status := "recorded"
	content := string(run.Status)
	if run.Status == domain.RunFailed {
		status = "failed"
	} else if run.Status == domain.RunCancelled {
		status = "cancelled"
	}
	if run.Error != nil && run.Error.Message != "" {
		content = run.Error.Message
	}
	return insertMessage(ctx, tx, storage.Message{
		ID: "msg_status_" + run.ID, SessionID: run.SessionID, RunID: run.ID, TurnID: run.TurnID,
		Role: "system", Source: "run", Kind: "run_status", Status: status, Content: content, CreatedAt: created, Event: ev.Type,
	})
}

func (db *DB) SaveCheckpoint(ctx context.Context, runID string, version int64, snapshot []byte, now time.Time) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO checkpoints(id, run_id, version, snapshot_json, event_seq, created_at)
		VALUES(?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) FROM events WHERE run_id = ?), ?)`,
		domain.NewID("ckp_"), runID, version, string(snapshot), runID, formatTime(now))
	return err
}

func (db *DB) LatestCheckpoint(ctx context.Context, runID string) ([]byte, error) {
	var raw string
	err := db.sql.QueryRowContext(ctx, `
		SELECT snapshot_json FROM checkpoints WHERE run_id = ?
		ORDER BY version DESC, created_at DESC LIMIT 1`, runID).Scan(&raw)
	if err != nil {
		return nil, mapErr(err, runID)
	}
	return []byte(raw), nil
}

// LatestCheckpointAtOrBefore locates a whole checkpoint by its actual event
// boundary. Legacy checkpoints without a sequence are never guessed from time
// or run version.
func (db *DB) LatestCheckpointAtOrBefore(ctx context.Context, runID string, eventSeq int64) ([]byte, int64, error) {
	var raw string
	var seq sql.NullInt64
	err := db.sql.QueryRowContext(ctx, `
		SELECT snapshot_json, event_seq FROM checkpoints
		WHERE run_id = ? AND event_seq IS NOT NULL AND event_seq <= ?
		ORDER BY event_seq DESC, version DESC, created_at DESC LIMIT 1`, runID, eventSeq).Scan(&raw, &seq)
	if err != nil {
		return nil, 0, mapErr(err, runID)
	}
	if !seq.Valid {
		return nil, 0, fmt.Errorf("checkpoint for run %q has no event sequence", runID)
	}
	return []byte(raw), seq.Int64, nil
}

// RewindChat removes events after the requested boundary and drops newer
// checkpoints in one transaction. It refuses to mutate anything when the run
// has no checkpoint at or before that boundary.
func (db *DB) RewindChat(ctx context.Context, runID string, eventSeq int64) (storage.ChatRewindResult, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return storage.ChatRewindResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var checkpointSeq, checkpointVersion int64
	var checkpointTime string
	if err := tx.QueryRowContext(ctx, `
		SELECT event_seq, version, created_at FROM checkpoints
		WHERE run_id = ? AND event_seq IS NOT NULL AND event_seq <= ?
		ORDER BY event_seq DESC, version DESC, created_at DESC LIMIT 1`, runID, eventSeq).Scan(&checkpointSeq, &checkpointVersion, &checkpointTime); err != nil {
		if err == sql.ErrNoRows {
			return storage.ChatRewindResult{}, fmt.Errorf("run %q has no checkpoint at or before event seq %d", runID, eventSeq)
		}
		return storage.ChatRewindResult{}, err
	}
	var result storage.ChatRewindResult
	result.CheckpointSeq = checkpointSeq
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(1), COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0)
		FROM events WHERE run_id = ? AND seq > ?`, runID, eventSeq).
		Scan(&result.DeletedEvents, &result.FirstSeq, &result.LastSeq); err != nil {
		return storage.ChatRewindResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE run_id = ? AND seq > ?`, runID, eventSeq); err != nil {
		return storage.ChatRewindResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE run_id = ? AND
		(event_seq IS NULL OR event_seq > ? OR version > ? OR (version = ? AND created_at > ?))`,
		runID, checkpointSeq, checkpointVersion, checkpointVersion, checkpointTime); err != nil {
		return storage.ChatRewindResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return storage.ChatRewindResult{}, err
	}
	return result, nil
}

func (db *DB) LookupIdempotency(ctx context.Context, runID, toolName, key string) (json.RawMessage, bool, error) {
	var raw string
	err := db.sql.QueryRowContext(ctx, `
		SELECT result_json FROM idempotency_keys WHERE run_id = ? AND tool_name = ? AND idempotency_key = ?`,
		runID, toolName, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(raw), true, nil
}

func (db *DB) SaveIdempotency(ctx context.Context, runID, toolName, key string, result json.RawMessage, now time.Time) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO idempotency_keys(idempotency_key, run_id, tool_name, result_json, created_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(idempotency_key, run_id, tool_name) DO NOTHING`,
		key, runID, toolName, string(result), formatTime(now))
	return err
}

// ExtendBudget raises the limits of one run and returns the stored budget.
func (db *DB) ExtendBudget(ctx context.Context, runID string, x domain.BudgetExtension) (domain.Budget, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return domain.Budget{}, err
	}
	defer func() { _ = tx.Rollback() }()
	b, err := loadBudget(ctx, tx, runID)
	if err != nil {
		return domain.Budget{}, err
	}
	next, err := b.Extend(x)
	if err != nil {
		return domain.Budget{}, err
	}
	if err := saveBudget(ctx, tx, runID, next, formatTime(db.now())); err != nil {
		return domain.Budget{}, err
	}
	return next, tx.Commit()
}

// ChargeUsage records the tokens and cost of a finished model call on the run
// and, for a child run, on its parent in the same transaction.
func (db *DB) ChargeUsage(ctx context.Context, runID string, u domain.UsageCharge) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var parentID string
	if err := tx.QueryRowContext(ctx, `SELECT parent_run_id FROM runs WHERE id = ?`, runID).Scan(&parentID); err != nil {
		return mapErr(err, runID)
	}
	when := formatTime(db.now())
	for _, id := range []string{runID, parentID} {
		if id == "" {
			continue
		}
		b, err := loadBudget(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := saveBudget(ctx, tx, id, b.AddUsage(u), when); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func loadBudget(ctx context.Context, tx *sql.Tx, runID string) (domain.Budget, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT budget_json FROM runs WHERE id = ?`, runID).Scan(&raw); err != nil {
		return domain.Budget{}, mapErr(err, runID)
	}
	var b domain.Budget
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return domain.Budget{}, err
	}
	return b, nil
}

func saveBudget(ctx context.Context, tx *sql.Tx, runID string, b domain.Budget, when string) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE runs SET budget_json = ?, updated_at = ? WHERE id = ?`, string(raw), when, runID)
	return err
}

func (db *DB) ReleaseLease(ctx context.Context, runID, owner string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM run_leases WHERE run_id = ? AND owner_id = ?`, runID, owner)
	return err
}

func (db *DB) AppendEvent(ctx context.Context, ev storage.Event) (int64, error) {
	if !ev.Persistent {
		return 0, domain.E(domain.CodeInvalidArgument, "ephemeral events are not stored")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	seq, err := insertEvent(ctx, tx, ev)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	db.eventCommitted(ev.RunID, seq)
	return seq, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, ev storage.Event) (int64, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE run_id = ?`, ev.RunID).Scan(&seq); err != nil {
		return 0, err
	}
	if ev.ID == "" {
		ev.ID = domain.NewID("evt_")
	}
	if len(ev.Payload) == 0 {
		ev.Payload = json.RawMessage(`{}`)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO events(event_id, run_id, seq, batch_id, invocation_id, type, payload_json, created_at, persistent)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		ev.ID, ev.RunID, seq, ev.BatchID, ev.InvocationID, ev.Type, string(ev.Payload), formatTime(ev.CreatedAt))
	if err != nil {
		return 0, err
	}
	return seq, nil
}

func (db *DB) ListEvents(ctx context.Context, runID string, afterSeq int64) ([]storage.Event, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT event_id, run_id, seq, batch_id, invocation_id, type, payload_json, created_at
		FROM events WHERE run_id = ? AND seq > ? ORDER BY seq ASC`, runID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []storage.Event
	for rows.Next() {
		var ev storage.Event
		var payload, created string
		if err := rows.Scan(&ev.ID, &ev.RunID, &ev.Seq, &ev.BatchID, &ev.InvocationID, &ev.Type, &payload, &created); err != nil {
			return nil, err
		}
		ev.Payload = json.RawMessage(payload)
		ev.CreatedAt, _ = parseTime(created)
		ev.Persistent = true
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (db *DB) CommitInvocation(ctx context.Context, runID string, inv domain.Invocation, expectedVersion int64, ev storage.Event) error {
	if !ev.Persistent {
		return domain.E(domain.CodeInvalidArgument, "invocation transition requires a persistent event")
	}
	result, err := json.Marshal(inv.Result)
	if err != nil {
		return err
	}
	args := inv.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	dispatched := 0
	if inv.Dispatched {
		dispatched = 1
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE tool_invocations
		SET status = ?, result_json = ?, version = ?, dispatched = ?, args_json = ?
		WHERE id = ? AND run_id = ? AND version = ?`,
		inv.Status, string(result), expectedVersion+1, dispatched, string(args), inv.ID, runID, expectedVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.E(domain.CodeConflict, "invocation version conflict").WithRef(inv.ID)
	}
	ev.RunID = runID
	ev.InvocationID = inv.ID
	ev.Persistent = true
	seq, err := insertEvent(ctx, tx, ev)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	db.eventCommitted(runID, seq)
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRun(sc scanner, id string) (storage.Run, error) {
	var r storage.Run
	var budget, errJSON, created, updated string
	var status, reason string
	err := sc.Scan(&r.ID, &r.ParentRunID, &r.SessionID, &r.TurnID, &r.WorkspaceID, &status, &reason, &budget, &r.Version, &r.FinalAnswer, &errJSON, &created, &updated, &r.WorktreeBaseSHA, &r.WorktreeBranch, &r.WorktreePath, &r.WorktreeState)
	if err != nil {
		return storage.Run{}, mapErr(err, id)
	}
	r.Status = domain.RunStatus(status)
	r.WaitReason = domain.WaitReason(reason)
	if budget != "" {
		if err := json.Unmarshal([]byte(budget), &r.Budget); err != nil {
			return storage.Run{}, err
		}
	}
	if errJSON != "" && errJSON != "null" {
		var de domain.Error
		if err := json.Unmarshal([]byte(errJSON), &de); err != nil {
			return storage.Run{}, err
		}
		if de.Code != "" {
			r.Error = &de
		}
	}
	r.CreatedAt, _ = parseTime(created)
	r.UpdatedAt, _ = parseTime(updated)
	return r, nil
}

func marshalError(e *domain.Error) (string, error) {
	if e == nil {
		return "", nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// PruneRuns removes terminal runs (COMPLETED, FAILED, CANCELLED) updated before
// the cutoff and all associated child rows across all run-related tables.
// A zero time prunes nothing. All deletions run atomically in a single transaction,
// verified with PRAGMA foreign_key_check. If foreign key check fails, the transaction
// is rolled back and an error is returned. On commit, PRAGMA wal_checkpoint(TRUNCATE)
// is executed best-effort.
func (db *DB) PruneRuns(ctx context.Context, before time.Time) (storage.PruneRunsResult, error) {
	tables := map[string]int{
		"workspace_snapshots":    0,
		"checkpoints":            0,
		"events":                 0,
		"provider_continuations": 0,
		"model_response_items":   0,
		"model_responses":        0,
		"tool_invocations":       0,
		"tool_batches":           0,
		"steps":                  0,
		"run_leases":             0,
		"approvals":              0,
		"scoped_grants":          0,
		"idempotency_keys":       0,
		"summary_jobs":           0,
		"runs":                   0,
	}
	result := storage.PruneRunsResult{
		Tables: tables,
	}
	if before.IsZero() {
		return result, nil
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _prune_run_ids (id TEXT PRIMARY KEY)`); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM _prune_run_ids`); err != nil {
		return result, err
	}
	defer func() {
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS _prune_run_ids`)
	}()

	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _prune_snapshot_ids (id TEXT PRIMARY KEY)`); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM _prune_snapshot_ids`); err != nil {
		return result, err
	}
	defer func() {
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS _prune_snapshot_ids`)
	}()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO _prune_run_ids (id)
		SELECT id FROM runs
		WHERE status IN (?, ?, ?) AND updated_at < ?`,
		domain.RunCompleted, domain.RunFailed, domain.RunCancelled, formatTime(before))
	if err != nil {
		return result, err
	}
	nCandidates, err := res.RowsAffected()
	if err != nil {
		return result, err
	}
	result.RunsToDelete = int(nCandidates)

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO _prune_snapshot_ids(id)
		SELECT ws.id FROM workspace_snapshots ws
		WHERE (
			CASE
				WHEN ws.run_id <> '' THEN ws.run_id
				WHEN ws.label LIKE 'rewind-safety/%' THEN substr(substr(ws.label, length('rewind-safety/') + 1), 1, instr(substr(ws.label, length('rewind-safety/') + 1), '/') - 1)
				WHEN ws.label LIKE 'run_%/%' THEN substr(ws.label, 1, instr(ws.label, '/') - 1)
				ELSE ''
			END IN (SELECT id FROM _prune_run_ids)
		) OR (
			CASE
				WHEN ws.run_id <> '' THEN ws.run_id
				WHEN ws.label LIKE 'rewind-safety/%' THEN substr(substr(ws.label, length('rewind-safety/') + 1), 1, instr(substr(ws.label, length('rewind-safety/') + 1), '/') - 1)
				WHEN ws.label LIKE 'run_%/%' THEN substr(ws.label, 1, instr(ws.label, '/') - 1)
				ELSE ''
			END <> ''
			AND NOT EXISTS (
				SELECT 1 FROM runs r WHERE r.id = CASE
					WHEN ws.run_id <> '' THEN ws.run_id
					WHEN ws.label LIKE 'rewind-safety/%' THEN substr(substr(ws.label, length('rewind-safety/') + 1), 1, instr(substr(ws.label, length('rewind-safety/') + 1), '/') - 1)
					WHEN ws.label LIKE 'run_%/%' THEN substr(ws.label, 1, instr(ws.label, '/') - 1)
					ELSE ''
				END
			)
		)`,
	); err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM _prune_snapshot_ids`).Scan(&result.SnapshotsToDelete); err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM workspace_snapshots ws
		WHERE CASE
			WHEN ws.run_id <> '' THEN ws.run_id
			WHEN ws.label LIKE 'rewind-safety/%' THEN substr(substr(ws.label, length('rewind-safety/') + 1), 1, instr(substr(ws.label, length('rewind-safety/') + 1), '/') - 1)
			WHEN ws.label LIKE 'run_%/%' THEN substr(ws.label, 1, instr(ws.label, '/') - 1)
			ELSE ''
		END = ''`).Scan(&result.UnassignedRetained); err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, run_id, workspace_id, workspace_path, tree_oid, label, event_seq, file_count, total_bytes, created_at
		FROM workspace_snapshots WHERE id IN (SELECT id FROM _prune_snapshot_ids)
		ORDER BY workspace_id, id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		rec, scanErr := scanWorkspaceSnapshot(rows)
		if scanErr != nil {
			_ = rows.Close()
			return result, scanErr
		}
		result.Snapshots = append(result.Snapshots, rec)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	if err = rows.Close(); err != nil {
		return result, err
	}

	if result.SnapshotsToDelete == 0 && nCandidates == 0 {
		_ = tx.Rollback()
		return result, nil
	}

	del := func(table string, query string) error {
		r, e := tx.ExecContext(ctx, query)
		if e != nil {
			return fmt.Errorf("delete from %s: %w", table, e)
		}
		cnt, e := r.RowsAffected()
		if e != nil {
			return fmt.Errorf("rows affected %s: %w", table, e)
		}
		tables[table] = int(cnt)
		return nil
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE messages SET run_id = ''
		WHERE kind = 'input' AND run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	if err = del("messages", `
		DELETE FROM messages WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}

	// 1. Delete child rows of model_responses (provider_continuations, model_response_items)
	if err = del("workspace_snapshots", `DELETE FROM workspace_snapshots WHERE id IN (SELECT id FROM _prune_snapshot_ids)`); err != nil {
		return result, err
	}
	result.SnapshotsDeleted = tables["workspace_snapshots"]

	if err = del("provider_continuations", `
		DELETE FROM provider_continuations
		WHERE response_id IN (
			SELECT mr.id FROM model_responses mr JOIN _prune_run_ids p ON mr.run_id = p.id
		)`); err != nil {
		return result, err
	}
	if err = del("model_response_items", `
		DELETE FROM model_response_items
		WHERE response_id IN (
			SELECT mr.id FROM model_responses mr JOIN _prune_run_ids p ON mr.run_id = p.id
		)`); err != nil {
		return result, err
	}

	// 2. Delete model_responses
	if err = del("model_responses", `DELETE FROM model_responses WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}

	// 3. Delete tool_invocations then tool_batches
	if err = del("tool_invocations", `DELETE FROM tool_invocations WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	if err = del("tool_batches", `DELETE FROM tool_batches WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}

	// 4. Delete steps, events, checkpoints, run_leases
	if err = del("steps", `DELETE FROM steps WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	if err = del("events", `DELETE FROM events WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	if err = del("checkpoints", `DELETE FROM checkpoints WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	if err = del("run_leases", `DELETE FROM run_leases WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}

	// 5. Delete tables with run_id but no foreign key constraints
	if err = del("approvals", `DELETE FROM approvals WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	if err = del("scoped_grants", `DELETE FROM scoped_grants WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	if err = del("idempotency_keys", `DELETE FROM idempotency_keys WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	if err = del("summary_jobs", `DELETE FROM summary_jobs WHERE run_id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}

	// 6. Delete runs
	if err = del("runs", `DELETE FROM runs WHERE id IN (SELECT id FROM _prune_run_ids)`); err != nil {
		return result, err
	}
	result.RunsDeleted = tables["runs"]

	// Drop temp table before checking FK
	if _, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS _prune_snapshot_ids`); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS _prune_run_ids`); err != nil {
		return result, err
	}

	// 7. PRAGMA foreign_key_check inside the transaction
	fkRows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return result, fmt.Errorf("pragma foreign_key_check: %w", err)
	}
	var violations []string
	for fkRows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err = fkRows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			_ = fkRows.Close()
			return result, fmt.Errorf("scan foreign_key_check: %w", err)
		}
		violations = append(violations, fmt.Sprintf("table=%s, rowid=%v, parent=%s, fkid=%d", table, rowid, parent, fkid))
	}
	if err = fkRows.Err(); err != nil {
		_ = fkRows.Close()
		return result, fmt.Errorf("foreign_key_check rows: %w", err)
	}
	_ = fkRows.Close()

	if len(violations) > 0 {
		err = fmt.Errorf("foreign key check failed: %s", strings.Join(violations, "; "))
		return result, err
	}

	// 8. Commit transaction
	if err = tx.Commit(); err != nil {
		return result, err
	}

	// 9. Best-effort PRAGMA wal_checkpoint(TRUNCATE) after commit
	var busy, logPages, checkpointedPages int
	if cpErr := db.sql.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logPages, &checkpointedPages); cpErr != nil {
		result.WALCheckpoint = fmt.Sprintf("failed: %v", cpErr)
	} else if busy != 0 {
		result.WALCheckpoint = "busy"
	} else {
		result.WALCheckpoint = "ok"
	}

	return result, nil
}

// PreviewPruneRuns reports the rows and snapshots a prune would select without
// changing persistent database state.
func (db *DB) PreviewPruneRuns(ctx context.Context, before time.Time) (storage.PruneRunsResult, error) {
	result := storage.PruneRunsResult{Tables: map[string]int{"workspace_snapshots": 0, "runs": 0}}
	if before.IsZero() {
		return result, nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE _prune_run_ids (id TEXT PRIMARY KEY)`); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE _prune_snapshot_ids (id TEXT PRIMARY KEY)`); err != nil {
		return result, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO _prune_run_ids(id) SELECT id FROM runs WHERE status IN (?, ?, ?) AND updated_at < ?`,
		domain.RunCompleted, domain.RunFailed, domain.RunCancelled, formatTime(before))
	if err != nil {
		return result, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return result, err
	}
	result.RunsToDelete = int(n)
	result.Tables["runs"] = int(n)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO _prune_snapshot_ids(id)
		SELECT ws.id FROM workspace_snapshots ws
		WHERE (
			CASE
				WHEN ws.run_id <> '' THEN ws.run_id
				WHEN ws.label LIKE 'rewind-safety/%' THEN substr(substr(ws.label, length('rewind-safety/') + 1), 1, instr(substr(ws.label, length('rewind-safety/') + 1), '/') - 1)
				WHEN ws.label LIKE 'run_%/%' THEN substr(ws.label, 1, instr(ws.label, '/') - 1)
				ELSE ''
			END IN (SELECT id FROM _prune_run_ids)
		) OR (
			CASE
				WHEN ws.run_id <> '' THEN ws.run_id
				WHEN ws.label LIKE 'rewind-safety/%' THEN substr(substr(ws.label, length('rewind-safety/') + 1), 1, instr(substr(ws.label, length('rewind-safety/') + 1), '/') - 1)
				WHEN ws.label LIKE 'run_%/%' THEN substr(ws.label, 1, instr(ws.label, '/') - 1)
				ELSE ''
			END <> ''
			AND NOT EXISTS (
				SELECT 1 FROM runs r WHERE r.id = CASE
					WHEN ws.run_id <> '' THEN ws.run_id
					WHEN ws.label LIKE 'rewind-safety/%' THEN substr(substr(ws.label, length('rewind-safety/') + 1), 1, instr(substr(ws.label, length('rewind-safety/') + 1), '/') - 1)
					WHEN ws.label LIKE 'run_%/%' THEN substr(ws.label, 1, instr(ws.label, '/') - 1)
					ELSE ''
				END
			)
		)`); err != nil {
		return result, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM _prune_snapshot_ids`).Scan(&result.SnapshotsToDelete); err != nil {
		return result, err
	}
	result.Tables["workspace_snapshots"] = result.SnapshotsToDelete
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM workspace_snapshots ws WHERE CASE
		WHEN ws.run_id <> '' THEN ws.run_id
		WHEN ws.label LIKE 'rewind-safety/%' THEN substr(substr(ws.label, length('rewind-safety/') + 1), 1, instr(substr(ws.label, length('rewind-safety/') + 1), '/') - 1)
		WHEN ws.label LIKE 'run_%/%' THEN substr(ws.label, 1, instr(ws.label, '/') - 1)
		ELSE '' END = ''`).Scan(&result.UnassignedRetained); err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, run_id, workspace_id, workspace_path, tree_oid, label, event_seq, file_count, total_bytes, created_at FROM workspace_snapshots WHERE id IN (SELECT id FROM _prune_snapshot_ids) ORDER BY workspace_id, id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		rec, scanErr := scanWorkspaceSnapshot(rows)
		if scanErr != nil {
			_ = rows.Close()
			return result, scanErr
		}
		result.Snapshots = append(result.Snapshots, rec)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	return result, nil
}
