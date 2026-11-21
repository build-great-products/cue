package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/build-great-products/cue/internal/identity"
	"github.com/build-great-products/cue/internal/task"
)

// ReplyAdapterEffect is one durable intent to deliver a task reply through a
// configured reply adapter. It has the Slack effect lifecycle (prepared,
// submitted, then confirmed, failed or ambiguous) and keeps no reply text,
// only its SHA-256.
type ReplyAdapterEffect struct {
	ID             string
	TaskID         string
	Adapter        string
	PayloadHash    string
	State          SlackEffectState
	AttemptCount   int
	ExitCode       *int
	DiagnosticCode string
	PreparedAt     time.Time
	SubmittedAt    *time.Time
	CompletedAt    *time.Time
	UpdatedAt      time.Time
}

// ReplyAdapterOutcome is the terminal result of one submitted adapter run.
type ReplyAdapterOutcome struct {
	State          SlackEffectState
	ExitCode       *int
	DiagnosticCode string
}

// Diagnostic codes recorded on reply adapter effects.
const (
	ReplyAdapterDiagnosticExit                  = "adapter_exit"
	ReplyAdapterDiagnosticFailed                = "adapter_failed"
	ReplyAdapterDiagnosticNotStarted            = "adapter_not_started"
	ReplyAdapterDiagnosticTimeout               = "timeout"
	ReplyAdapterDiagnosticSignal                = "adapter_signaled"
	ReplyAdapterDiagnosticStartupReconciliation = "startup_reconciliation"
)

var ErrReplyAdapterEffectNotFound = errors.New("reply adapter effect not found")

const replyAdapterEffectColumns = `effect_id,related_task_id,adapter,payload_hash,state,attempt_count,exit_code,coalesce(diagnostic_code,''),prepared_at_us,submitted_at_us,completed_at_us,updated_at_us`

// ReplyAdapterPayloadHash is the SHA-256 of the reply text an adapter is
// given, bound to the adapter and task.
func ReplyAdapterPayloadHash(taskID, adapter, text string) string {
	sum := sha256.New()
	for _, v := range []string{"cue.reply_adapter.payload.v1", taskID, adapter, text} {
		_, _ = sum.Write([]byte(v))
		_, _ = sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

type replyAdapterPrepared struct {
	EffectID string `json:"effect_id"`
}

// PrepareReplyAdapterEffect records a prepared adapter effect for a task
// reply under requestID before anything runs. A replay of the same request
// and input returns the effect it created, in whatever state it is now; the
// same request ID with other input is a conflict.
func (s *Store) PrepareReplyAdapterEffect(ctx context.Context, requestID, taskID, adapter, text string) (ReplyAdapterEffect, error) {
	if !identity.ValidRequestID(requestID) {
		return ReplyAdapterEffect{}, invalidInput("invalid request ID")
	}
	if !validFullTaskID(taskID) || !task.ValidSourceName(adapter) {
		return ReplyAdapterEffect{}, invalidInput("invalid reply adapter effect")
	}
	normalized, err := normalizeSlackEffectText(text)
	if err != nil {
		return ReplyAdapterEffect{}, invalidInput(err.Error())
	}
	payload := ReplyAdapterPayloadHash(taskID, adapter, normalized)
	req := request{operation: "reply_adapter.effect.prepare", hash: hash("reply_adapter.effect.prepare.v1", payload)}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	defer tx.Rollback()
	var operation, inputHash, outcome, resultType, resultJSON string
	err = tx.QueryRowContext(ctx, `SELECT operation,input_hash,outcome,coalesce(result_type,''),coalesce(result_json,'')
		FROM mutation_requests WHERE request_id=?`, requestID).Scan(&operation, &inputHash, &outcome, &resultType, &resultJSON)
	if err == nil {
		if operation != req.operation || inputHash != req.hash || outcome != "success" || resultType != "reply_adapter_effect" {
			return ReplyAdapterEffect{}, ErrRequestConflict
		}
		var prior replyAdapterPrepared
		if err := json.Unmarshal([]byte(resultJSON), &prior); err != nil {
			return ReplyAdapterEffect{}, err
		}
		return getReplyAdapterEffect(ctx, tx, prior.EffectID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReplyAdapterEffect{}, err
	}
	effectID, err := s.ids.NewEffectID()
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO reply_adapter_effects(
		effect_id,related_task_id,adapter,payload_hash,state,attempt_count,prepared_at_us,updated_at_us
	) VALUES(?,?,?,?,'prepared',0,?,?)`, effectID, taskID, adapter, payload, now, now); err != nil {
		return ReplyAdapterEffect{}, err
	}
	encoded, err := json.Marshal(replyAdapterPrepared{EffectID: effectID})
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_requests(
		request_id,operation,input_hash,outcome,result_type,result_json,created_at,completed_at,expiry_eligible_at
	) VALUES(?,?,?,'success','reply_adapter_effect',?,?,?,?)`,
		requestID, req.operation, req.hash, string(encoded), now, now, now+RequestRetention.Microseconds()); err != nil {
		return ReplyAdapterEffect{}, err
	}
	effect, err := getReplyAdapterEffect(ctx, tx, effectID)
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	return effect, tx.Commit()
}

// ClaimReplyAdapterEffect moves a prepared effect to submitted, the one
// attempt it gets. It must commit before the adapter runs.
func (s *Store) ClaimReplyAdapterEffect(ctx context.Context, effectID string) (ReplyAdapterEffect, error) {
	return s.updateReplyAdapterEffect(ctx, effectID, func(tx *sql.Tx, now int64) (sql.Result, error) {
		return tx.ExecContext(ctx, `UPDATE reply_adapter_effects SET state='submitted',attempt_count=1,
			submitted_at_us=max(?,updated_at_us),updated_at_us=max(?,updated_at_us)
			WHERE effect_id=? AND state='prepared' AND attempt_count=0`, now, now, effectID)
	})
}

// CommitReplyAdapterOutcome makes a submitted effect terminal.
func (s *Store) CommitReplyAdapterOutcome(ctx context.Context, effectID string, outcome ReplyAdapterOutcome) (ReplyAdapterEffect, error) {
	if !outcome.State.terminal() || (outcome.ExitCode != nil && (*outcome.ExitCode < 0 || *outcome.ExitCode > 255)) {
		return ReplyAdapterEffect{}, invalidInput("invalid reply adapter outcome")
	}
	var exit any
	if outcome.ExitCode != nil {
		exit = *outcome.ExitCode
	}
	return s.updateReplyAdapterEffect(ctx, effectID, func(tx *sql.Tx, now int64) (sql.Result, error) {
		return tx.ExecContext(ctx, `UPDATE reply_adapter_effects SET state=?,exit_code=?,diagnostic_code=?,
			completed_at_us=max(?,submitted_at_us,updated_at_us),updated_at_us=max(?,submitted_at_us,updated_at_us)
			WHERE effect_id=? AND state='submitted' AND attempt_count=1`,
			outcome.State, exit, nullString(outcome.DiagnosticCode), now, now, effectID)
	})
}

func (s *Store) updateReplyAdapterEffect(ctx context.Context, effectID string, update func(*sql.Tx, int64) (sql.Result, error)) (ReplyAdapterEffect, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	defer tx.Rollback()
	result, err := update(tx, s.now())
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	changed, err := rowsChanged(result)
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	if !changed {
		if _, err := getReplyAdapterEffect(ctx, tx, effectID); err != nil {
			return ReplyAdapterEffect{}, err
		}
		return ReplyAdapterEffect{}, ErrSlackEffectTransition
	}
	effect, err := getReplyAdapterEffect(ctx, tx, effectID)
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	return effect, tx.Commit()
}

// ReconcileSubmittedReplyAdapterEffects marks every effect left submitted
// by a stopped gateway ambiguous: the adapter may or may not have delivered.
func (s *Store) ReconcileSubmittedReplyAdapterEffects(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	result, err := s.db.ExecContext(ctx, `UPDATE reply_adapter_effects SET state='ambiguous',diagnostic_code=?,
		completed_at_us=max(?,submitted_at_us,updated_at_us),updated_at_us=max(?,submitted_at_us,updated_at_us)
		WHERE state='submitted'`, ReplyAdapterDiagnosticStartupReconciliation, now, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// GetReplyAdapterEffect returns one effect by its full ID.
func (s *Store) GetReplyAdapterEffect(ctx context.Context, effectID string) (ReplyAdapterEffect, error) {
	return getReplyAdapterEffect(ctx, s.db, effectID)
}

// LatestReplyAdapterEffect returns a task's newest adapter effect; false when
// it has none.
func (s *Store) LatestReplyAdapterEffect(ctx context.Context, taskID string) (ReplyAdapterEffect, bool, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+replyAdapterEffectColumns+" FROM reply_adapter_effects WHERE related_task_id=? ORDER BY prepared_at_us DESC, rowid DESC LIMIT 1", taskID)
	effect, err := scanReplyAdapterEffect(row)
	if errors.Is(err, ErrReplyAdapterEffectNotFound) {
		return ReplyAdapterEffect{}, false, nil
	}
	return effect, err == nil, err
}

// ReplyAdapterEffectCounts counts adapter effects by state.
func (s *Store) ReplyAdapterEffectCounts(ctx context.Context) (map[SlackEffectState]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT state,count(*) FROM reply_adapter_effects GROUP BY state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[SlackEffectState]int64{SlackEffectPrepared: 0, SlackEffectSubmitted: 0, SlackEffectConfirmed: 0, SlackEffectFailed: 0, SlackEffectAmbiguous: 0}
	for rows.Next() {
		var state SlackEffectState
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

func getReplyAdapterEffect(ctx context.Context, q rowQueryer, effectID string) (ReplyAdapterEffect, error) {
	return scanReplyAdapterEffect(q.QueryRowContext(ctx, "SELECT "+replyAdapterEffectColumns+" FROM reply_adapter_effects WHERE effect_id=?", effectID))
}

func scanReplyAdapterEffect(row *sql.Row) (ReplyAdapterEffect, error) {
	var e ReplyAdapterEffect
	var exit, submitted, completed sql.NullInt64
	var prepared, updated int64
	err := row.Scan(&e.ID, &e.TaskID, &e.Adapter, &e.PayloadHash, &e.State, &e.AttemptCount, &exit, &e.DiagnosticCode, &prepared, &submitted, &completed, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ReplyAdapterEffect{}, ErrReplyAdapterEffectNotFound
	}
	if err != nil {
		return ReplyAdapterEffect{}, err
	}
	if exit.Valid {
		code := int(exit.Int64)
		e.ExitCode = &code
	}
	e.PreparedAt, e.UpdatedAt = time.UnixMicro(prepared).UTC(), time.UnixMicro(updated).UTC()
	if submitted.Valid {
		at := time.UnixMicro(submitted.Int64).UTC()
		e.SubmittedAt = &at
	}
	if completed.Valid {
		at := time.UnixMicro(completed.Int64).UTC()
		e.CompletedAt = &at
	}
	return e, nil
}
