package sam

import (
	"bytes"
	"context"
	"slices"
	"unicode/utf8"
)

func cloneToolInfo(t ToolInfo) ToolInfo {
	t.Parameters = bytes.Clone(t.Parameters)
	return t
}

func toolInfo(t Tool) ToolInfo {
	return ToolInfo{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
}

func (r *Runtime) beforeTurn(ctx context.Context, t *turnRun) (hookRun, error) {
	h := newHookRun(t.hookScope(), stageTurn)
	if len(r.cfg.Hooks.BeforeTurn) == 0 {
		return h, nil
	}
	in := BeforeTurnInput{Scope: h.scope, Instructions: t.instructions}
	if err := r.db.QueryRowContext(ctx, "SELECT source FROM turns WHERE id = ?", t.turnID).Scan(&in.Source); err != nil {
		return h, storageErr(err)
	}
	err := eachHistory(r.reader(ctx), func(it histItem) error {
		in.UserMessages = append(in.UserMessages, it.visible())
		return nil
	}, histSelect(visibleItem)+" AND h.turn_id = ? AND h.kind IN ('user', 'steering') ORDER BY h.seq", t.sessionID, t.turnID)
	if err != nil {
		return h, storageErr(err)
	}
	for _, cb := range r.cfg.Hooks.BeforeTurn {
		if ctx.Err() != nil {
			return h, context.Cause(ctx)
		}
		detached := in
		detached.UserMessages = slices.Clone(in.UserMessages) // genuine messages have no mutable Source
		text, err := cb.Handle(ctx, detached)
		if ctx.Err() != nil {
			return h, context.Cause(ctx)
		}
		if err == nil && !utf8.ValidString(text) {
			err = errorf(CodeHookFailed, "returned invalid UTF-8")
		}
		if err != nil {
			return h, r.recordHookStop(&h, r.hookError(&h, cb.ID, CodeHookFailed, err.Error()))
		}
		h.add(cb.ID, text)
	}
	return h, nil
}

func (r *Runtime) beforeRequest(ctx context.Context, t *turnRun) (hookRun, error) {
	h := newHookRun(t.hookScope(), stageRequest)
	if len(r.cfg.Hooks.BeforeRequest) == 0 {
		return h, nil
	}
	in := BeforeRequestInput{Scope: h.scope, Instructions: t.instructions}
	if err := r.db.QueryRowContext(ctx, `SELECT COALESCE(c.summary, ''),
		(SELECT COALESCE(MAX(seq), 0) FROM history WHERE session_id = s.id)
		FROM sessions s LEFT JOIN compactions c ON c.id = s.summary_id WHERE s.id = ?`, t.sessionID).Scan(&in.Summary, &in.HistoryThrough); err != nil {
		return h, storageErr(err)
	}
	for _, tool := range t.toolList {
		in.Tools = append(in.Tools, toolInfo(tool))
	}
	for _, cb := range r.cfg.Hooks.BeforeRequest {
		if ctx.Err() != nil {
			return h, context.Cause(ctx)
		}
		detached := in
		detached.Tools = make([]ToolInfo, len(in.Tools))
		for i, tool := range in.Tools {
			detached.Tools[i] = cloneToolInfo(tool)
		}
		text, err := cb.Handle(ctx, detached)
		if ctx.Err() != nil {
			return h, context.Cause(ctx)
		}
		if err == nil && !utf8.ValidString(text) {
			err = errorf(CodeHookFailed, "returned invalid UTF-8")
		}
		if err != nil {
			return h, r.recordHookStop(&h, r.hookError(&h, cb.ID, CodeHookFailed, err.Error()))
		}
		h.add(cb.ID, text)
	}
	return h, nil
}

// Decision failures are returned for atomic stop/audit settlement by the batch.
func (r *Runtime) beforeTool(ctx context.Context, t *turnRun, c plannedCall, tool Tool) (hookRun, error) {
	h := newHookRun(t.hookScope(), stageTool)
	h.callID = c.CallID
	h.scope.Attempt = c.Attempt
	for _, cb := range r.cfg.Hooks.BeforeTool {
		if ctx.Err() != nil {
			return h, r.cancelHookDecision(ctx, &h)
		}
		in := BeforeToolInput{Scope: h.scope, CallID: c.CallID, Tool: cloneToolInfo(toolInfo(tool)), Arguments: []byte(c.Arguments)}
		d, err := cb.Handle(ctx, in)
		if ctx.Err() != nil {
			return h, r.cancelHookDecision(ctx, &h)
		}
		if err == nil && !validDecision(d.Action, d.Reason, "") {
			err = errorf(CodeHookFailed, "returned an invalid decision; explicitly allow or deny with a nonblank reason")
		}
		if err != nil {
			err = r.hookError(&h, cb.ID, CodeHookFailed, err.Error())
			return h, r.recordHookStop(&h, err)
		}
		h.result.Handlers = append(h.result.Handlers, HookHandlerDecision{ID: cb.ID, Action: d.Action})
		if d.Action == HookDeny {
			return h, r.recordHookStop(&h, r.hookError(&h, cb.ID, CodeToolDenied, d.Reason))
		}
	}
	return h, nil
}

func (r *Runtime) beforeCompaction(ctx context.Context, in BeforeCompactionInput) (hookRun, error) {
	h := newHookRun(in.Scope, stageCompaction)
	for _, cb := range r.cfg.Hooks.BeforeCompaction {
		if ctx.Err() != nil {
			return h, r.cancelHookDecision(ctx, &h)
		}
		d, err := cb.Handle(ctx, in)
		if ctx.Err() != nil {
			return h, r.cancelHookDecision(ctx, &h)
		}
		if err == nil && !validDecision(d.Action, d.Reason, d.AppendInstructions) {
			err = errorf(CodeHookFailed, "returned an invalid compaction decision")
		}
		if err != nil {
			return h, r.recordHookStop(&h, r.hookError(&h, cb.ID, CodeHookFailed, err.Error()))
		}
		h.result.Handlers = append(h.result.Handlers, HookHandlerDecision{ID: cb.ID, Action: d.Action})
		if d.Action == HookDeny {
			return h, r.recordHookStop(&h, r.hookError(&h, cb.ID, CodeCompactionDenied, d.Reason))
		}
		h.add(cb.ID, d.AppendInstructions)
	}
	return h, nil
}
