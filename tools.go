package sam

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Tool is a host-supplied or built-in model tool. Registrations require a name,
// a non-nil Run function, and an object JSON schema; invalid host tools are
// skipped and reported through Runtime.Diagnostics. A host tool with a built-in
// name replaces that built-in. Run must honor ctx cancellation and should send
// output through call.Output. Its returned string is short result metadata
// appended to captured output; the combined result must fit the per-call limit.
type Tool struct {
	// Name is the function name advertised to the model.
	Name string
	// Description explains the tool's purpose to the model.
	Description string
	// Parameters is a JSON schema whose top-level type must be "object".
	Parameters json.RawMessage
	// ReplayUnsafe skips unfinished calls carried across a runtime restart,
	// including calls that never started. The model receives a recovery result
	// and may issue a fresh call. False permits replay; it is not an exactly-once
	// guarantee. Recovery honors the restriction saved when planning the call
	// or a stricter current definition. This is host policy, never an argument.
	ReplayUnsafe bool
	// Run validates arguments and honors cancellation. If it stops because ctx
	// ended, return or wrap ctx.Err() or context.Cause(ctx) (errors.Is must
	// identify it). Nil is known success even after cancellation; other errors
	// are known failures. Identifiable shutdown cancellation leaves an uncertain
	// call, which recovery skips when ReplayUnsafe is set.
	Run func(ctx context.Context, call ToolCall) (string, error)
}

// ToolCall is the input to Tool.Run. Workspace is a working directory, not a
// sandbox; custom tools run with the host process's authority.
type ToolCall struct {
	// SessionID identifies the calling session.
	SessionID string
	// TurnID and CallID form the stable logical identity with SessionID.
	// Attempt counts durably admitted invocations; recovery may repeat effects.
	TurnID, CallID string
	Attempt        int
	uncertain      bool
	// Workspace is the session's configured working directory.
	Workspace string
	// Arguments is the model-provided JSON value; validate it in Run.
	Arguments json.RawMessage
	// Output captures and streams output under the call's byte limit.
	Output *Output

	launched *[]*bgProc // background processes this call started, until its result is saved
}

// ToolInfo describes a registered tool without its implementation.
type ToolInfo struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Parameters   json.RawMessage `json:"parameters"`
	ReplayUnsafe bool            `json:"replay_unsafe"`
}

// ErrOutputLimit is returned by Output writes after the call's capture limit is
// exceeded. The overflowing write is truncated to the remaining budget, the
// tool context is cancelled, and later writes fail without capturing bytes.
var ErrOutputLimit = errors.New("tool output limit exceeded")

// Output is a bounded capture-and-stream sink for one tool call. Bytes up to
// the limit are kept and streamed; the first byte beyond it cancels the tool's
// context and returns ErrOutputLimit. Captured output may already have been
// published in events even when the final call result reports an error.
type Output struct {
	mu       sync.Mutex
	limit    int
	buf      []byte
	overflow bool
	cancel   context.CancelFunc
	emit     func(stream string, p []byte)
}

// Write captures p on the "output" stream. If the limit is exceeded, it
// returns the number of bytes accepted and ErrOutputLimit.
func (o *Output) Write(p []byte) (int, error) { return o.write("output", p) }

// Stream returns a writer that captures into the same per-call budget and
// labels resulting events with name (for example, "stdout" or "stderr").
func (o *Output) Stream(name string) io.Writer {
	return writerFunc(func(p []byte) (int, error) { return o.write(name, p) })
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func (o *Output) write(stream string, p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.overflow {
		return 0, ErrOutputLimit
	}
	n := len(p)
	if room := o.limit - len(o.buf); n > room {
		n, o.overflow = room, true
	}
	o.buf = append(o.buf, p[:n]...)
	if n > 0 && o.emit != nil {
		o.emit(stream, p[:n])
	}
	if o.overflow {
		o.cancel()
		return n, ErrOutputLimit
	}
	return n, nil
}

func (o *Output) result() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf, o.overflow
}

// toolError is the model-visible JSON error result of a tool call.
func toolError(code, message string, limit int) string {
	e := map[string]any{"code": code, "message": truncate(message, 2048)}
	if limit > 0 {
		e["limit_bytes"] = limit
	}
	return mustJSON(map[string]any{"error": e})
}

func limitMessage(tool string, limit int) string {
	msg := fmt.Sprintf("%s output exceeded %s", tool, formatBytes(limit))
	switch tool {
	case "bash":
		return msg + " across stdout and stderr. Cancellation was requested; earlier side effects may have occurred. " +
			"Inspect state before retrying. Filter the output or request a smaller result."
	case "read":
		return msg + ". Select a smaller range with offset and limit arguments."
	case historyToolName:
		return msg + ". Read fewer entries: narrow from_seq/to_seq, or use smaller before/after values with around_seq."
	}
	return msg + ". The call was stopped; earlier side effects may have occurred. Request a narrower or summarized result."
}

func formatBytes(n int) string {
	if n%1024 == 0 {
		return fmt.Sprintf("%d KiB", n/1024)
	}
	return fmt.Sprintf("%d bytes", n)
}

type plannedCall struct {
	Namespace      string
	DispatchTarget string
	CallID         string
	Name           string
	Arguments      string
	Status         string
	Attempt        int
	Uncertain      bool
	ReplayUnsafe   bool
}

// runBatch executes one model response's tool calls with bounded
// parallelism. Queued calls do not start after ctx is cancelled; they stay
// planned and later receive not-executed results.
func (r *Runtime) runBatch(ctx context.Context, t *turnRun, calls []plannedCall) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, max(1, t.cfg.ToolConcurrency))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			if ctx.Err() == nil {
				if saved := r.failExecution(t.sessionID, err); saved != nil {
					firstErr = saved
				}
			}
			cancel()
		}
		mu.Unlock()
	}
	for _, c := range calls {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := r.runCall(ctx, t, c, fail); err != nil {
				fail(err)
			}
		})
	}
	wg.Wait()
	return firstErr
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (r *Runtime) runCall(ctx context.Context, t *turnRun, c plannedCall, fail func(error)) error {
	base := Event{SessionID: t.sessionID, TurnID: t.turnID, CallID: c.CallID, Step: t.step, Attempt: c.Attempt}
	if ctx.Err() != nil {
		return nil // still planned: settlement knows Run never began
	}
	tool, ok := t.tools[c.DispatchTarget]
	if !ok {
		return r.finishCall(t, c, "failed", toolError("tool_unavailable", fmt.Sprintf("tool %q is not available", c.Name), 0))
	}
	approval, hookErr := r.beforeTool(ctx, t, c, tool)
	if hookErr != nil {
		code := ErrorCodeOf(hookErr)
		if code != CodeToolDenied && code != CodeHookFailed {
			return hookErr
		}
		if code == CodeHookFailed {
			fail(hookErr)
		} else {
			if err := r.tx(func(tx *sql.Tx) error { return appendEvent(tx, approval.event()) }); err != nil {
				return err
			}
		}
		if err := r.finishCall(t, c, "failed", toolError(string(code), hookErr.Error(), 0)); err != nil {
			return err
		}
		if code == CodeToolDenied {
			return nil
		}
		return hookErr
	}
	r.mu.Lock()
	if ctx.Err() != nil {
		r.mu.Unlock()
		return r.cancelHookDecision(ctx, &approval)
	}
	admitted := false
	err := r.tx(func(tx *sql.Tx) error {
		x, err := loadExecution(tx, t.sessionID)
		if err != nil {
			return err
		}
		if x == nil {
			return fmt.Errorf("execution disappeared before tool intent admission")
		}
		if err := approval.save(tx); err != nil {
			return err
		}
		if x.Action == actionStop {
			return nil
		}
		if _, err := tx.ExecContext(bg, "UPDATE tool_calls SET status = 'started', attempts = attempts + 1, started_at = ? WHERE turn_id = ? AND call_id = ? AND status = 'planned'",
			nowMillis(), t.turnID, c.CallID); err != nil {
			return err
		}
		ev := event(t.sessionID, EventToolStarted, ToolStartedPayload{Name: c.Name, Arguments: rawArgs(c.Arguments)})
		ev.TurnID, ev.CallID, ev.Step, ev.Attempt = base.TurnID, base.CallID, base.Step, base.Attempt
		if err := appendEvent(tx, ev); err != nil {
			return err
		}
		admitted = true
		return nil
	})
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if !admitted {
		return nil
	}
	return r.executeCall(ctx, t, c, tool, base)
}

// executeCall runs only after durable intent. A cancellation observed before
// invoking host code is a known non-execution, not an uncertain effect.
func (r *Runtime) executeCall(ctx context.Context, t *turnRun, c plannedCall, tool Tool, base Event) (err error) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := &batcher{r: r, base: base}
	out := &Output{limit: t.cfg.ToolOutputLimit, cancel: cancel, emit: func(s string, p []byte) { b.add(EventToolOutput, s, p) }}
	var launched []*bgProc
	defer func() {
		// A background process whose launch result could not be saved is
		// stopped: nothing durable would identify it. Earlier, acknowledged
		// launches keep running; effects that already happened stay.
		if err != nil {
			for _, p := range launched {
				p.requestStop()
			}
		}
	}()
	if ctx.Err() != nil {
		if errors.Is(context.Cause(ctx), errShutdown) {
			return r.suspendCall(t, c, false)
		}
		return r.finishCall(t, c, "not_executed", toolError("not_executed", "The call was stopped after intent was saved but before Run was invoked.", 0))
	}
	text, runErr := tool.Run(callCtx, ToolCall{
		SessionID: t.sessionID, TurnID: t.turnID, CallID: c.CallID, Attempt: c.Attempt, uncertain: c.Uncertain,
		Workspace: t.workspace, Arguments: json.RawMessage(c.Arguments), Output: out, launched: &launched,
	})
	if err := b.flush(); err != nil {
		return err
	}
	status, result := classifyCall(ctx, c.Name, out, text, runErr, t.cfg.ToolOutputLimit)
	if status == "cancelled" && errors.Is(context.Cause(ctx), errShutdown) {
		return r.suspendCall(t, c, true)
	}
	return r.finishCall(t, c, status, result)
}

// classifyCall turns a finished Run into the call's status and result. A
// known successful return is kept even if the session was interrupted
// meanwhile: the effect happened, and the turn stops on its own. Only an
// unfinished call is reported as cancelled.
func classifyCall(ctx context.Context, name string, out *Output, text string, runErr error, limit int) (string, string) {
	captured, overflow := out.result()
	switch {
	case runErr != nil && ErrorCodeOf(runErr) == CodeProcessObservation:
		message := runErr.Error()
		if overflow {
			message += "; output also exceeded the configured limit, and earlier side effects may have occurred"
		}
		e := map[string]any{"error": map[string]any{
			"code": string(CodeProcessObservation), "message": truncate(message, 2048),
		}}
		if len(captured) > 0 {
			e["output"] = string(captured)
		}
		return "failed", mustJSON(e)
	case overflow || errors.Is(runErr, ErrOutputLimit):
		return "failed", toolError("tool_output_limit", limitMessage(name, limit), limit)
	case identifiableCancellation(ctx, runErr):
		return "cancelled", toolError("cancelled",
			"The tool call was cancelled before it finished; earlier side effects may have occurred.", 0)
	case runErr != nil:
		e := map[string]any{"error": map[string]any{"code": "tool_error", "message": truncate(runErr.Error(), 2048)}}
		if len(captured) > 0 {
			e["output"] = string(captured)
		}
		return "failed", mustJSON(e)
	}
	result := string(captured)
	if text != "" {
		if result != "" && result[len(result)-1] != '\n' {
			result += "\n"
		}
		result += text
	}
	if len(result) > limit {
		return "failed", toolError("tool_output_limit", limitMessage(name, limit), limit)
	}
	return "completed", result
}

func rawArgs(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return json.RawMessage(mustJSON(s))
}

func identifiableCancellation(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx)))
}

func (r *Runtime) finishCall(t *turnRun, c plannedCall, status, result string) error {
	if c.Uncertain && status == "completed" && len(uncertainWarning)+1+len(result) > t.cfg.ToolOutputLimit {
		status, result = "failed", toolError("tool_output_limit", limitMessage(c.Name, t.cfg.ToolOutputLimit), t.cfg.ToolOutputLimit)
	}
	result = uncertainResult(c, status, result, t.cfg.ToolOutputLimit)
	return r.tx(func(tx *sql.Tx) error {
		return saveCallResult(tx, t.sessionID, t.turnID, t.step, c, status, result)
	})
}

// Normal execution and recovery publish tool outcomes through the same owner.
// Batch assembly later pairs these results with calls in original model order.
func saveCallResult(tx *sql.Tx, sid, tid string, step int, c plannedCall, status, result string) error {
	if _, err := tx.ExecContext(bg, "UPDATE tool_calls SET status = ?, output = ?, ended_at = ? WHERE turn_id = ? AND call_id = ?",
		status, result, nowMillis(), tid, c.CallID); err != nil {
		return err
	}
	kind := map[string]string{"completed": EventToolCompleted, "failed": EventToolFailed, "uncertain": EventToolFailed, "cancelled": EventToolCancelled, "not_executed": EventToolCancelled}[status]
	ev := event(sid, kind, ToolEndedPayload{Name: c.Name, Result: result})
	ev.TurnID, ev.CallID, ev.Step, ev.Attempt = tid, c.CallID, step, c.Attempt
	return appendEvent(tx, ev)
}

// closeOpenCalls writes a function_call_output history item, in model-call
// order, for every tool call of the session that does not have one yet.
// Calls that never started get a not-executed result; calls that started
// without a recorded outcome get an uncertain result. Nothing is rerun.
func closeOpenCalls(tx *sql.Tx, sessionID string) error { return assembleCalls(tx, sessionID, true) }

func assembleCalls(tx *sql.Tx, sessionID string, terminal bool) error {
	rows, err := tx.QueryContext(bg, `SELECT turn_id, call_id, status, COALESCE(output, ''), had_uncertain_attempt FROM tool_calls
		WHERE session_id = ? AND history_seq IS NULL ORDER BY rowid`, sessionID)
	if err != nil {
		return err
	}
	type open struct {
		turnID, callID, status, output string
		uncertain                      bool
	}
	var calls []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.turnID, &o.callID, &o.status, &o.output, &o.uncertain); err != nil {
			rows.Close()
			return err
		}
		calls = append(calls, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range calls {
		status, output := c.status, c.output
		if !terminal && (status == "planned" || status == "started") {
			continue
		}
		switch status {
		case "planned":
			status, output = "not_executed", toolError("not_executed", "The tool call was not executed because the turn stopped first.", 0)
		case "started":
			status, output = "uncertain", toolError("uncertain",
				"The tool call started but its outcome was not recorded (the runtime stopped). It may or may not have taken effect; inspect state before retrying.", 0)
		}
		if c.uncertain && (c.status == "planned" || c.status == "started") {
			status, output = "uncertain", toolError("uncertain", uncertainWarning+" The current attempt did not finish before the operation stopped.", 0)
		}
		seq, err := appendHistory(tx, sessionID, c.turnID, kindToolResult, "",
			map[string]any{"type": "function_call_output", "call_id": c.callID, "output": output})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(bg, "UPDATE tool_calls SET status = ?, output = ?, history_seq = ?, ended_at = COALESCE(ended_at, ?) WHERE turn_id = ? AND call_id = ?",
			status, output, seq, nowMillis(), c.turnID, c.callID); err != nil {
			return err
		}
	}
	return nil
}
