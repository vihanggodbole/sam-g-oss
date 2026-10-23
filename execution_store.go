package sam

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type executionAction string

const (
	actionPrepare        executionAction = "prepare"
	actionRequest        executionAction = "request"
	actionBatch          executionAction = "batch"
	actionCompactPrepare executionAction = "compact_prepare"
	actionCompactRequest executionAction = "compact_request"
	actionStop           executionAction = "stop"
)

type modelProgress struct {
	Attempts  int    `json:"attempts_used"`
	NotBefore int64  `json:"not_before_ms"`
	Refresh   string `json:"refresh"`
	InFlight  bool   `json:"in_flight"`
}

func freshProgress() modelProgress { return modelProgress{Refresh: "available"} }

type execution struct {
	SessionID, TurnID, CompactionID string
	Action                          executionAction
	Step                            int
	TurnHookDone, CompactionChecked bool
	Progress                        modelProgress
	StopReason                      string
	StopError                       *Error
}

func loadExecution(q querier, sid string) (*execution, error) {
	x := &execution{SessionID: sid}
	var progress, stop string
	err := q.QueryRowContext(bg, `SELECT action, COALESCE(turn_id,''), COALESCE(compaction_id,''), step,
 turn_hook_done, compaction_checked, model_progress, COALESCE(stop_reason,''), COALESCE(stop_error,'')
 FROM executions WHERE session_id = ?`, sid).Scan(&x.Action, &x.TurnID, &x.CompactionID, &x.Step,
		&x.TurnHookDone, &x.CompactionChecked, &progress, &x.StopReason, &stop)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(progress), &x.Progress); err != nil {
		return nil, err
	}
	if stop != "" {
		if err := json.Unmarshal([]byte(stop), &x.StopError); err != nil {
			return nil, err
		}
	}
	if err := x.validate(); err != nil {
		return nil, err
	}
	return x, nil
}

func (x *execution) validate() error {
	bad := func() error { return fmt.Errorf("invalid execution for session %s", x.SessionID) }
	if x.TurnID == "" && (x.CompactionID == "" || x.Step != 0) {
		return bad()
	}
	if x.TurnID != "" && x.Step < 1 {
		return bad()
	}
	switch x.Action {
	case actionPrepare, actionRequest, actionBatch:
		if x.TurnID == "" || x.CompactionID != "" {
			return bad()
		}
	case actionCompactPrepare, actionCompactRequest:
		if x.CompactionID == "" {
			return bad()
		}
	case actionStop:
		if x.StopReason != "interrupted" && x.StopReason != "failed" {
			return bad()
		}
	default:
		return bad()
	}
	if !x.Progress.valid(x.Action) {
		return bad()
	}
	if x.Action != actionStop && (x.StopReason != "" || x.StopError != nil) {
		return bad()
	}
	return nil
}

func (p modelProgress) valid(action executionAction) bool {
	if p.Attempts < 0 || p.Attempts > 4 || p.NotBefore < 0 {
		return false
	}
	if p.Refresh != "available" && p.Refresh != "pending" && p.Refresh != "used" {
		return false
	}
	if p.InFlight && (p.Attempts == 0 || (action != actionRequest && action != actionCompactRequest && action != actionStop)) {
		return false
	}
	return action != actionBatch || p.Attempts > 0
}

func saveExecution(q querier, x *execution) error {
	if err := x.validate(); err != nil {
		return err
	}
	var stop any
	if x.StopError != nil {
		stop = mustJSON(x.StopError)
	}
	_, err := q.ExecContext(bg, `INSERT INTO executions
 (session_id,action,turn_id,compaction_id,step,turn_hook_done,compaction_checked,model_progress,stop_reason,stop_error)
 VALUES (?, ?, NULLIF(?,''), NULLIF(?,''), ?, ?, ?, ?, NULLIF(?,''), ?)
 ON CONFLICT(session_id) DO UPDATE SET action=excluded.action,turn_id=excluded.turn_id,compaction_id=excluded.compaction_id,
 step=excluded.step,turn_hook_done=excluded.turn_hook_done,compaction_checked=excluded.compaction_checked,
 model_progress=excluded.model_progress,stop_reason=excluded.stop_reason,stop_error=excluded.stop_error`,
		x.SessionID, x.Action, x.TurnID, x.CompactionID, x.Step, x.TurnHookDone, x.CompactionChecked, mustJSON(x.Progress), x.StopReason, stop)
	return err
}

func (x *execution) event(kind string, payload any) Event {
	e := event(x.SessionID, kind, payload).turn(x.TurnID).op(x.CompactionID)
	e.Step, e.Attempt = x.Step, x.Progress.Attempts
	return e
}

// settleAttempt is shared by crash recovery, clean suspension and terminal
// failure. Only committed deltas are quoted, once, guarded by in_flight.
// Live callers must drain their producer and batcher before entering here.
func settleAttempt(tx *sql.Tx, x *execution, outcome string, failure error, usage *Usage, partial bool) error {
	if !x.Progress.InFlight {
		return nil
	}
	if partial && x.CompactionID == "" {
		var text, reasoning strings.Builder
		err := queryRows(tx, func(rows *sql.Rows) error {
			var kind, raw string
			if err := rows.Scan(&kind, &raw); err != nil {
				return err
			}
			var d DeltaPayload
			if err := json.Unmarshal([]byte(raw), &d); err != nil {
				return err
			}
			if kind == EventReasoningDelta {
				reasoning.WriteString(d.Text)
			} else {
				text.WriteString(d.Text)
			}
			return nil
		}, `SELECT kind,payload FROM events WHERE session_id=? AND turn_id=? AND operation_id='' AND step=? AND attempt=?
  AND kind IN ('text.delta','refusal.delta','reasoning.delta') ORDER BY seq`, x.SessionID, x.TurnID, x.Step, x.Progress.Attempts)
		if err != nil {
			return err
		}
		p := partialOutput{text.String(), reasoning.String()}
		if !p.empty() {
			if err := appendPartialNote(tx, x.SessionID, x.TurnID, p); err != nil {
				return err
			}
		}
	}
	p := ModelAttemptEndedPayload{Outcome: outcome, Usage: usage}
	if failure != nil {
		p.Code, p.Message = ErrorCodeOf(failure), failure.Error()
		p.ProviderFailure = providerFailureOf(failure)
	}
	if err := appendEvent(tx, x.event(EventModelAttemptEnded, p)); err != nil {
		return err
	}
	x.Progress.InFlight = false
	return nil
}

// ModelAttemptStartedPayload identifies one durably reserved provider attempt.
type ModelAttemptStartedPayload struct {
	Model      string `json:"model"`
	Account    string `json:"account,omitempty"`
	Compaction bool   `json:"compaction"`
	Reason     string `json:"reason"`
}

// ModelAttemptEndedPayload ends one attempt, not its logical operation.
type ModelAttemptEndedPayload struct {
	Outcome         string           `json:"outcome"`
	Code            ErrorCode        `json:"code,omitempty"`
	Message         string           `json:"message,omitempty"`
	Usage           *Usage           `json:"usage"`
	ProviderFailure *ProviderFailure `json:"provider_failure,omitempty"`
}

func finishAttempt(tx *sql.Tx, x *execution, usage Usage) error {
	if !x.Progress.InFlight {
		return fmt.Errorf("completed response without reserved attempt")
	}
	if err := appendEvent(tx, x.event(EventModelAttemptEnded, ModelAttemptEndedPayload{Outcome: "completed", Usage: &usage})); err != nil {
		return err
	}
	x.Progress.InFlight = false
	return nil
}

func stopExecution(x *execution, reason string, err error) {
	if x.Action == actionStop {
		return
	}
	x.Action, x.StopReason = actionStop, reason
	if err != nil {
		x.StopError = &Error{Code: "internal", Message: err.Error()}
		var safe *Error
		if errors.As(err, &safe) {
			*x.StopError = *safe
		}
	}
}
