package sam

import (
	"context"
	"database/sql"
	"time"
)

// StartTurnRequest starts a turn with a new prompt on an idle session.
// Pending (including paused) guidance is applied first, in arrival order. Every
// mutation requires a RequestID; retry an uncertain call with the same ID and
// identical arguments to recover its original acknowledgement.
type StartTurnRequest struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
}

// ContinueRequest explicitly resumes a paused or stopped session with its
// pending guidance and an optional new prompt. Its RequestID follows the same
// retry rule as other mutations: reuse it only for the identical request.
type ContinueRequest struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt,omitempty"`
}

// TurnAck acknowledges saved turn work, not its completion. Observe the turn's
// terminal event or inspect it with Runtime.ListTurns.
type TurnAck struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
}

// SteerRequest submits guidance for the session's current or next turn. A
// required RequestID makes retries safe when the acknowledgement is uncertain.
type SteerRequest struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

// SteerAck acknowledges durable guidance. TurnID is set when guidance started
// a new turn on an idle, unpaused session. Paused is true when guidance waits
// for an explicit Continue.
type SteerAck struct {
	SteeringID string `json:"steering_id"`
	TurnID     string `json:"turn_id,omitempty"`
	Paused     bool   `json:"paused"`
}

// SessionRequest identifies a session for Interrupt, ClearSteering, or Compact.
// RequestID is required and deduplicates retries of the same operation.
type SessionRequest struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
}

// Steering is a pending guidance message.
type Steering struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Paused    bool      `json:"paused"`
	CreatedAt time.Time `json:"created_at"`
}

// ClearAck lists the guidance messages that were cleared.
type ClearAck struct {
	Cleared []string `json:"cleared"`
}

// CompactAck acknowledges a manual compaction started in the background.
type CompactAck struct {
	CompactionID string `json:"compaction_id"`
}

// StartTurn durably accepts a prompt on an idle session and returns without
// waiting for the turn. Its RequestID must be unique for a new mutation and
// stable across retries. Once accepted, the turn is owned by the Runtime and
// continues independently of ctx; observe completion through events.
func (r *Runtime) StartTurn(ctx context.Context, req StartTurnRequest) (TurnAck, error) {
	if req.Prompt == "" {
		return TurnAck{}, errorf(CodeInvalidArgument, "prompt is required")
	}
	return r.begin(req.RequestID, "start_turn", req, req.SessionID, req.Prompt, "prompt")
}

// Continue starts a new turn after interruption or failure, applying paused
// guidance before the optional prompt. Suspended original work must instead
// be admitted with Resume or terminally stopped with Interrupt. It returns
// after acceptance; the Runtime owns the resulting work independently of ctx.
func (r *Runtime) Continue(ctx context.Context, req ContinueRequest) (TurnAck, error) {
	return r.begin(req.RequestID, "continue", req, req.SessionID, req.Prompt, "continue")
}

// begin accepts a prompt or continuation. Session state is checked only
// after request deduplication, so an accepted request retried after its
// session was deleted still returns the saved acknowledgement. Neither
// admission nor the worker rereads prompt files; setup/reload prepared them.
func (r *Runtime) begin(requestID, op string, args any, sessionID, promptText, source string) (TurnAck, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ack := TurnAck{SessionID: sessionID}
	fresh, err := r.mutateFresh(requestID, op, args, &ack, func(tx *sql.Tx) error {
		s, err := loadSession(tx, sessionID)
		if err != nil {
			return err
		}
		if s.phase != PhaseIdle || r.workers[s.id] != nil {
			return errorf(CodeConflict, "Session has unfinished work (%s). Resume it (Go Resume or server resume_on_start on restart), or Interrupt and wait before starting another turn.", s.phase)
		}
		if promptText == "" {
			var n int
			if err := tx.QueryRowContext(bg, `SELECT (SELECT COUNT(*) FROM history WHERE session_id = ?1) +
				(SELECT COUNT(*) FROM steering WHERE session_id = ?1 AND status = 'pending')`, s.id).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return errorf(CodeInvalidArgument, "nothing to continue: the session has no history or pending guidance")
			}
		}
		ack.TurnID, err = r.startTurn(tx, s, promptText, source)
		return err
	})
	if fresh {
		r.launch(sessionID, false)
	}
	return ack, err
}

// Steer durably accepts guidance. During work it is applied before the next
// model request; guidance still pending at normal completion starts a follow-up.
// On an idle, unpaused session it starts a turn; on a paused session it waits
// for Continue. Accepted work is owned by the Runtime, not ctx.
func (r *Runtime) Steer(ctx context.Context, req SteerRequest) (SteerAck, error) {
	if req.Text == "" {
		return SteerAck{}, errorf(CodeInvalidArgument, "text is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var ack SteerAck
	fresh, err := r.mutateFresh(req.RequestID, "steer", req, &ack, func(tx *sql.Tx) error {
		var err error
		ack, err = r.acceptSteering(tx, req, false)
		return err
	})
	if fresh && ack.TurnID != "" {
		r.launch(req.SessionID, false)
	}
	return ack, err
}

// acceptSteering is the single queue/follow-up path. Notifications must remain
// durable even if the parent's model is unavailable; explicit user requests keep
// their existing model validation. An unavailable parent retains pending text.
func (r *Runtime) acceptSteering(tx *sql.Tx, req SteerRequest, notification bool) (SteerAck, error) {
	var ack SteerAck
	s, err := loadSession(tx, req.SessionID)
	if err != nil {
		return ack, err
	}
	m, modelErr := r.resolveBound(s)
	if modelErr == nil && s.phase == PhaseIdle && r.workers[s.id] == nil && !s.paused {
		_, modelErr = r.selectWorkModels(s)
	}
	if modelErr != nil && !notification {
		return ack, modelErr
	}
	if modelErr == nil {
		if err := bindModel(tx, s, m); err != nil {
			return ack, err
		}
	}
	ack.SteeringID, ack.Paused = newID("steer"), s.paused
	if _, err := tx.ExecContext(bg, `INSERT INTO steering (id,session_id,seq,text,status,created_at)
	 VALUES (?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM steering WHERE session_id=?),?,'pending',?)`,
		ack.SteeringID, s.id, s.id, req.Text, nowMillis()); err != nil {
		return ack, err
	}
	if err := touchActivity(tx, s.id); err != nil {
		return ack, err
	}
	p := SteeringPayload{SteeringID: ack.SteeringID, Text: req.Text, Paused: s.paused}
	if err := appendEvent(tx, event(s.id, EventSteeringAccepted, p)); err != nil {
		return ack, err
	}
	if modelErr == nil && s.phase == PhaseIdle && r.workers[s.id] == nil && !s.paused {
		ack.TurnID, err = r.startTurn(tx, s, "", "steering")
	}
	return ack, err
}

// Interrupt cancels the session's current turn, follow-up, or compaction and
// pauses pending guidance. On an idle session it only pauses. It does not stop
// the Runtime or unrelated sessions. Owned descendants are stopped too; observe
// settlement through events. A returned nil means the interrupt was saved, not
// that cleanup has finished.
func (r *Runtime) Interrupt(ctx context.Context, req SessionRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	var parents []string
	fresh, err := r.mutateFresh(req.RequestID, "interrupt", req, nil, func(tx *sql.Tx) error {
		if _, err := loadSession(tx, req.SessionID); err != nil {
			return err
		}
		if err := queryRows(tx, func(rows *sql.Rows) error {
			var id string
			err := rows.Scan(&id)
			ids = append(ids, id)
			return err
		}, `WITH RECURSIVE owned(id) AS (SELECT ? UNION ALL
		 SELECT s.id FROM sessions s JOIN owned o ON s.parent_id=o.id) SELECT id FROM owned`, req.SessionID); err != nil {
			return err
		}
		for _, id := range ids {
			if err := r.interruptSession(tx, id); err != nil {
				return err
			}
		}
		// Pause the entire tree before any child can enqueue a parent notice.
		for _, id := range ids {
			parent, err := r.notifySubagent(tx, id)
			if err != nil {
				return err
			}
			if parent != "" {
				parents = append(parents, parent)
			}
		}
		return nil
	})
	if fresh {
		for _, id := range ids {
			if w := r.workers[id]; w != nil {
				w.cancel(errInterrupted)
			}
		}
		r.launchNotificationParents(parents)
	}
	return err
}

func (r *Runtime) interruptSession(tx *sql.Tx, id string) error {
	x, err := loadExecution(tx, id)
	if err != nil {
		return err
	}
	if x != nil {
		stopExecution(x, "interrupted", nil)
		if err := saveExecution(tx, x); err != nil {
			return err
		}
		if r.workers[id] == nil {
			if err := r.finishStop(tx, x); err != nil {
				return err
			}
		}
	}
	if err := setPaused(tx, id, true); err != nil {
		return err
	}
	return appendEvent(tx, event(id, EventSessionPaused, map[string]string{"reason": "interrupted"}))
}

// ListSteering returns pending guidance in arrival order.
func (r *Runtime) ListSteering(ctx context.Context, sessionID string) ([]Steering, error) {
	if err := r.readable(); err != nil {
		return nil, err
	}
	db := r.reader(ctx)
	s, err := loadSession(db, sessionID)
	if err != nil {
		return nil, readErr(ctx, err)
	}
	out := []Steering{}
	err = queryRows(db, func(rows *sql.Rows) error {
		st := Steering{Paused: s.paused}
		var ts int64
		err := rows.Scan(&st.ID, &st.Text, &ts)
		st.CreatedAt = fromMillis(ts)
		out = append(out, st)
		return err
	}, "SELECT id, text, created_at FROM steering WHERE session_id = ? AND status = 'pending' ORDER BY seq", sessionID)
	return out, readErr(ctx, err)
}

// ClearSteering clears all guidance still pending when accepted. Applied
// guidance, the pause flag, and running work are unchanged. The returned IDs
// identify exactly the messages cleared by this request.
func (r *Runtime) ClearSteering(ctx context.Context, req SessionRequest) (ClearAck, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ack := ClearAck{Cleared: []string{}}
	err := r.mutate(req.RequestID, "clear_steering", req, &ack, func(tx *sql.Tx) error {
		if _, err := loadSession(tx, req.SessionID); err != nil {
			return err
		}
		err := queryRows(tx, func(rows *sql.Rows) error {
			var id string
			err := rows.Scan(&id)
			ack.Cleared = append(ack.Cleared, id)
			return err
		}, `UPDATE steering SET status = 'cleared' WHERE session_id = ? AND status = 'pending' RETURNING id`, req.SessionID)
		if err != nil {
			return err
		}
		// This is a metadata change, not conversation activity, but compact
		// workspace summaries must tell clients their guidance counts are stale.
		if len(ack.Cleared) > 0 {
			if _, err := tx.ExecContext(bg, `UPDATE sessions SET updated_at = MAX(updated_at, ?) WHERE id = ?`, nowMillis(), req.SessionID); err != nil {
				return err
			}
		}
		return appendEvent(tx, event(req.SessionID, EventSteeringCleared, SteeringClearedPayload(ack)))
	})
	return ack, err
}

// Compact starts manual compaction on an idle session and returns its operation
// ID without waiting for summarization. It is never queued: a busy session
// returns a conflict. The pause flag is preserved. Settings are checked after
// request deduplication; the worker uses installed prepared prompt contents.
func (r *Runtime) Compact(ctx context.Context, req SessionRequest) (CompactAck, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ack CompactAck
	var spec *compactionSpec
	fresh, err := r.mutateFresh(req.RequestID, "compact", req, &ack, func(tx *sql.Tx) error {
		s, err := loadSession(tx, req.SessionID)
		if err != nil {
			return err
		}
		if s.phase != PhaseIdle || r.workers[s.id] != nil {
			return errorf(CodeConflict, "Session has unfinished work. Resume it, or Interrupt and wait before compacting.")
		}
		if _, err := r.selectWorkModels(s); err != nil {
			return err
		}
		if spec, err = r.resolveCompaction(s, newID("cmp"), false); err != nil {
			return err
		}
		if err := beginCompaction(tx, s.id, "", spec); err != nil {
			return err
		}
		ack.CompactionID = spec.id
		return saveExecution(tx, &execution{SessionID: s.id, CompactionID: spec.id, Action: actionCompactPrepare, Progress: freshProgress()})
	})
	if fresh {
		r.launch(req.SessionID, false)
	}
	return ack, err
}
