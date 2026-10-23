package sam

import (
	"context"
	"encoding/json"
)

// compactionHookInput is reached only after normal selection/preparation and
// base input-fit checks. New notes append beyond the frozen cut, so this
// summary never absorbs its own instructions. Denials remain private operation
// diagnostics, not transient messages in a later conversational request.
func (r *Runtime) compactionHookInput(ctx context.Context, s *sessionRow, spec *compactionSpec, turnID string, view *contextView, cut int, protected *histItem, text string, reserve int) (hookRun, []json.RawMessage, error) {
	input := rawInput(userMessage(text))
	in := BeforeCompactionInput{
		Scope:     HookScope{SessionID: s.id, TurnID: turnID, OperationID: spec.id, Step: spec.step, Attempt: spec.attempt, Workspace: s.workspace, Model: spec.model.Alias, Account: spec.model.accountName()},
		Automatic: spec.automatic, MainModel: s.cfg.Model, Prompt: spec.prompt, PreviousSummary: view.summary,
		FromSeq: view.items[0].seq, ThroughSeq: view.items[cut-1].seq,
		EstimatedInputTokens: estimateRequest(spec.prompt, input, nil), InputBudget: spec.model.ContextTokens - reserve,
	}
	if protected != nil {
		in.ProtectedUserSeq = protected.seq
	}
	h, err := r.beforeCompaction(ctx, in)
	if err != nil {
		return h, nil, err
	}
	input = noteInput(input, h.notes)
	if len(h.notes) != 0 {
		if need := estimateRequest(spec.prompt, input, nil); need > in.InputBudget {
			return h, nil, r.rejectHookInput(&h, h.notes, need, in.InputBudget)
		}
	}
	return h, input, nil
}
