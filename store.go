package sam

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"modernc.org/sqlite"
)

// Session phases.
const (
	PhaseSuspended  = "suspended"
	PhaseIdle       = "idle"
	PhaseRunning    = "running"
	PhaseCompacting = "compacting"
	PhaseStopping   = "stopping"
)

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// bg is used for storage calls: persistence must not be cut short by the
// cancellation of the work being recorded.
var bg = context.Background()

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// tx runs fn in a transaction for work that is already accepted or running.
// Failures are storage errors unless fn returned an *Error. Any storage
// failure stops admission and cancels active work (fail-stop): effects must
// not continue without durable records.
func (r *Runtime) tx(fn func(tx *sql.Tx) error) error { return r.runTx(fn, false) }

// admitTx runs the transaction that accepts a new command. If SQLite stays
// busy or locked past the busy timeout, nothing was accepted: it returns
// storage_busy and leaves the runtime healthy, so the caller can retry with
// the same request ID. Other failures are fail-stop, as in tx.
func (r *Runtime) admitTx(fn func(tx *sql.Tx) error) error { return r.runTx(fn, true) }

func (r *Runtime) runTx(fn func(tx *sql.Tx) error, admission bool) error {
	tx, err := r.db.BeginTx(bg, nil)
	if err != nil {
		return r.txFailed(err, admission)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		var e *Error
		if errors.As(err, &e) {
			return err
		}
		return r.txFailed(err, admission)
	}
	if err := tx.Commit(); err != nil {
		return r.storageFailed(err) // the outcome of a failed commit is not known
	}
	return nil
}

func (r *Runtime) txFailed(err error, admission bool) error {
	if admission && isBusy(err) {
		return errorf(CodeStorageBusy, "the database stayed locked by another connection for %s; nothing was accepted. "+
			"Retry the same request ID shortly", busyTimeout)
	}
	return r.storageFailed(err)
}

// isBusy reports SQLite lock contention (SQLITE_BUSY or SQLITE_LOCKED,
// including extended codes).
func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	c := se.Code() & 0xff
	return c == 5 || c == 6
}

// queryRows runs a query and calls scan for every row, returning the first
// query, scan, or iteration error.
func queryRows(q querier, scan func(*sql.Rows) error, query string, args ...any) error {
	rows, err := q.QueryContext(bg, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ctxDB runs read-only queries of a public call with the caller's context
// instead of the background context the shared helpers pass.
type ctxDB struct {
	ctx context.Context
	db  *sql.DB
}

func (r *Runtime) reader(ctx context.Context) querier { return ctxDB{ctx: ctx, db: r.db} }

func (c ctxDB) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	return c.db.ExecContext(c.ctx, q, args...)
}

func (c ctxDB) QueryContext(_ context.Context, q string, args ...any) (*sql.Rows, error) {
	return c.db.QueryContext(c.ctx, q, args...)
}

func (c ctxDB) QueryRowContext(_ context.Context, q string, args ...any) *sql.Row {
	return c.db.QueryRowContext(c.ctx, q, args...)
}

type sessionRow struct {
	id, name, workspace string
	parentID            string
	isSubagent          bool
	cfg                 SessionConfig
	overrides           SessionOverrides
	instructions        string
	compactionPrompt    string
	resources           PreparedResources
	binding             *ModelBinding
	modelHistoryStart   int64
	phase               string
	paused              bool
	summaryID           string
	createdAt           int64
	updatedAt           int64
}

const sessionCols = `id, name, workspace, config, bound_model, bound_provider, bound_base_url, bound_model_id,
	COALESCE((SELECT CASE WHEN action = 'stop' THEN 'stopping' ELSE 'suspended' END FROM executions WHERE session_id = sessions.id), 'idle'),
	paused, COALESCE(summary_id, ''), created_at, updated_at, overrides, instructions, compaction_prompt, model_history_start, COALESCE(parent_id,''), is_subagent, resources, bound_api`

func scanSession(sc interface{ Scan(...any) error }) (*sessionRow, error) {
	var s sessionRow
	var cfg, resources string
	var api ModelAPI
	var bm, bp, bu, bi, overrides sql.NullString
	err := sc.Scan(&s.id, &s.name, &s.workspace, &cfg, &bm, &bp, &bu, &bi,
		&s.phase, &s.paused, &s.summaryID, &s.createdAt, &s.updatedAt, &overrides, &s.instructions, &s.compactionPrompt, &s.modelHistoryStart, &s.parentID, &s.isSubagent, &resources, &api)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(cfg), &s.cfg); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(resources), &s.resources); err != nil {
		return nil, err
	}
	if overrides.Valid {
		if err := json.Unmarshal([]byte(overrides.String), &s.overrides); err != nil {
			return nil, err
		}
	} else {
		s.overrides = explicitConfig(s.cfg)
	}
	if bm.Valid {
		s.binding = &ModelBinding{Model: bm.String, Provider: bp.String, BaseURL: bu.String, ModelID: bi.String, API: api.effective()}
	}
	return &s, nil
}

func loadSession(q querier, id string) (*sessionRow, error) {
	return scanSession(q.QueryRowContext(bg, "SELECT "+sessionCols+" FROM sessions WHERE id = ?", id))
}

func setPaused(q querier, id string, paused bool) error {
	_, err := q.ExecContext(bg, "UPDATE sessions SET paused = ?, updated_at = ? WHERE id = ?",
		paused, nowMillis(), id)
	return err
}

// appendEvent commits an event inside tx; it becomes visible to
// subscribers only when tx commits.
func appendEvent(q querier, e Event) error {
	payload := e.Payload
	if payload == nil {
		payload = json.RawMessage("{}")
	}
	_, err := q.ExecContext(bg, `INSERT INTO events (session_id, seq, turn_id, operation_id, call_id, kind, created_at, payload, step, attempt)
		VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE session_id = ?), ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.SessionID, e.SessionID, e.TurnID, e.OperationID, e.CallID, e.Kind, nowMillis(), string(payload), e.Step, e.Attempt)
	return err
}

func event(sessionID, kind string, payload any) Event {
	return Event{SessionID: sessionID, Kind: kind, Payload: json.RawMessage(mustJSON(payload))}
}

func (e Event) turn(id string) Event { e.TurnID = id; return e }
func (e Event) op(id string) Event   { e.OperationID = id; return e }

// History item kinds.
const (
	kindUser       = "user"
	kindSteering   = "steering"
	kindOutput     = "output"
	kindToolResult = "tool_result"
	kindNote       = "note"
)

func appendHistory(q querier, sessionID, turnID, kind, provenance string, item any) (int64, error) {
	var seq int64
	err := q.QueryRowContext(bg, `INSERT INTO history (session_id, seq, turn_id, kind, provenance, item, created_at)
		VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM history WHERE session_id = ?), ?, ?, ?, ?, ?) RETURNING seq`,
		sessionID, sessionID, turnID, kind, provenance, mustJSON(item), nowMillis()).Scan(&seq)
	if err != nil {
		return 0, err
	}
	return seq, touchActivity(q, sessionID)
}

// touchActivity records conversation activity: accepted input or a saved
// history entry. A parent's tree_activity_at also rises with its children so
// chat lists can order a parent by its delegated work. Metadata edits, reads
// and settings changes never call it. Time only moves forward.
func touchActivity(q querier, sessionID string) error {
	_, err := q.ExecContext(bg, `UPDATE sessions SET
		activity_at = CASE WHEN id = ?2 THEN MAX(activity_at, ?1) ELSE activity_at END,
		tree_activity_at = MAX(tree_activity_at, ?1)
		WHERE id = ?2 OR id = (SELECT parent_id FROM sessions WHERE id = ?2)`, nowMillis(), sessionID)
	return err
}

func userMessage(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": text}},
	}
}

// dedupe implements request-ID idempotency inside tx. It returns the saved
// acknowledgement when the request was already accepted.
func dedupe(q querier, requestID, op string, args any) (saved []byte, hash string, err error) {
	if requestID == "" {
		return nil, "", errorf(CodeInvalidArgument, "request_id is required")
	}
	sum := sha256.Sum256([]byte(op + "\x00" + mustJSON(args)))
	hash = hex.EncodeToString(sum[:])
	var gotOp, gotHash, resp string
	err = q.QueryRowContext(bg, "SELECT operation, hash, response FROM requests WHERE id = ?", requestID).
		Scan(&gotOp, &gotHash, &resp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, hash, nil
	}
	if err != nil {
		return nil, "", err
	}
	if gotOp != op || gotHash != hash {
		return nil, "", errorf(CodeConflict, "request_id %q was already used with different arguments", requestID)
	}
	return []byte(resp), hash, nil
}

func saveRequest(q querier, requestID, op, hash string, resp any) error {
	_, err := q.ExecContext(bg, "INSERT INTO requests (id, operation, hash, response, created_at) VALUES (?, ?, ?, ?, ?)",
		requestID, op, hash, mustJSON(resp), nowMillis())
	return err
}

// nullableUsage converts usage counters to SQL values (NULL means unknown).
func nullableUsage(u Usage) (any, any, any, any, any) {
	conv := func(p *int64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	return conv(u.InputTokens), conv(u.OutputTokens), conv(u.ReasoningTokens), conv(u.CacheReadTokens), conv(u.CacheWriteTokens)
}

func scanUsage(a, b, c, read, write sql.NullInt64) Usage {
	conv := func(n sql.NullInt64) *int64 {
		if !n.Valid {
			return nil
		}
		v := n.Int64
		return &v
	}
	return Usage{InputTokens: conv(a), OutputTokens: conv(b), ReasoningTokens: conv(c), CacheReadTokens: conv(read), CacheWriteTokens: conv(write)}
}

// addUsage accumulates only reported counters: unknown + unknown stays unknown.
// Check before SQLite arithmetic can promote an overflowing INTEGER to REAL.
func addUsage(a, b Usage) (Usage, error) {
	out := Usage{}
	left := []*int64{a.InputTokens, a.OutputTokens, a.ReasoningTokens, a.CacheReadTokens, a.CacheWriteTokens}
	right := []*int64{b.InputTokens, b.OutputTokens, b.ReasoningTokens, b.CacheReadTokens, b.CacheWriteTokens}
	dst := []**int64{&out.InputTokens, &out.OutputTokens, &out.ReasoningTokens, &out.CacheReadTokens, &out.CacheWriteTokens}
	for i, n := range right {
		if n == nil {
			*dst[i] = left[i]
			continue
		}
		v := *n
		if old := left[i]; old != nil {
			if *old > math.MaxInt64-v {
				return Usage{}, errorf(CodeProviderError, "accumulated token usage overflow")
			}
			v += *old
		}
		*dst[i] = &v
	}
	return out, nil
}
