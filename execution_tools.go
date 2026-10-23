package sam

import (
	"context"
	"database/sql"
	"fmt"
)

const uncertainWarning = "Warning: a previous interrupted attempt may already have produced effects."

func executionPhase(x *execution, owned bool) string {
	if x == nil {
		if owned {
			return PhaseStopping
		}
		return PhaseIdle
	}
	if x.Action == actionStop {
		return PhaseStopping
	}
	if !owned {
		return PhaseSuspended
	}
	if x.CompactionID != "" {
		return PhaseCompacting
	}
	return PhaseRunning
}

func loadCalls(q querier, x *execution) ([]plannedCall, error) {
	var calls []plannedCall
	err := queryRows(q, func(rows *sql.Rows) error {
		var c plannedCall
		err := rows.Scan(&c.CallID, &c.Name, &c.Arguments, &c.Status, &c.Attempt, &c.Uncertain, &c.Namespace, &c.DispatchTarget, &c.ReplayUnsafe)
		calls = append(calls, c)
		return err
	}, `SELECT call_id,name,arguments,status,attempts,had_uncertain_attempt,namespace,COALESCE(dispatch_target,name),replay_unsafe FROM tool_calls
 WHERE turn_id=? AND step=? AND history_seq IS NULL ORDER BY idx`, x.TurnID, x.Step)
	return calls, err
}

func (r *Runtime) executeBatch(ctx context.Context, x *execution, w *worker) error {
	calls, err := loadCalls(r.db, x)
	if err != nil {
		return r.storageFailed(err)
	}
	pending := calls[:0]
	for _, c := range calls {
		if c.Status == "planned" {
			c.Attempt++
			pending = append(pending, c)
		}
	}
	w.main.step = x.Step
	if err := r.runBatch(ctx, w.main, pending); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tx(func(tx *sql.Tx) error {
		current, err := loadExecution(tx, x.SessionID)
		if err != nil || current == nil {
			return err
		}
		if current.Action == actionStop {
			return nil
		}
		var unfinished int
		if err := tx.QueryRowContext(bg, `SELECT COUNT(*) FROM tool_calls WHERE turn_id=? AND step=? AND status IN ('planned','started')`, x.TurnID, x.Step).Scan(&unfinished); err != nil {
			return err
		}
		if unfinished != 0 {
			return fmt.Errorf("batch has unfinished calls without cancellation")
		}
		if err := assembleCalls(tx, x.SessionID, false); err != nil {
			return err
		}
		current.Step++
		current.Action = actionPrepare
		current.Progress = freshProgress()
		current.CompactionChecked = false
		return saveExecution(tx, current)
	})
}

// Reconciliation settles only carried-over unsafe calls. Replay-allowed calls
// remain pending for executeBatch; known results and explicit stops keep their
// existing paths. Reopening again cannot duplicate an already-saved skip.
func (r *Runtime) reconcileCalls(tx *sql.Tx, x *execution) (bool, error) {
	calls, err := loadCalls(tx, x)
	if err != nil {
		return false, err
	}
	changed := false
	for _, c := range calls {
		if c.Status != "planned" && c.Status != "started" {
			continue
		}
		if c.Status == "started" {
			if err := interruptCall(tx, x, c, true); err != nil {
				return false, err
			}
			c.Uncertain = true
			changed = true
		}
		if x.Action == actionStop || !(c.ReplayUnsafe || r.tools[c.DispatchTarget].ReplayUnsafe) {
			continue
		}
		status, detail := "not_executed", "This call never started."
		if c.Uncertain {
			status, detail = "uncertain", "An earlier attempt may already have produced effects; its outcome was not saved."
		}
		result := toolError("recovery_skipped", fmt.Sprintf(
			"Recovery skipped this replay-unsafe call. %s Inspect current state before issuing a fresh call; do not blindly repeat a server shutdown command. History reference: turn %s, step %d, call %s.",
			detail, x.TurnID, x.Step, c.CallID), 0)
		if err := saveCallResult(tx, x.SessionID, x.TurnID, x.Step, c, status, result); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func interruptCall(tx *sql.Tx, x *execution, c plannedCall, invoked bool) error {
	if _, err := tx.ExecContext(bg, `UPDATE tool_calls SET status='planned',had_uncertain_attempt=MAX(had_uncertain_attempt,?),started_at=NULL WHERE turn_id=? AND call_id=?`, invoked, x.TurnID, c.CallID); err != nil {
		return err
	}
	if !invoked {
		return nil
	}
	e := x.event(EventToolAttemptInterrupted, map[string]string{"warning": uncertainWarning})
	e.CallID, e.Attempt = c.CallID, c.Attempt
	return appendEvent(tx, e)
}

func (r *Runtime) suspendCall(t *turnRun, c plannedCall, invoked bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tx(func(tx *sql.Tx) error {
		x, err := loadExecution(tx, t.sessionID)
		if err != nil {
			return err
		}
		if x == nil {
			return fmt.Errorf("tool outcome without execution")
		}
		return interruptCall(tx, x, c, invoked)
	})
}

func uncertainResult(c plannedCall, status, result string, limit int) string {
	if !c.Uncertain {
		return result
	}
	// Failure explanations are outside the capture limit. They must never lose
	// this warning, even when the configured limit is smaller than the warning.
	if status != "completed" {
		return uncertainWarning + "\n" + result
	}
	result = uncertainWarning + "\n" + result
	if len(result) > limit {
		return toolError("tool_output_limit", uncertainWarning+" "+limitMessage(c.Name, limit), limit)
	}
	return result
}
