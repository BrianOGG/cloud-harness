package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/storage"
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

func (db *DB) CreateTurn(ctx context.Context, t storage.Turn) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO turns(id, session_id, user_input, status, created_at) VALUES(?, ?, ?, ?, ?)`,
		t.ID, t.SessionID, t.UserInput, t.Status, formatTime(t.CreatedAt))
	return err
}

func (db *DB) CreateRun(ctx context.Context, r storage.Run) error {
	budget, err := json.Marshal(r.Budget)
	if err != nil {
		return err
	}
	errJSON, err := marshalError(r.Error)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO runs(id, parent_run_id, session_id, turn_id, status, wait_reason, budget_json, version, final_answer, error_json, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ParentRunID, r.SessionID, r.TurnID, r.Status, r.WaitReason, string(budget), r.Version, r.FinalAnswer, errJSON, formatTime(r.CreatedAt), formatTime(r.UpdatedAt))
	return err
}

func (db *DB) GetRun(ctx context.Context, id string) (storage.Run, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, parent_run_id, session_id, turn_id, status, wait_reason, budget_json, version, final_answer, error_json, created_at, updated_at
		FROM runs WHERE id = ?`, id)
	return scanRun(row, id)
}

func (db *DB) ListRuns(ctx context.Context, sessionID string) ([]storage.Run, error) {
	q := `SELECT id, parent_run_id, session_id, turn_id, status, wait_reason, budget_json, version, final_answer, error_json, created_at, updated_at FROM runs`
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID == "" {
		rows, err = db.sql.QueryContext(ctx, q+` ORDER BY created_at ASC LIMIT 200`)
	} else {
		rows, err = db.sql.QueryContext(ctx, q+` WHERE session_id = ? ORDER BY created_at ASC LIMIT 200`, sessionID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

func (db *DB) ListExpiredRunningRuns(ctx context.Context, now time.Time) ([]storage.Run, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT r.id, r.parent_run_id, r.session_id, r.turn_id, r.status, r.wait_reason, r.budget_json, r.version, r.final_answer, r.error_json, r.created_at, r.updated_at
		FROM runs r
		LEFT JOIN run_leases l ON l.run_id = r.id
		WHERE r.status = ? AND (l.run_id IS NULL OR l.expires_at <= ?)
		ORDER BY r.created_at ASC`, domain.RunRunning, formatTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	if _, err := insertEvent(ctx, tx, ev); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) SaveCheckpoint(ctx context.Context, runID string, version int64, snapshot []byte, now time.Time) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO checkpoints(id, run_id, version, snapshot_json, created_at) VALUES(?, ?, ?, ?, ?)`,
		domain.NewID("ckp_"), runID, version, string(snapshot), formatTime(now))
	return err
}

func (db *DB) LatestCheckpoint(ctx context.Context, runID string) ([]byte, error) {
	var raw string
	err := db.sql.QueryRowContext(ctx, `
		SELECT snapshot_json FROM checkpoints WHERE run_id = ? ORDER BY version DESC, created_at DESC LIMIT 1`, runID).Scan(&raw)
	if err != nil {
		return nil, mapErr(err, runID)
	}
	return []byte(raw), nil
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

func (db *DB) ExtendModelBudget(ctx context.Context, runID string, extra int) error {
	if extra <= 0 {
		return domain.E(domain.CodeInvalidArgument, "extra model calls must be positive")
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
	b.MaxModelCalls += extra
	encoded, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET budget_json = ?, updated_at = ? WHERE id = ?`, string(encoded), formatTime(db.now()), runID); err != nil {
		return err
	}
	return tx.Commit()
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
	defer rows.Close()
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
	if _, err := insertEvent(ctx, tx, ev); err != nil {
		return err
	}
	return tx.Commit()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRun(sc scanner, id string) (storage.Run, error) {
	var r storage.Run
	var budget, errJSON, created, updated string
	var status, reason string
	err := sc.Scan(&r.ID, &r.ParentRunID, &r.SessionID, &r.TurnID, &status, &reason, &budget, &r.Version, &r.FinalAnswer, &errJSON, &created, &updated)
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
