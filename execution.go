package sam

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DuplicateEffectWarning describes the contract acknowledged by Resume.
const DuplicateEffectWarning = "Resuming unfinished work may repeat replay-allowed tool and hook effects. Carried-over replay-unsafe calls are skipped and reported to the agent, which may issue fresh calls. Saved results are not rerun; earlier unrecorded effects may already have occurred."

// resumeAdmissionTx binds the unaccepted transaction to the caller and checks
// cancellation after lock acquisition and before admission commits.
func (r *Runtime) resumeAdmissionTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		// A failed BEGIN has not accepted work. If it timed out on the
		// SQLite writer lock after the caller cancelled, report that admission
		// cancellation instead of a storage failure/busy result.
		if admissionCanceled(ctx, err) || (ctx.Err() != nil && isBusy(err)) {
			return ctx.Err()
		}
		return r.txFailed(err, true)
	}
	if err := ctx.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		var e *Error
		if errors.As(err, &e) {
			return err
		}
		if admissionCanceled(ctx, err) {
			return ctx.Err()
		}
		return r.txFailed(err, true)
	}
	if err := ctx.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		if admissionCanceled(ctx, err) {
			return ctx.Err()
		}
		return r.storageFailed(err)
	}
	return nil
}

// admissionCanceled only recognizes errors that identify cancellation or the
// transaction that database/sql rolled back with its context. Independent
// storage errors remain storage errors even when the caller also cancelled.
func admissionCanceled(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, sql.ErrTxDone))
}

// Resume admits all suspended executions without waiting for completion. It
// acknowledges DuplicateEffectWarning. ctx controls admission, not the lifetime
// of admitted work or a strict return deadline: an in-progress SQLite lock wait
// may delay cancellation until its existing five-second busy timeout. Cancellation
// before admission prevents worker launch; after commit the runtime owns the work.
// Repeated calls never create duplicate workers.
func (r *Runtime) Resume(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.admit(); err != nil {
		return err
	}
	var ids []string
	err := r.resumeAdmissionTx(ctx, func(tx *sql.Tx) error {
		if err := queryRows(tx, func(rows *sql.Rows) error {
			var id string
			err := rows.Scan(&id)
			if r.workers[id] == nil {
				ids = append(ids, id)
			}
			return err
		}, `SELECT session_id FROM executions ORDER BY session_id`); err != nil {
			return err
		}
		for _, id := range ids {
			x, err := loadExecution(tx, id)
			if err != nil {
				return err
			}
			if err := appendEvent(tx, x.event(EventOperationResumed, map[string]string{"warning": DuplicateEffectWarning})); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		r.launch(id, true)
	}
	return nil
}

// executeSession is the only orchestration path, for fresh and resumed work.
func (r *Runtime) executeSession(ctx context.Context, sid string, w *worker) {
	defer func() { r.mu.Lock(); defer r.mu.Unlock(); r.detachWorker(sid, w) }()
	for {
		if r.broken.Load() != nil {
			return
		}
		x, err := r.nextExecution(sid, w)
		if err != nil {
			r.storageFailed(err)
			return
		}
		if x == nil {
			return
		}
		if x.Action == actionStop {
			r.finalizeExecution(x)
			continue
		}
		if ctx.Err() != nil {
			r.suspendExecution(x, w)
			return
		}
		if x.Action != actionBatch {
			err = providerBudget(x.Progress)
		}
		if err == nil {
			err = w.selectionError
		}
		if err == nil && (w.main == nil || w.main.turnID != x.TurnID) {
			w.main = nil
			if x.TurnID != "" {
				w.main, err = r.prepareTurn(sid, x.TurnID, w.models)
			}
		}
		if err == nil {
			err = r.executeAction(ctx, x, w)
		}
		if err == nil {
			continue
		}
		if r.broken.Load() != nil {
			return
		}
		if ctx.Err() != nil && errors.Is(context.Cause(ctx), errShutdown) {
			r.suspendExecution(x, w)
			return
		}
		if err := r.failExecution(sid, err); err != nil {
			return
		}
	}
}

// nextExecution closes the completion/steering race. Guidance accepted after
// terminal commit but before worker detachment is queued, not stranded. Its
// acceptance and the final ownership decision share the admission lock.
func (r *Runtime) nextExecution(sid string, w *worker) (*execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, err := loadExecution(r.db, sid)
	if err != nil || x != nil {
		return x, err
	}
	var parent string
	err = r.tx(func(tx *sql.Tx) error {
		if err := r.handoffGuidance(tx, sid, "steering"); err != nil {
			return err
		}
		var err error
		parent, err = r.notifySubagent(tx, sid)
		return err
	})
	if err != nil {
		return nil, err
	}
	r.captureWork(sid, w)
	if parent != "" {
		r.launchNotificationParents([]string{parent})
	}
	x, err = loadExecution(r.db, sid)
	if err == nil && x == nil {
		r.detachWorker(sid, w)
	}
	return x, err
}

func (r *Runtime) executeAction(ctx context.Context, x *execution, w *worker) error {
	switch x.Action {
	case actionPrepare:
		return r.prepareMain(ctx, x, w)
	case actionRequest, actionCompactRequest:
		return r.requestAttempt(ctx, x, w)
	case actionBatch:
		return r.executeBatch(ctx, x, w)
	case actionCompactPrepare:
		return r.prepareSummary(ctx, x, w)
	}
	return fmt.Errorf("invalid dispatch action %q", x.Action)
}

func (r *Runtime) failExecution(sid string, failure error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tx(func(tx *sql.Tx) error {
		x, err := loadExecution(tx, sid)
		if err != nil || x == nil {
			return err
		}
		if x.Action == actionStop {
			return nil
		}
		var he *hookFailure
		if errors.As(failure, &he) {
			if err := appendEvent(tx, he.event); err != nil {
				return err
			}
		}
		stopExecution(x, "failed", failure)
		return saveExecution(tx, x)
	})
}

func (r *Runtime) suspendExecution(old *execution, w *worker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.tx(func(tx *sql.Tx) error {
		x, err := loadExecution(tx, old.SessionID)
		if err != nil || x == nil {
			return err
		}
		if x.Action == actionStop {
			return r.finishStop(tx, x)
		}
		if err := settleAttempt(tx, x, "interrupted", errShutdown, nil, true); err != nil {
			return err
		}
		unprepare(x)
		if err := saveExecution(tx, x); err != nil {
			return err
		}
		return appendEvent(tx, x.event(EventOperationSuspended, map[string]string{"reason": "shutdown"}))
	})
	w.prepared = nil
}

func unprepare(x *execution) {
	switch x.Action {
	case actionRequest:
		x.Action = actionPrepare
	case actionCompactRequest:
		x.Action = actionCompactPrepare
	}
}

func (r *Runtime) finalizeExecution(old *execution) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.tx(func(tx *sql.Tx) error {
		x, err := loadExecution(tx, old.SessionID)
		if err != nil || x == nil {
			return err
		}
		return r.finishStop(tx, x)
	})
}

func (r *Runtime) finishStop(tx *sql.Tx, x *execution) error {
	var failure error = x.StopError
	if x.StopError == nil {
		failure = errInterrupted
	}
	if err := settleAttempt(tx, x, "cancelled", failure, nil, true); err != nil {
		return err
	}
	if err := closeOpenCalls(tx, x.SessionID); err != nil {
		return err
	}
	status, kind, code, msg := "cancelled", EventCompactionCancelled, ErrorCode(""), ""
	if x.StopReason == "failed" {
		status, kind = "failed", EventCompactionFailed
	}
	if x.StopError != nil {
		code, msg = x.StopError.Code, x.StopError.Message
	}
	if x.CompactionID != "" {
		if _, err := tx.ExecContext(bg, `UPDATE compactions SET state=?,error=?,ended_at=? WHERE id=?`, status, msg, nowMillis(), x.CompactionID); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(bg, `SELECT model FROM compactions WHERE id=?`, x.CompactionID).Scan(&raw); err != nil {
			return err
		}
		model, err := decodeOpModel(raw)
		if err != nil {
			return err
		}
		p := CompactionPayload{Model: model.Model, Account: model.Account, Automatic: x.TurnID != "", Code: code, Error: msg, ProviderFailure: providerFailureOf(failure)}
		if err := appendEvent(tx, x.event(kind, p)); err != nil {
			return err
		}
	}
	if x.TurnID != "" {
		if _, err := tx.ExecContext(bg, `UPDATE turns SET status=?,reason=?,error=?,ended_at=? WHERE id=?`, status, x.StopReason, msg, nowMillis(), x.TurnID); err != nil {
			return err
		}
		if err := appendEvent(tx, x.event("turn."+status, TurnEndedPayload{Reason: x.StopReason, Code: code, Error: msg, ProviderFailure: providerFailureOf(failure)})); err != nil {
			return err
		}
	}
	s, err := loadSession(tx, x.SessionID)
	if err != nil {
		return err
	}
	if err := pauseIdle(tx, s, true, x.StopReason); err != nil {
		return err
	}
	_, err = tx.ExecContext(bg, `DELETE FROM executions WHERE session_id=?`, x.SessionID)
	return err
}

// completeExecution includes the follow-up acceptance, even during Close.
// Closing prevents its launch, not preservation of already accepted guidance.
func (r *Runtime) completeExecution(tx *sql.Tx, x *execution) error {
	if x.Action == actionStop {
		return saveExecution(tx, x)
	}
	if x.TurnID != "" {
		if _, err := tx.ExecContext(bg, `UPDATE turns SET status='completed',ended_at=? WHERE id=?`, nowMillis(), x.TurnID); err != nil {
			return err
		}
		if err := appendEvent(tx, x.event(EventTurnCompleted, TurnEndedPayload{})); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(bg, `DELETE FROM executions WHERE session_id=?`, x.SessionID); err != nil {
		return err
	}
	source := "follow_up"
	if x.TurnID == "" {
		source = "steering"
	}
	return r.handoffGuidance(tx, x.SessionID, source)
}

func (r *Runtime) handoffGuidance(tx *sql.Tx, sid, source string) error {
	s, err := loadSession(tx, sid)
	if err != nil {
		return err
	}
	n, err := pendingCount(tx, sid)
	if err != nil {
		return err
	}
	if !s.paused && n > 0 {
		// A catalog edit may make the next turn inadmissible. Preserve the
		// completed result and pending guidance rather than rolling them back.
		if _, err := r.selectWorkModels(s); err != nil {
			return pauseIdle(tx, s, true, "Next turn needs settings repair: "+err.Error())
		}
		_, err = r.startTurn(tx, s, userInput{}, source)
	}
	return err
}

func (r *Runtime) reconcileExecutions(tx *sql.Tx) error {
	var ids []string
	if err := queryRows(tx, func(rows *sql.Rows) error { var id string; err := rows.Scan(&id); ids = append(ids, id); return err }, `SELECT session_id FROM executions`); err != nil {
		return err
	}
	for _, id := range ids {
		x, err := loadExecution(tx, id)
		if err != nil {
			return err
		}
		if err := validateReferences(tx, x); err != nil {
			return err
		}
		changed := x.Progress.InFlight
		if err := settleAttempt(tx, x, "interrupted", fmt.Errorf("runtime process stopped"), nil, true); err != nil {
			return err
		}
		callsChanged, err := r.reconcileCalls(tx, x)
		if err != nil {
			return err
		}
		changed = changed || callsChanged
		if x.Action == actionStop {
			if err := r.finishStop(tx, x); err != nil {
				return err
			}
			continue
		}
		unprepare(x)
		if err := saveExecution(tx, x); err != nil {
			return err
		}
		if changed {
			if err := appendEvent(tx, x.event(EventOperationSuspended, map[string]string{"reason": "recovery"})); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateReferences(q querier, x *execution) error {
	if x.CompactionID != "" {
		var parent string
		if err := q.QueryRowContext(bg, `SELECT COALESCE(turn_id,'') FROM compactions WHERE id=?`, x.CompactionID).Scan(&parent); err != nil {
			return err
		}
		if parent != x.TurnID {
			return fmt.Errorf("execution references a compaction belonging to another operation")
		}
	}
	for _, op := range []struct{ id, table, column string }{{x.TurnID, "turns", "status"}, {x.CompactionID, "compactions", "state"}} {
		if op.id == "" {
			continue
		}
		var state, sid string
		if err := q.QueryRowContext(bg, "SELECT "+op.column+",session_id FROM "+op.table+" WHERE id=?", op.id).Scan(&state, &sid); err != nil {
			return err
		}
		if state != "running" || sid != x.SessionID {
			return fmt.Errorf("execution references non-running or foreign operation %s", op.id)
		}
	}
	return nil
}
