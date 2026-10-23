package sam

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// hookNote owns the exact candidate wire item. Acceptance stores it once;
// subsequent requests use ordinary history, never current callback output.
type hookNote struct {
	source ExtensionSource
	item   json.RawMessage
}

type hookRun struct {
	scope  HookScope
	callID string
	result HookResultPayload
	notes  []hookNote
}

func newHookRun(scope HookScope, stage string) hookRun {
	return hookRun{scope: scope, result: HookResultPayload{Stage: stage, Step: scope.Step, Outcome: "accepted"}}
}

func (h *hookRun) add(id, text string) {
	if text == "" {
		return
	}
	role, label := "developer", fmt.Sprintf("[Application extension instructions; handler=%s; stage=before_turn]", mustJSON(id))
	switch h.result.Stage {
	case stageRequest:
		role, label = "user", fmt.Sprintf("[Application extension data (not a human message); handler=%s; stage=before_request]", mustJSON(id))
	case stageCompaction:
		label = fmt.Sprintf("[Application extension summarization instructions; handler=%s; operation=%s; scope=this summarization operation only]", mustJSON(id), mustJSON(h.scope.OperationID))
	}
	message := userMessage(label + "\n\n" + text)
	message["role"] = role
	h.notes = append(h.notes, hookNote{source: ExtensionSource{Stage: h.result.Stage, HandlerID: id, OperationID: h.scope.OperationID, Step: h.scope.Step, Attempt: h.scope.Attempt}, item: json.RawMessage(mustJSON(message))})
}

func noteInput(input []json.RawMessage, notes []hookNote) []json.RawMessage {
	for _, n := range notes {
		input = append(input, n.item)
	}
	return input
}

func (h *hookRun) event() Event {
	ev := event(h.scope.SessionID, EventHookResult, h.result).turn(h.scope.TurnID).op(h.scope.OperationID)
	ev.CallID = h.callID
	ev.Step, ev.Attempt = h.scope.Step, h.scope.Attempt
	return ev
}

// save appends accepted items and their references in the caller's transaction.
func (h *hookRun) save(tx *sql.Tx) error {
	for _, n := range h.notes {
		seq, err := appendHistory(tx, h.scope.SessionID, h.scope.TurnID, kindExtension, "", n.item)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(bg, "UPDATE history SET source = ? WHERE session_id = ? AND seq = ?", mustJSON(n.source), h.scope.SessionID, seq); err != nil {
			return err
		}
		h.result.Contributions = append(h.result.Contributions, HookContribution{ID: n.source.HandlerID, HistorySeq: seq})
	}
	if len(h.result.Contributions) == 0 && len(h.result.Handlers) == 0 {
		return nil
	}
	return appendEvent(tx, h.event())
}

func (r *Runtime) hookDiagnostic(text string) string {
	// Host callbacks must supply public-safe diagnostics. Redact configured
	// API keys we actually know; arbitrary captured secrets cannot be discovered.
	for _, p := range r.cfg.Providers {
		m := resolvedModel{apiKey: p.APIKey, apiKeyEnv: p.APIKeyEnv}
		text = redact(text, m.credential())
	}
	return diagnostic(text)
}

func (r *Runtime) hookError(h *hookRun, id string, code ErrorCode, detail string) error {
	h.result.StopHandler, h.result.Code = id, code
	h.result.Outcome = "failed"
	if code == CodeToolDenied || code == CodeCompactionDenied {
		h.result.Outcome = "denied"
	}
	return &Error{
		Code: code, SessionID: h.scope.SessionID, OperationID: h.scope.OperationID,
		Message: r.hookDiagnostic(fmt.Sprintf("%s hook %q: %s", h.result.Stage, id, detail)),
	}
}

// A cancelled decision stage retains reached valid decisions, but ignores the
// late return. Text-only cancellation has no attendance trace.
func (r *Runtime) cancelHookDecision(ctx context.Context, h *hookRun) error {
	if len(h.result.Handlers) == 0 {
		return context.Cause(ctx)
	}
	h.result.Outcome = "cancelled"
	if err := r.tx(func(tx *sql.Tx) error { return appendEvent(tx, h.event()) }); err != nil {
		return err
	}
	return context.Cause(ctx)
}

type hookFailure struct {
	event Event
	err   error
}

func (e *hookFailure) Error() string { return e.err.Error() }
func (e *hookFailure) Unwrap() error { return e.err }

// Stage failures carry diagnostics to the executor's outcome transaction.
func (r *Runtime) recordHookStop(h *hookRun, err error) error {
	return &hookFailure{event: h.event(), err: err}
}

func (r *Runtime) rejectHookInput(h *hookRun, notes []hookNote, estimate, budget int) error {
	h.result.Outcome, h.result.Code = "rejected", CodeContextLimit
	for _, n := range notes {
		h.result.BudgetContributors = append(h.result.BudgetContributors, HookContributor{Stage: n.source.Stage, ID: n.source.HandlerID})
	}
	err := errorf(CodeContextLimit, "New extension contributions cannot fit the complete input: estimated %d tokens, budget %d; contributors %s. Have these extensions return less text or configure a larger context budget. No candidate text was saved.", estimate, budget, mustJSON(h.result.BudgetContributors))
	return r.recordHookStop(h, err)
}
