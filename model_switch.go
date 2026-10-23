package sam

import (
	"context"
	"database/sql"
	"fmt"
)

// SwitchModelRequest explicitly selects the main model/account for future work
// in the same chat. Only the target session must be fully idle, including
// stopping workers and follow-ups. Past history and turn settings
// remain unchanged; old reasoning/tool protocol items are projected as portable
// text after the handoff. No inference occurs. Reasoning options reset to the
// provider defaults; other session settings, including compaction, are retained.
type SwitchModelRequest struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Account   string `json:"account"`
}

// SwitchModel records the handoff, binding, override choices and receipt in one
// transaction. Retries of an accepted request never repeat or undo a later switch.
func (r *Runtime) SwitchModel(ctx context.Context, req SwitchModelRequest) (SessionSettings, error) {
	var out SessionSettings
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if !r.configMu.TryLock() {
		return out, errorf(CodeConflict, "configuration preparation is in progress")
	}
	defer r.configMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.mutate(req.RequestID, "switch_model", req, &out, func(tx *sql.Tx) error {
		s, err := loadSession(tx, req.SessionID)
		if err != nil {
			return err
		}
		if s.phase != PhaseIdle || r.workers[s.id] != nil {
			return errBusy
		}
		previous, previousAccount := s.cfg.Model, s.cfg.Account
		s.overrides.Model, s.overrides.Account = new(req.Model), new(req.Account)
		s.overrides.ReasoningEffort, s.overrides.ReasoningSummary = new(""), new(false)
		s.cfg = s.overrides.apply(r.cfg.Defaults)
		if err := r.validateSessionConfig(s.cfg); err != nil {
			return err
		}
		m, err := r.resolve(req.Model)
		if err != nil {
			return err
		}
		old := s.binding
		binding := m.binding()
		if err := tx.QueryRowContext(bg, "SELECT COALESCE(MAX(seq),0) FROM history WHERE session_id=?", s.id).Scan(&s.modelHistoryStart); err != nil {
			return err
		}
		// The boundary is monotonic: even switching back does not revive opaque
		// items from an older binding/account generation.
		_, err = tx.ExecContext(bg, `UPDATE sessions SET bound_model=?,bound_provider=?,bound_base_url=?,bound_model_id=?,bound_api=?,model_history_start=? WHERE id=?`, binding.Model, binding.Provider, binding.BaseURL, binding.ModelID, binding.API, s.modelHistoryStart, s.id)
		if err != nil {
			return err
		}
		s.updatedAt = nowMillis()
		if err := savePrepared(tx, s); err != nil {
			return err
		}
		text := fmt.Sprintf("[Model changed] Future work uses %s with account %q (previously %s with account %q). Earlier tool calls/results and exposed reasoning summaries are read-only text context; opaque reasoning is not forwarded.", req.Model, req.Account, previous, previousAccount)
		if _, err := appendHistory(tx, s.id, "", kindNote, "", userMessage(text)); err != nil {
			return err
		}
		out = SessionSettings{Effective: s.cfg.withDefaults(), Overrides: s.overrides, Resources: s.resources}
		if err := appendEvent(tx, event(s.id, EventModelChanged, map[string]any{"previous": old, "previous_account": previousAccount, "binding": binding, "account": req.Account})); err != nil {
			return err
		}
		return appendEvent(tx, event(s.id, EventConfigChanged, out))
	})
	return out, err
}
