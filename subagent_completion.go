package sam

import (
	"database/sql"
	"fmt"
)

// notifySubagent runs only at quiescence, after steering handoff. The latest
// terminal turn identifies the outcome; the marker, ordinary steering receipt,
// queued notice and optional parent follow-up all commit together. It returns
// only a newly accepted parent's worker ID to launch AFTER commit under mu.
func (r *Runtime) notifySubagent(tx *sql.Tx, sid string) (string, error) {
	var parent, notified, name string
	err := tx.QueryRowContext(bg, `SELECT COALESCE(parent_id,''),notified_turn_id,name
  FROM sessions WHERE id=?`, sid).Scan(&parent, &notified, &name)
	if err != nil || parent == "" {
		return "", err
	}
	x, err := loadExecution(tx, sid)
	if err != nil || x != nil {
		return "", err
	}
	t, err := lastSubagentTurn(tx, sid)
	if err != nil || t == nil {
		return "", err
	}
	if t.EndedAt == nil || t.ID == notified {
		return "", nil
	}
	text := fmt.Sprintf("[notification] Subagent %s %s. Session: %s. "+
		"Read its history for the result and events for diagnostic details.", truncate(name, 120), t.Status, sid)
	req := SteerRequest{
		RequestID: "subagent-notice/" + sid + "/" + t.ID,
		SessionID: parent,
		Text:      text,
	}
	var ack SteerAck
	saved, hash, err := dedupe(tx, req.RequestID, "steer", req)
	if err != nil {
		return "", err
	}
	if saved == nil {
		ack, err = r.acceptSteering(tx, req, true)
		if err != nil {
			return "", err
		}
		if err := saveRequest(tx, req.RequestID, "steer", hash, ack); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(bg, `UPDATE sessions SET notified_turn_id=? WHERE id=?`, t.ID, sid); err != nil {
		return "", err
	}
	if ack.TurnID != "" {
		return parent, nil
	}
	return "", nil
}

func (r *Runtime) launchNotificationParents(ids []string) {
	if r.closing || r.broken.Load() != nil {
		return
	}
	for _, id := range ids {
		if r.workers[id] == nil {
			r.launch(id, false)
		}
	}
}

// Recovery closes the crash window between a child's terminal commit and its
// worker's final quiescence check. Newly accepted parent turns remain suspended
// until Resume, exactly like all other recovered work. No Open-time inference.
func (r *Runtime) recoverSubagents(tx *sql.Tx) error {
	var ids []string
	if err := queryRows(tx, func(rows *sql.Rows) error {
		var id string
		err := rows.Scan(&id)
		ids = append(ids, id)
		return err
	}, `SELECT id FROM sessions WHERE parent_id IS NOT NULL
  AND id NOT IN (SELECT session_id FROM executions)`); err != nil {
		return err
	}
	for _, id := range ids {
		// A previously visited child's notice may have accepted this parent's
		// follow-up already. The initial quiescent listing is not a reservation.
		x, err := loadExecution(tx, id)
		if err != nil {
			return err
		}
		if x != nil {
			continue
		}
		s, err := loadSession(tx, id)
		if err != nil {
			return err
		}
		n, err := pendingCount(tx, id)
		if err != nil {
			return err
		}
		if _, err := r.resolveBound(s); err != nil && !s.paused && n > 0 {
			// Preserve guidance that cannot yet run; it is not a done task.
			continue
		}
		if err := r.handoffGuidance(tx, id, "steering"); err != nil {
			return err
		}
		if _, err := r.notifySubagent(tx, id); err != nil {
			return err
		}
	}
	return nil
}
