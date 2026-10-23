package sam

import (
	"context"
	"database/sql"
	"strings"
)

type summaryPreparation struct {
	spec      *compactionSpec
	view      *contextView
	cut       int
	protected *histItem
	notes     []hookNote
	main      resolvedModel
	budget    budget
}

func (r *Runtime) prepareSummary(ctx context.Context, x *execution, w *worker) error {
	if err := prepareBudget(ctx, x); err != nil {
		return err
	}
	if w.main != nil {
		if err := r.collectTurn(ctx, x, w.main); err != nil {
			return err
		}
		if err := r.drainGuidance(ctx, x); err != nil {
			return err
		}
	}
	s, view, err := r.loadView(x.SessionID)
	if err != nil {
		return err
	}
	spec := w.compaction
	if spec == nil || spec.id != x.CompactionID {
		spec = &compactionSpec{id: x.CompactionID, model: w.models.summary, automatic: x.TurnID != ""}
		if w.main != nil {
			spec.model, err = w.main.summaryModel, w.main.summaryError
		} else {
			spec.model, err = r.withAccount(spec.model, spec.model.account.name)
		}
		if err != nil {
			return err
		}
		spec.prompt = s.compactionPrompt
		spec.instructions, spec.tools = s.instructions, r.enabledTools(s.cfg)
		w.compaction = spec
	}
	spec.step, spec.attempt = x.Step, x.Progress.Attempts+1
	if w.main != nil {
		spec.pending = w.main.pendingTurn.notes
	}
	main := w.models.main
	b := budgets(main, s.cfg)
	cut := view.selectCut(b.recent)
	if cut == 0 {
		return errorf(CodeConflict, "nothing to compact: the remaining history is within the retained recent-history budget")
	}
	protected, err := precedingUser(r.db, s.id, view.items[cut].seq)
	if err != nil {
		return storageErr(err)
	}
	if protected != nil && protected.seq >= view.items[cut].seq {
		protected = nil
	}
	text := portableText(view, view.items[:cut])
	reserve := spec.model.outputReserve(s.cfg.Compaction.OutputReserve)
	if need := estimateTokens(len(spec.prompt)+len(text)) + reserve; need > spec.model.ContextTokens {
		return errorf(CodeContextLimit, "The selected history exceeds this compaction model's context: an estimated %d tokens (including a %d-token output reserve) for model %q, which allows %d. Choose a model with a larger context window.", need, reserve, spec.model.Alias, spec.model.ContextTokens)
	}
	hook, input, err := r.compactionHookInput(ctx, s, spec, x.TurnID, view, cut, protected, text, reserve)
	if err != nil {
		return err
	}
	initiator := "user"
	if spec.automatic {
		initiator = "agent"
	}
	req := modelRequest{sessionID: x.SessionID, model: spec.model, instructions: spec.prompt, input: input, initiator: initiator, maxTokens: reserve}
	body, err := r.client.body(req)
	if err != nil {
		return err
	}
	p := &preparedRequest{
		request: req, body: body, mainConfig: s.cfg, mainInstructions: s.instructions,
		summary: &summaryPreparation{spec: spec, view: view, cut: cut, protected: protected, notes: hook.notes, main: main, budget: b},
	}
	return r.acceptSummary(ctx, x, w, p, &hook)
}

func (r *Runtime) acceptSummary(ctx context.Context, x *execution, w *worker, p *preparedRequest, hook *hookRun) error {
	spec := p.summary.spec
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	err := r.tx(func(tx *sql.Tx) error {
		current, err := loadExecution(tx, x.SessionID)
		if err != nil {
			return err
		}
		if current == nil || current.Action == actionStop {
			return errInterrupted
		}
		if err := hook.save(tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(bg, `UPDATE compactions SET prompt=? WHERE id=? AND prompt=''`, spec.prompt, spec.id); err != nil {
			return err
		}
		current.Action = actionCompactRequest
		return saveExecution(tx, current)
	})
	if err == nil {
		w.prepared = p
	}
	return err
}

func (p *summaryPreparation) validate(res *modelResult) (*summaryResult, error) {
	text, refusal := outputText(res.output)
	summary := strings.TrimSpace(text)
	if refusal != "" {
		return nil, errorf(CodeProviderError, "the compaction model refused to summarize (%s): %s", codeModelRefusal, truncate(refusal, 1024))
	}
	if summary == "" {
		return nil, errorf(CodeProviderError, "the compaction model returned no summary text")
	}
	next := &contextView{catalog: p.view.catalog, portableThrough: p.view.portableThrough, summary: summary, coveredSeq: p.view.items[p.cut-1].seq, items: p.view.items[p.cut:], protectedUser: p.protected}
	spec := p.spec
	before := estimateRequest(spec.instructions, noteInput(noteInput(p.view.input(), p.notes), spec.pending), spec.tools)
	after := estimateRequest(spec.instructions, noteInput(noteInput(next.input(), p.notes), spec.pending), spec.tools)
	if after >= before {
		return nil, errorf(CodeContextLimit, "The summary did not reduce the context (estimated %d tokens before, %d after). Try a more concise compaction prompt or a smaller recent-history budget.", before, after)
	}
	if after > p.budget.input {
		return nil, errorf(CodeContextLimit, "After compaction the context is still estimated at %d tokens (summary about %d), but model %q allows %d input tokens after its output reserve. Try a more concise compaction prompt or a smaller recent-history budget; instructions, the protected user request and complete recent groups cannot be trimmed.", after, estimateTokens(len(summary)), p.main.Alias, p.budget.input)
	}
	result := &summaryResult{summary: summary, covered: next.coveredSeq}
	if p.protected != nil {
		result.protected = p.protected.seq
	}
	return result, nil
}
