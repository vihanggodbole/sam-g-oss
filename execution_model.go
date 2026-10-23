package sam

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (r *Runtime) requestAttempt(ctx context.Context, x *execution, w *worker) error {
	if err := prepareBudget(ctx, x); err != nil {
		return err
	}
	p := w.prepared
	if p == nil {
		return errorf(CodeStorageError, "request action has no live preparation")
	}
	force := x.Progress.Refresh == "pending"
	if err := r.reserveAttempt(ctx, x, w, p); err != nil {
		return err
	}
	if force && w.rejected == "" {
		_, cred, err := r.client.auth.current(p.request.model.account)
		if err != nil {
			return r.modelFailure(ctx, x, w, err, false, false)
		}
		w.rejected = cred.Access
	}
	b := &batcher{r: r, base: x.event("", nil)}
	visible := false
	h := streamHooks{}
	if p.summary == nil {
		h.delta = func(kind, text string) {
			if text == "" {
				return
			}
			visible = true
			eventKind := EventReasoningDelta
			switch kind {
			case "text":
				eventKind = EventTextDelta
			case "refusal":
				eventKind = EventRefusalDelta
			}
			b.add(eventKind, "", []byte(text))
		}
	}
	// Reservation precedes every credential/transport operation. once clears the
	// live rejected token only after successful credential resolution.
	res, err := r.client.once(ctx, p.request, p.body, h, &w.rejected)
	if ferr := b.flush(); ferr != nil {
		return ferr
	}
	if err != nil {
		return r.modelFailure(ctx, x, w, err, visible, force && w.rejected != "")
	}
	return r.modelSuccess(x, w, res)
}

func (r *Runtime) reserveAttempt(ctx context.Context, x *execution, w *worker, p *preparedRequest) error {
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
		current.Progress.Attempts++
		current.Progress.NotBefore = 0
		current.Progress.InFlight = true
		if current.Progress.Refresh == "pending" {
			current.Progress.Refresh = "used"
		}
		reason := "retry"
		if current.Progress.Attempts == 1 {
			reason = "initial"
		} else if w.recovered {
			reason = "recovery"
		}
		payload := ModelAttemptStartedPayload{Model: p.request.model.Alias, Account: p.request.model.accountName(), Compaction: p.summary != nil, Reason: reason}
		if err := appendEvent(tx, current.event(EventModelAttemptStarted, payload)); err != nil {
			return err
		}
		if err := saveExecution(tx, current); err != nil {
			return err
		}
		*x = *current
		if w.main != nil {
			w.main.attempt = current.Progress.Attempts
		}
		w.recovered = false
		return nil
	})
}

func modelFailureError(m resolvedModel, err error) error {
	var me *modelError
	if !errors.As(err, &me) {
		return err
	}
	e := &Error{Code: CodeProviderError, Message: me.msg, ProviderFailure: me.failure}
	if m.API.effective() == APIAnthropicMessages && me.failure != nil && (me.failure.HTTPStatus == 400 || me.failure.Code == "invalid_request_error") {
		e.Message += ". Check the Messages model/settings. If signed context became incompatible after an instruction, model or credential change, explicitly switch this idle chat's model/account to create a portable-history boundary, or start a new chat. Saved history is unchanged; no fallback was attempted"
	}
	if me.authRequired && m.auth != "" {
		e.Code = CodeAuthRequired
	}
	if me.incompatible {
		e.Code, e.Message = CodeHistoryIncompatible, historyIncompatible(m, me.msg)
	}
	return e
}

func (r *Runtime) modelFailure(ctx context.Context, old *execution, w *worker, failure error, visible, refreshFailed bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tx(func(tx *sql.Tx) error {
		x, err := loadExecution(tx, old.SessionID)
		if err != nil || x == nil {
			return err
		}
		usage, err := recordFailedUsage(tx, x, failure)
		if err != nil {
			return err
		}
		m := w.prepared.request.model
		safe := modelFailureError(m, failure)
		if x.CompactionID != "" {
			safe = &Error{Code: ErrorCodeOf(safe), Message: "compaction request failed: " + safe.Error(), ProviderFailure: providerFailureOf(safe)}
		}
		if x.Action == actionStop {
			if err := settleAttempt(tx, x, "cancelled", safe, usage, true); err != nil {
				return err
			}
			w.prepared = nil
			return saveExecution(tx, x)
		}
		if errors.Is(context.Cause(ctx), errShutdown) && identifiableCancellation(ctx, failure) {
			if err := settleAttempt(tx, x, "interrupted", errShutdown, usage, true); err != nil {
				return err
			}
			unprepare(x)
			w.prepared = nil
			return saveExecution(tx, x)
		}
		retry, delay := r.retryDecision(x, w, failure, visible, refreshFailed)
		if ctx.Err() != nil && !errors.Is(context.Cause(ctx), errShutdown) {
			retry = false
		}
		if !retry {
			if errors.Is(context.Cause(ctx), errInterrupted) {
				stopExecution(x, "interrupted", nil)
			} else {
				var me *modelError
				if errors.As(failure, &me) && me.rejected != "" {
					safe = &Error{Code: CodeAuthRequired, Message: fmt.Sprintf("the provider rejected account %q (HTTP 401); the one refresh cycle or four-attempt budget is exhausted. Log in again before continuing: %s", m.account.name, me.msg), ProviderFailure: providerFailureOf(safe)}
				}
				stopExecution(x, "failed", safe)
			}
		}
		if err := settleAttempt(tx, x, "failed", safe, usage, !retry); err != nil {
			return err
		}
		if retry {
			x.Progress.NotBefore = time.Now().Add(delay).UnixMilli()
			payload := RetryPayload{Attempt: x.Progress.Attempts, Reason: safe.Error(), DelaySecs: delay.Seconds(), Compaction: x.CompactionID != ""}
			if err := appendEvent(tx, x.event(EventModelRetry, payload)); err != nil {
				return err
			}
		} else {
			w.prepared = nil
		}
		return saveExecution(tx, x)
	})
}

func recordFailedUsage(tx *sql.Tx, x *execution, failure error) (*Usage, error) {
	var me *modelError
	if !errors.As(failure, &me) || me.usage == nil {
		return nil, nil
	}
	table, id := "turns", x.TurnID
	if x.CompactionID != "" {
		table, id = "compactions", x.CompactionID
	}
	return me.usage, recordUsage(tx, x.SessionID, x.TurnID, table, id, *me.usage, x.CompactionID != "")
}

func (r *Runtime) retryDecision(x *execution, w *worker, failure error, visible, refreshFailed bool) (bool, time.Duration) {
	var me *modelError
	if visible || x.Progress.Attempts >= 4 || !errors.As(failure, &me) || !me.retryable {
		return false, 0
	}
	delay := me.retryAfter
	if me.rejected != "" {
		if x.Progress.Refresh != "available" {
			return false, 0
		}
		x.Progress.Refresh = "pending"
		w.rejected = me.rejected
		return true, 0
	}
	if refreshFailed {
		x.Progress.Refresh = "pending"
	}
	if delay == 0 && (me.failure == nil || me.failure.RetryAfterSeconds == nil) {
		delay = r.client.backoff[x.Progress.Attempts-1]
	}
	return true, delay
}

func (r *Runtime) modelSuccess(old *execution, w *worker, res *modelResult) error {
	p := w.prepared
	var summary *summaryResult
	var invalid error
	if p.summary != nil {
		summary, invalid = p.summary.validate(res)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.tx(func(tx *sql.Tx) error {
		x, err := loadExecution(tx, old.SessionID)
		if err != nil || x == nil {
			return err
		}
		if err := finishAttempt(tx, x, res.usage); err != nil {
			return err
		}
		if p.summary != nil {
			if err := recordUsage(tx, x.SessionID, x.TurnID, "compactions", x.CompactionID, res.usage, true); err != nil {
				return err
			}
			if invalid != nil {
				stopExecution(x, "failed", invalid)
			}
			if x.Action == actionStop {
				return saveExecution(tx, x)
			}
			if err := installSummary(tx, x.SessionID, x.TurnID, p.summary.spec, summary); err != nil {
				return err
			}
			if x.TurnID == "" {
				return r.completeExecution(tx, x)
			}
			x.CompactionID = ""
			x.Action = actionPrepare
			x.Progress = freshProgress()
			return saveExecution(tx, x)
		}
		count, err := saveOutput(tx, w.main, res)
		if err != nil {
			return err
		}
		if x.Action == actionStop {
			return saveExecution(tx, x)
		}
		if count == 0 {
			return r.completeExecution(tx, x)
		}
		x.Action = actionBatch
		return saveExecution(tx, x)
	})
	if err == nil {
		w.prepared = nil
		w.rejected = ""
		if p.summary != nil {
			w.compaction = nil
		}
		r.captureWork(old.SessionID, w)
	}
	return err
}
