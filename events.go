package sam

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"
)

// EventSchemaVersion is the version of the Event envelope.
const EventSchemaVersion = 1

// Event kinds identify persisted lifecycle, output, and diagnostic events.
// Use the named constants instead of matching event strings where possible.
const (
	EventTurnStarted            = "turn.started"
	EventTurnCompleted          = "turn.completed"
	EventTurnCancelled          = "turn.cancelled"
	EventTurnFailed             = "turn.failed"
	EventTurnInterrupted        = "turn.interrupted"
	EventTextDelta              = "text.delta"
	EventRefusalDelta           = "refusal.delta"
	EventRefusal                = "response.refusal"
	EventReasoningDelta         = "reasoning.delta"
	EventToolStarted            = "tool.started"
	EventToolOutput             = "tool.output"
	EventToolCompleted          = "tool.completed"
	EventToolFailed             = "tool.failed"
	EventToolCancelled          = "tool.cancelled"
	EventSteeringAccepted       = "steering.accepted"
	EventSteeringApplied        = "steering.applied"
	EventSteeringCleared        = "steering.cleared"
	EventSessionPaused          = "session.paused"
	EventCompactionStarted      = "compaction.started"
	EventCompactionCompleted    = "compaction.completed"
	EventCompactionFailed       = "compaction.failed"
	EventCompactionCancelled    = "compaction.cancelled"
	EventConfigChanged          = "config.changed"
	EventModelChanged           = "session.model_changed"
	EventModelAttemptStarted    = "model.attempt.started"
	EventModelAttemptEnded      = "model.attempt.ended"
	EventOperationSuspended     = "operation.suspended"
	EventOperationResumed       = "operation.resumed"
	EventToolAttemptInterrupted = "tool.attempt.interrupted"
	EventModelRetry             = "model.retry"
	EventUsage                  = "usage"
	EventSessionRenamed         = "session.renamed"
	EventSubagentStarted        = "subagent.started" // TurnAck identifies the accepted child
)

// Presentation categories of events, history items, and diagnostics. A
// category tells a client how to show an entry: normal output, a diagnostic
// notice, or an error. It is not the outcome of a turn: streamed output does
// not mean the turn succeeded, and a failed tool call is an error the model
// may still recover from within the same turn. Inspect Kind and its payload or
// the saved operation record for the actual outcome.
const (
	CategoryOutput     = "output"
	CategoryDiagnostic = "diagnostic"
	CategoryError      = "error"
)

// EventCategory returns the presentation category of an event kind. Unknown
// kinds default to CategoryOutput so clients can display future event types.
func EventCategory(kind string) string {
	switch kind {
	case EventTurnFailed, EventToolFailed, EventCompactionFailed, EventRefusalDelta, EventRefusal:
		return CategoryError
	case EventModelAttemptStarted, EventModelAttemptEnded, EventOperationSuspended, EventOperationResumed, EventToolAttemptInterrupted,
		EventHookResult, EventModelRetry, EventCompactionStarted, EventCompactionCompleted, EventCompactionCancelled,
		EventTurnCancelled, EventTurnInterrupted, EventToolCancelled, EventSessionPaused:
		return CategoryDiagnostic
	}
	return CategoryOutput
}

// Event is one committed, replayable session event. Seq increases within its
// session, starting at 1. Payload is JSON whose shape is given by the payload
// type documented for Kind. Category is derived from Kind and is a display hint,
// not a success status. Work failures after acceptance arrive as events rather
// than method errors.
type Event struct {
	// Version is EventSchemaVersion.
	Version int `json:"version"`
	// SessionID and Seq identify this event within its session.
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
	// Time is when this committed event was created.
	Time time.Time `json:"time"`
	// Kind is the event type; Category is its presentation hint.
	Kind     string `json:"kind"`
	Category string `json:"category"`
	// TurnID, OperationID, and CallID are set when the event belongs to one.
	TurnID      string `json:"turn_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	CallID      string `json:"call_id,omitempty"`
	Step        int    `json:"step,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	// Payload is the event-specific JSON value; decode it using Kind.
	Payload json.RawMessage `json:"payload"`
}

// Payload types by event kind.
type (
	// TurnStartedPayload: turn.started. Reasoning is "summaries" when the
	// session requested provider reasoning summaries, else "unavailable".
	TurnStartedPayload struct {
		Source    string `json:"source"`
		Prompt    string `json:"prompt,omitempty"`
		Images    int    `json:"images,omitempty"` // number of images sent with the prompt
		Model     string `json:"model"`
		Account   string `json:"account,omitempty"` // login account, for subscription providers
		Reasoning string `json:"reasoning"`
	}
	// TurnEndedPayload: turn.completed/cancelled/failed/interrupted. Code
	// is the error code of a failed turn.
	TurnEndedPayload struct {
		Reason          string           `json:"reason,omitempty"`
		Code            ErrorCode        `json:"code,omitempty"`
		Error           string           `json:"error,omitempty"`
		ProviderFailure *ProviderFailure `json:"provider_failure,omitempty"`
	}
	// DeltaPayload: text.delta, refusal.delta, and reasoning.delta.
	DeltaPayload struct {
		Text string `json:"text"`
	}
	// RefusalPayload: response.refusal, the complete refusal text of a
	// completed model response. Code is "model_refusal".
	RefusalPayload struct {
		Code string `json:"code"`
		Text string `json:"text"`
	}
	// ToolStartedPayload: tool.started.
	ToolStartedPayload struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	// ToolOutputPayload: tool.output. Stream is "stdout", "stderr" or "output".
	ToolOutputPayload struct {
		Stream string `json:"stream"`
		Text   string `json:"text"`
	}
	// ToolEndedPayload: tool.completed/failed/cancelled. Result is the
	// exact text supplied to the model.
	ToolEndedPayload struct {
		Name   string `json:"name"`
		Result string `json:"result"`
	}
	// SteeringPayload: steering.accepted and steering.applied.
	SteeringPayload struct {
		SteeringID string `json:"steering_id"`
		Text       string `json:"text"`
		Paused     bool   `json:"paused,omitempty"`
	}
	// SteeringClearedPayload: steering.cleared.
	SteeringClearedPayload struct {
		Cleared []string `json:"cleared"`
	}
	// CompactionPayload: compaction.* events.
	CompactionPayload struct {
		Model           string           `json:"model"`
		Account         string           `json:"account,omitempty"` // login account, for subscription providers
		Automatic       bool             `json:"automatic"`
		CoveredSeq      int64            `json:"covered_seq,omitempty"`
		Code            ErrorCode        `json:"code,omitempty"`
		Error           string           `json:"error,omitempty"`
		ProviderFailure *ProviderFailure `json:"provider_failure,omitempty"`
	}
	// RetryPayload: model.retry.
	RetryPayload struct {
		Attempt    int     `json:"attempt"`
		Reason     string  `json:"reason"`
		DelaySecs  float64 `json:"delay_seconds"`
		Compaction bool    `json:"compaction,omitempty"`
	}
	// UsagePayload: usage.
	UsagePayload struct {
		Usage
		Compaction bool `json:"compaction,omitempty"`
	}
)

// EventQuery selects a page or subscription of events after a session cursor.
type EventQuery struct {
	// SessionID selects the session whose events are read.
	SessionID string
	// After is the last sequence already handled; returned events have larger Seq.
	After int64
	// Limit is the page size for Events; 0 or above 500 means 500.
	Limit int
}

const pollInterval = 100 * time.Millisecond

// Events returns a bounded page of committed events after q.After. Save the
// largest returned Seq and pass it as After to resume. Limit 0 or above 500
// means 500. A future cursor is rejected by Subscribe.
func (r *Runtime) Events(ctx context.Context, q EventQuery) ([]Event, error) {
	if err := r.readable(); err != nil {
		return nil, err
	}
	if _, err := loadSession(r.reader(ctx), q.SessionID); err != nil {
		return nil, readErr(ctx, err)
	}
	return r.eventPage(ctx, q)
}

func (r *Runtime) eventPage(ctx context.Context, q EventQuery) ([]Event, error) {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx, `SELECT seq, turn_id, operation_id, call_id, kind, created_at, payload, step, attempt
		FROM events WHERE session_id = ? AND seq > ? ORDER BY seq LIMIT ?`, q.SessionID, q.After, limit)
	if err != nil {
		return nil, readErr(ctx, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows, q.SessionID)
		if err != nil {
			return nil, readErr(ctx, err)
		}
		out = append(out, e)
	}
	return out, readErr(ctx, rows.Err())
}

func scanEvent(rows *sql.Rows, sid string) (Event, error) {
	e := Event{Version: EventSchemaVersion, SessionID: sid}
	var ts int64
	var payload string
	err := rows.Scan(&e.Seq, &e.TurnID, &e.OperationID, &e.CallID, &e.Kind, &ts, &payload, &e.Step, &e.Attempt)
	e.Time, e.Payload, e.Category = fromMillis(ts), json.RawMessage(payload), EventCategory(e.Kind)
	return e, err
}

// Subscription replays committed events after a cursor and then follows new
// events by polling SQLite while Next is waiting. Delivery is at least once
// across reconnects; use Seq to deduplicate. Closing a subscription never
// cancels the underlying turn.
type Subscription struct {
	r         *Runtime
	sessionID string
	after     int64
	page      []Event
	done      chan struct{}
	once      sync.Once
	ctx       context.Context // ends with Close or the runtime; cancels a query in progress
	cancel    context.CancelFunc
}

// Subscribe validates q.After and returns a subscription starting after that
// sequence. Events are replayed before following new ones. A cursor beyond the
// current end is invalid; after reconnect, deduplicate by Seq. Closing the
// subscription only stops observation and never affects running work.
func (r *Runtime) Subscribe(ctx context.Context, q EventQuery) (*Subscription, error) {
	if err := r.readable(); err != nil {
		return nil, err
	}
	var last int64
	err := r.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = ?", q.SessionID).Scan(&last)
	if err != nil {
		return nil, readErr(ctx, err)
	}
	if _, err := loadSession(r.reader(ctx), q.SessionID); err != nil {
		return nil, readErr(ctx, err)
	}
	if q.After < 0 || q.After > last {
		return nil, errorf(CodeInvalidArgument, "cursor %d is outside the event range 0..%d", q.After, last)
	}
	sub := &Subscription{r: r, sessionID: q.SessionID, after: q.After, done: make(chan struct{})}
	sub.ctx, sub.cancel = context.WithCancel(r.ctx)
	return sub, nil
}

// stopped reports why the subscription must not yield anything more: the
// caller's context ended, or the subscription or runtime closed.
func (s *Subscription) stopped(ctx context.Context) error {
	select {
	case <-s.done:
		return errorf(CodeClosed, "subscription closed")
	case <-s.r.ctx.Done():
		return errClosed
	default:
		return ctx.Err()
	}
}

// Next returns the next event, waiting for one if caught up. The supplied ctx
// bounds this wait and query; ending it closes neither the subscription nor the
// turn. It returns not_found if the session is deleted, closed if the runtime or
// subscription closes, or ctx.Err when ctx ends. These states are checked
// before yielding a buffered event.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	for {
		if err := s.stopped(ctx); err != nil {
			return Event{}, err
		}
		if len(s.page) > 0 {
			e := s.page[0]
			s.page = s.page[1:]
			s.after = e.Seq
			return e, nil
		}
		page, err := s.load(ctx)
		if err != nil {
			return Event{}, err
		}
		if s.page = page; len(page) > 0 {
			continue
		}
		t := time.NewTimer(pollInterval)
		select {
		case <-t.C:
		case <-ctx.Done():
		case <-s.done:
		case <-s.r.ctx.Done():
		}
		t.Stop()
	}
}

// load reads the next page, or checks that the session still exists when
// there is none. Its queries end with ctx, Close, or the runtime, including
// while they wait for the shared database connection.
func (s *Subscription) load(ctx context.Context) ([]Event, error) {
	qctx, cancel := context.WithCancel(ctx)
	defer context.AfterFunc(s.ctx, cancel)()
	defer cancel()
	page, err := s.r.eventPage(qctx, EventQuery{SessionID: s.sessionID, After: s.after, Limit: 200})
	if err == nil && len(page) == 0 {
		_, err = loadSession(s.r.reader(qctx), s.sessionID)
		err = readErr(qctx, err)
	}
	if err != nil {
		if serr := s.stopped(ctx); serr != nil {
			return nil, serr
		}
		return nil, err
	}
	return page, nil
}

// Close ends the subscription and cancels a query Next is running or waiting
// to run. It is safe to call more than once and does not stop session work.
func (s *Subscription) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.cancel()
	})
	return nil
}

// batcher coalesces small deltas of one kind/stream into events of roughly
// 16 KiB or 50 ms, committing each batch before it becomes visible.
type batcher struct {
	r      *Runtime
	base   Event // session, turn and call IDs
	mu     sync.Mutex
	kind   string
	stream string
	buf    []byte
	timer  *time.Timer
	err    error
}

const (
	batchBytes = 16 << 10
	batchDelay = 50 * time.Millisecond
)

func (b *batcher) add(kind, stream string, p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return
	}
	if len(b.buf) > 0 && (kind != b.kind || stream != b.stream) {
		b.flushLocked(true)
	}
	b.kind, b.stream = kind, stream
	b.buf = append(b.buf, p...)
	if len(b.buf) >= batchBytes {
		b.flushLocked(false)
	} else if b.timer == nil {
		b.timer = time.AfterFunc(batchDelay, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.timer = nil
			b.flushLocked(false)
		})
	}
}

// flush commits everything buffered and reports any earlier storage error.
func (b *batcher) flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked(true)
	return b.err
}

func (b *batcher) flushLocked(final bool) {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if len(b.buf) == 0 || b.err != nil {
		return
	}
	n := len(b.buf)
	if !final {
		n = completeUTF8(b.buf)
	}
	if n == 0 {
		return
	}
	var payload any = DeltaPayload{Text: string(b.buf[:n])}
	if b.kind == EventToolOutput {
		payload = ToolOutputPayload{Stream: b.stream, Text: string(b.buf[:n])}
	}
	ev := event(b.base.SessionID, b.kind, payload)
	ev.TurnID, ev.OperationID, ev.CallID = b.base.TurnID, b.base.OperationID, b.base.CallID
	ev.Step, ev.Attempt = b.base.Step, b.base.Attempt
	b.err = b.r.tx(func(tx *sql.Tx) error { return appendEvent(tx, ev) })
	b.buf = append(b.buf[:0], b.buf[n:]...)
}

// completeUTF8 returns the length of p without a trailing incomplete rune.
func completeUTF8(p []byte) int {
	for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
		if utf8.RuneStart(p[i]) {
			if utf8.FullRune(p[i:]) {
				return len(p)
			}
			return i
		}
	}
	return len(p)
}
