package sam

import (
	"context"
	"database/sql"
	"time"
)

// preparedRequest lives only on a worker. Neither bodies nor private callback
// candidates are serialized; restart is a new preparation using current config.
type preparedRequest struct {
	request          modelRequest
	body             []byte
	mainConfig       SessionConfig
	mainInstructions string
	summary          *summaryPreparation
}

func waitDeadline(ctx context.Context, ms int64) error {
	if delay := time.Until(fromMillis(ms)); ms != 0 && delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	return context.Cause(ctx)
}

func providerBudget(p modelProgress) error {
	if p.Attempts >= 4 {
		return errorf(CodeProviderError, "provider attempts exhausted: all four reserved attempts for this request were consumed, including interrupted attempts")
	}
	return nil
}

func prepareBudget(ctx context.Context, x *execution) error {
	if err := providerBudget(x.Progress); err != nil {
		return err
	}
	return waitDeadline(ctx, x.Progress.NotBefore)
}

func (r *Runtime) collectTurn(ctx context.Context, x *execution, t *turnRun) error {
	if x.TurnHookDone || t.turnCollected {
		return nil
	}
	// BeforeTurn is attributed to the turn, not the prospective model attempt.
	t.step, t.attempt = 0, 0
	h, err := r.beforeTurn(ctx, t)
	if err != nil {
		return err
	}
	t.pendingTurn, t.turnCollected = h, true
	return nil
}

func (r *Runtime) drainGuidance(ctx context.Context, x *execution) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return r.tx(func(tx *sql.Tx) error {
		current, err := loadExecution(tx, x.SessionID)
		if err != nil {
			return err
		}
		if current == nil || current.Action == actionStop {
			return errInterrupted
		}
		return applyPending(tx, x.SessionID, x.TurnID)
	})
}

func (r *Runtime) prepareMain(ctx context.Context, x *execution, w *worker) error {
	if err := prepareBudget(ctx, x); err != nil {
		return err
	}
	t := w.main
	if err := r.collectTurn(ctx, x, t); err != nil {
		return err
	}
	t.step, t.attempt = x.Step, x.Progress.Attempts+1
	if err := r.drainGuidance(ctx, x); err != nil {
		return err
	}
	s, view, err := r.loadView(x.SessionID)
	if err != nil {
		return err
	}
	if !x.CompactionChecked {
		selected, err := r.checkCompaction(ctx, x, w, s, view)
		if err != nil || selected {
			return err
		}
	}
	h, err := r.beforeRequest(ctx, t)
	if err != nil {
		return err
	}
	if err := r.drainGuidance(ctx, x); err != nil {
		return err
	}
	_, view, err = r.loadView(x.SessionID)
	if err != nil {
		return err
	}
	t.initiator, err = r.canonicalInitiator(x.SessionID)
	if err != nil {
		return err
	}
	notes := append(append([]hookNote(nil), t.pendingTurn.notes...), h.notes...)
	input := noteInput(view.input(), notes)
	if t.model.NoImages {
		input = withoutImages(input)
	}
	if len(notes) > 0 {
		need, limit := estimateRequest(t.instructions, input, t.toolList), budgets(t.model, t.cfg).input
		if need > limit {
			return r.rejectHookInput(&h, notes, need, limit)
		}
	}
	req := modelRequest{sessionID: x.SessionID, model: t.model, instructions: t.instructions, input: input, outcomes: view.callOutcomes(), tools: t.toolList, effort: t.cfg.ReasoningEffort, summary: t.cfg.ReasoningSummary, initiator: t.initiator, maxTokens: t.model.outputReserve(t.cfg.Compaction.OutputReserve)}
	body, err := r.client.body(req)
	if err != nil {
		return err
	}
	p := &preparedRequest{request: req, body: body, mainConfig: t.cfg, mainInstructions: t.instructions}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	err = r.tx(func(tx *sql.Tx) error {
		current, err := loadExecution(tx, x.SessionID)
		if err != nil {
			return err
		}
		if current == nil || current.Action == actionStop {
			return errInterrupted
		}
		if !current.TurnHookDone {
			if err := t.pendingTurn.save(tx); err != nil {
				return err
			}
		}
		if err := h.save(tx); err != nil {
			return err
		}
		current.TurnHookDone, current.Action = true, actionRequest
		return saveExecution(tx, current)
	})
	if err == nil {
		w.prepared = p
		t.pendingTurn = hookRun{}
	}
	return err
}

func (r *Runtime) checkCompaction(ctx context.Context, x *execution, w *worker, s *sessionRow, view *contextView) (bool, error) {
	t := w.main
	b := budgets(t.model, t.cfg)
	need := estimateRequest(t.instructions, noteInput(view.input(), t.pendingTurn.notes), t.toolList)
	selected := !t.cfg.Compaction.DisableAuto && need > b.trigger && view.selectCut(b.recent) > 0
	var spec *compactionSpec
	var err error
	if selected {
		spec = &compactionSpec{id: newID("cmp"), model: w.models.summary, automatic: true}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return false, context.Cause(ctx)
	}
	err = r.tx(func(tx *sql.Tx) error {
		current, err := loadExecution(tx, x.SessionID)
		if err != nil {
			return err
		}
		if current == nil || current.Action == actionStop {
			return errInterrupted
		}
		current.CompactionChecked = true
		if selected {
			if err := beginCompaction(tx, s.id, x.TurnID, spec); err != nil {
				return err
			}
			current.CompactionID, current.Action, current.Progress = spec.id, actionCompactPrepare, freshProgress()
		}
		if err := saveExecution(tx, current); err != nil {
			return err
		}
		*x = *current
		return nil
	})
	return selected, err
}
