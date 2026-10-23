package sam

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Session is an inspectable snapshot of a session. Phase describes current
// work; Paused controls whether pending steering may start automatically.
// Usage counters are nullable because a provider may omit them.
type Session struct {
	ID                 string        `json:"id"`
	ParentID           string        `json:"parent_id,omitempty"`
	IsSubagent         bool          `json:"is_subagent,omitempty"`
	Name               string        `json:"name"`
	Workspace          string        `json:"workspace"`
	Phase              string        `json:"phase"` // idle, running, compacting, suspended, stopping
	Paused             bool          `json:"paused"`
	Binding            *ModelBinding `json:"binding,omitempty"`
	ModelError         string        `json:"model_error,omitempty"`
	ActiveTurnID       string        `json:"active_turn_id,omitempty"`
	ActiveCompactionID string        `json:"active_compaction_id,omitempty"`
	ActiveTools        []ActiveTool  `json:"active_tools"`
	QueuedTools        int           `json:"queued_tools"`
	PendingSteering    int           `json:"pending_steering"`
	LastEventSeq       int64         `json:"last_event_seq"`
	ReplayAfterSeq     int64         `json:"replay_after_seq"` // event cursor for rebuilding current active work; idle sessions use LastEventSeq
	Usage              Usage         `json:"usage"`
	CompactionUsage    Usage         `json:"compaction_usage"`
	// Latest usage is projected from the newest ended provider attempt in each
	// scope, including failures without usage. It never falls back to older data.
	LatestUsage           *Usage    `json:"latest_usage"`
	LatestCompactionUsage *Usage    `json:"latest_compaction_usage"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// ActiveTool is a running tool call.
type ActiveTool struct {
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Turn is a turn record with its status, per-turn usage, and frozen
// instructions. A turn may fail after StartTurn has returned successfully.
type Turn struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"` // running, suspended, completed, cancelled, failed, legacy interrupted
	Source       string     `json:"source"` // prompt, continue, steering, follow_up
	Reason       string     `json:"reason,omitempty"`
	Error        string     `json:"error,omitempty"`
	Instructions string     `json:"instructions,omitempty"`
	Usage        Usage      `json:"usage"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
}

// CreateSessionRequest creates a session for an existing absolute workspace
// directory. RequestID is required and makes a retry of the identical request
// return the original session. Config records optional overrides; nil fields
// inherit runtime defaults. Explicit empty Model is invalid, not inheritance.
type CreateSessionRequest struct {
	RequestID string            `json:"request_id"`
	Name      string            `json:"name"`
	Workspace string            `json:"workspace"`
	Config    *SessionOverrides `json:"config,omitempty"`
}

// ListQuery pages through sessions, or through turns when SessionID is set.
// After continues forward; Before continues backward ("0" selects the newest
// turn page). They cannot be combined. Summary omits frozen turn instructions.
// Limit 0 or an out-of-range value uses the documented maximum.
type ListQuery struct {
	SessionID string // for ListTurns
	After     string
	Before    string // for ListTurns; "0" selects the newest page
	Summary   bool   // for ListTurns; omit frozen instructions from each record
	Limit     int    // default and maximum 200
}

// SessionPage is a page of sessions. Pass Next as ListQuery.After to continue;
// an empty Next means the listing is complete.
type SessionPage struct {
	Sessions []Session `json:"sessions"`
	Next     string    `json:"next,omitempty"`
}

// RenameSessionRequest renames a session.
type RenameSessionRequest struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

// DeleteSessionRequest deletes an idle session and all its records.
type DeleteSessionRequest struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
}

// UpdateConfigRequest replaces a session's entire override set. Omitted fields
// resume inheritance. Only the target session must be fully idle. Its effective
// settings and prompt resources are refreshed atomically; other chats are untouched.
// RequestID is required and must be reused only for an identical retry.
type UpdateConfigRequest struct {
	RequestID string           `json:"request_id"`
	SessionID string           `json:"session_id"`
	Config    SessionOverrides `json:"config"`
}

// CreateSession validates the workspace and configuration and saves a
// session. The workspace must already be an absolute directory; it is a working
// directory, not a sandbox. Validation follows request deduplication, so a
// retried create returns the saved session even if its workspace was removed.
func (r *Runtime) CreateSession(ctx context.Context, req CreateSessionRequest) (Session, error) {
	if req.Config != nil {
		req.Config = new(req.Config.snapshot())
	}
	var out Session
	if !r.configMu.TryLock() {
		return out, errorf(CodeConflict, "configuration preparation is in progress; retry after it finishes")
	}
	defer r.configMu.Unlock()
	r.mu.Lock()
	err := r.admit()
	var saved json.RawMessage
	if err == nil {
		err = r.admitTx(func(tx *sql.Tx) error {
			var err error
			saved, _, err = dedupe(tx, req.RequestID, "create_session", req)
			return err
		})
	}
	p := *r.preparedConfig
	p.files = maps.Clone(p.files)
	r.mu.Unlock()
	if err != nil {
		return out, err
	}
	if saved != nil {
		err := json.Unmarshal(saved, &out)
		return out, err
	}
	cfg, err := r.newSessionConfig(req)
	if err != nil {
		return out, err
	}
	s := &sessionRow{cfg: cfg, workspace: filepath.Clean(req.Workspace)}
	if req.Config != nil {
		s.overrides = *req.Config
	}
	if err := p.preparePrompts(s); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err = r.mutate(req.RequestID, "create_session", req, &out, func(tx *sql.Tx) error {
		id, now := newID("ses"), nowMillis()
		name := req.Name
		if name == "" {
			name = id
		}
		if _, err := tx.ExecContext(bg, `INSERT INTO sessions (id, name, workspace, config, overrides, instructions, compaction_prompt, resources, created_at, updated_at, activity_at, tree_activity_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, name, s.workspace, mustJSON(s.cfg), mustJSON(s.overrides), s.instructions, s.compactionPrompt, mustJSON(s.resources), now, now, now, now); err != nil {
			return err
		}
		var err error
		out, err = r.sessionView(tx, id)
		return err
	})
	if err == nil {
		r.files = p.files
	}
	return out, err
}

// newSessionConfig validates a create request and returns its effective config.
func (r *Runtime) newSessionConfig(req CreateSessionRequest) (SessionConfig, error) {
	cfg := r.cfg.Defaults
	if req.Config != nil {
		cfg = req.Config.apply(cfg)
	}
	if !filepath.IsAbs(req.Workspace) {
		return cfg, errorf(CodeInvalidArgument, "workspace must be an absolute path")
	}
	if st, err := os.Stat(req.Workspace); err != nil || !st.IsDir() {
		return cfg, errorf(CodeInvalidArgument, "workspace %s is not an existing directory", req.Workspace)
	}
	if cfg.Model == "" {
		return cfg, errorf(CodeInvalidArgument, "no model selected and no default model configured")
	}
	return cfg, r.validateSessionConfig(cfg)
}

// mutate runs a deduplicated mutation under the transition lock. When the
// request ID was already accepted, the saved acknowledgement is decoded into
// ack and fn is not run.
func (r *Runtime) mutate(requestID, op string, args, ack any, fn func(tx *sql.Tx) error) error {
	_, err := r.mutateFresh(requestID, op, args, ack, fn)
	return err
}

func (r *Runtime) mutateFresh(requestID, op string, args, ack any, fn func(tx *sql.Tx) error) (fresh bool, err error) {
	if err := r.admit(); err != nil {
		return false, err
	}
	err = r.admitTx(func(tx *sql.Tx) error {
		saved, hash, err := dedupe(tx, requestID, op, args)
		if err != nil || saved != nil {
			if saved != nil && ack != nil {
				return json.Unmarshal(saved, ack)
			}
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		fresh = true
		return saveRequest(tx, requestID, op, hash, ack)
	})
	return fresh && err == nil, err
}

// ListSessions pages through sessions in creation order. Limit 0 or above
// 200 means 200.
func (r *Runtime) ListSessions(ctx context.Context, q ListQuery) (SessionPage, error) {
	if err := r.readable(); err != nil {
		return SessionPage{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	db := r.reader(ctx)
	limit := pageLimit(q.Limit)
	var ids []string
	var last int64
	err := queryRows(db, func(rows *sql.Rows) error {
		var id string
		err := rows.Scan(&last, &id)
		ids = append(ids, id)
		return err
	}, "SELECT rowid, id FROM sessions WHERE rowid > ? ORDER BY rowid LIMIT ?", cursor(q.After), limit)
	if err != nil {
		return SessionPage{}, readErr(ctx, err)
	}
	page := SessionPage{Sessions: []Session{}}
	for _, id := range ids {
		s, err := r.sessionView(db, id)
		if err != nil {
			return SessionPage{}, readErr(ctx, err)
		}
		page.Sessions = append(page.Sessions, s)
	}
	if len(ids) == limit {
		page.Next = strconv.FormatInt(last, 10)
	}
	return page, nil
}

func pageLimit(n int) int {
	if n <= 0 || n > 200 {
		return 200
	}
	return n
}

func cursor(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// GetSession returns a session snapshot including active work and usage.
func (r *Runtime) GetSession(ctx context.Context, id string) (Session, error) {
	if err := r.readable(); err != nil {
		return Session{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Session{}, readErr(ctx, err)
	}
	defer tx.Rollback()
	s, err := r.sessionView(tx, id)
	if err != nil {
		return Session{}, readErr(ctx, err)
	}
	return s, readErr(ctx, tx.Commit())
}

func (r *Runtime) sessionView(q querier, id string) (Session, error) {
	s, err := loadSession(q, id)
	if err != nil {
		return Session{}, err
	}
	v := Session{
		ID: s.id, ParentID: s.parentID, IsSubagent: s.isSubagent, Name: s.name, Workspace: s.workspace, Phase: s.phase, Paused: s.paused, Binding: s.binding,
		ActiveTools: []ActiveTool{}, CreatedAt: fromMillis(s.createdAt), UpdatedAt: fromMillis(s.updatedAt),
	}
	if _, err := r.resolveBound(s); err != nil {
		v.ModelError = err.Error()
	}
	x, err := loadExecution(q, id)
	if err != nil {
		return Session{}, err
	}
	v.Phase = executionPhase(x, r.workers[id] != nil)
	var ia, oa, ra, readA, writeA, ic, oc, rc, readC, writeC sql.NullInt64
	var latest, latestCompaction sql.NullString
	err = q.QueryRowContext(bg, `SELECT
		COALESCE((SELECT turn_id FROM executions WHERE session_id = ?1), ''),
		COALESCE((SELECT compaction_id FROM executions WHERE session_id = ?1), ''),
		(SELECT COUNT(*) FROM steering WHERE session_id = ?1 AND status = 'pending'),
		(SELECT COUNT(*) FROM tool_calls WHERE session_id = ?1 AND status = 'planned' AND history_seq IS NULL),
		(SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = ?1),
		(SELECT SUM(input_tokens) FROM turns WHERE session_id = ?1),
		(SELECT SUM(output_tokens) FROM turns WHERE session_id = ?1),
		(SELECT SUM(reasoning_tokens) FROM turns WHERE session_id = ?1),
		(SELECT SUM(cache_read_tokens) FROM turns WHERE session_id = ?1),
		(SELECT SUM(cache_write_tokens) FROM turns WHERE session_id = ?1),
		(SELECT SUM(input_tokens) FROM compactions WHERE session_id = ?1),
		(SELECT SUM(output_tokens) FROM compactions WHERE session_id = ?1),
		(SELECT SUM(reasoning_tokens) FROM compactions WHERE session_id = ?1),
		(SELECT SUM(cache_read_tokens) FROM compactions WHERE session_id = ?1),
		(SELECT SUM(cache_write_tokens) FROM compactions WHERE session_id = ?1),
		(SELECT json_extract(payload, '$.usage') FROM events WHERE session_id = ?1
		 AND kind = 'model.attempt.ended' AND operation_id = '' ORDER BY seq DESC LIMIT 1),
		(SELECT json_extract(payload, '$.usage') FROM events WHERE session_id = ?1
		 AND kind = 'model.attempt.ended' AND operation_id != '' ORDER BY seq DESC LIMIT 1)`, id).
		Scan(&v.ActiveTurnID, &v.ActiveCompactionID, &v.PendingSteering, &v.QueuedTools, &v.LastEventSeq,
			&ia, &oa, &ra, &readA, &writeA, &ic, &oc, &rc, &readC, &writeC, &latest, &latestCompaction)
	if err != nil {
		return Session{}, err
	}
	v.Usage, v.CompactionUsage = scanUsage(ia, oa, ra, readA, writeA), scanUsage(ic, oc, rc, readC, writeC)
	v.ReplayAfterSeq, err = replayAfterSeq(q, id, x, v.LastEventSeq)
	if err != nil {
		return Session{}, err
	}
	if latest.Valid {
		if err := json.Unmarshal([]byte(latest.String), &v.LatestUsage); err != nil {
			return Session{}, err
		}
	}
	if latestCompaction.Valid {
		if err := json.Unmarshal([]byte(latestCompaction.String), &v.LatestCompactionUsage); err != nil {
			return Session{}, err
		}
	}
	rows, err := q.QueryContext(bg, `SELECT call_id, name, arguments FROM tool_calls
		WHERE session_id = ? AND status = 'started' AND history_seq IS NULL ORDER BY rowid`, id)
	if err != nil {
		return Session{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var a ActiveTool
		var args string
		if err := rows.Scan(&a.CallID, &a.Name, &args); err != nil {
			return Session{}, err
		}
		a.Arguments = rawArgs(args)
		v.ActiveTools = append(v.ActiveTools, a)
	}
	return v, rows.Err()
}

// replayAfterSeq starts at the active model attempt, tool batch, or compaction,
// rather than replaying earlier completed work in the same turn.
func replayAfterSeq(q querier, sessionID string, x *execution, last int64) (int64, error) {
	if x == nil {
		return last, nil
	}
	var query string
	var args []any
	switch {
	case x.CompactionID != "":
		query = `SELECT MIN(seq) FROM events WHERE session_id=? AND operation_id=? AND kind=?`
		args = []any{sessionID, x.CompactionID, EventCompactionStarted}
	case x.Action == actionBatch || (x.Action == actionStop && !x.Progress.InFlight):
		query = `SELECT MIN(seq) FROM events WHERE session_id=? AND turn_id=? AND step=? AND kind=?`
		args = []any{sessionID, x.TurnID, x.Step, EventToolStarted}
	case x.Progress.InFlight:
		query = `SELECT MIN(seq) FROM events WHERE session_id=? AND turn_id=? AND operation_id='' AND step=? AND attempt=? AND kind=?`
		args = []any{sessionID, x.TurnID, x.Step, x.Progress.Attempts, EventModelAttemptStarted}
	default:
		return last, nil
	}
	var start sql.NullInt64
	if err := q.QueryRowContext(bg, query, args...).Scan(&start); err != nil {
		return 0, err
	}
	if start.Valid && start.Int64 > 0 && start.Int64-1 < last {
		return start.Int64 - 1, nil
	}
	return last, nil
}

// ListTurns pages through a session's turns in start order. Use the returned
// cursor as ListQuery.After. Before pages return the nearest older records in
// the same order and omit frozen instructions when Summary is true. Limit 0 or
// above 200 means 200.
func (r *Runtime) ListTurns(ctx context.Context, q ListQuery) ([]Turn, string, error) {
	if err := r.readable(); err != nil {
		return nil, "", err
	}
	if _, err := loadSession(r.reader(ctx), q.SessionID); err != nil {
		return nil, "", readErr(ctx, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if q.Before != "" && q.After != "" {
		return nil, "", errorf(CodeInvalidArgument, "after and before cannot be used together")
	}
	before := int64(0)
	if q.Before != "" {
		var err error
		before, err = strconv.ParseInt(q.Before, 10, 64)
		if err != nil || before < 0 {
			return nil, "", errorf(CodeInvalidArgument, "before cursor must be a non-negative decimal row ID")
		}
	}
	limit := pageLimit(q.Limit)
	instructions := "''"
	if !q.Summary {
		instructions = "COALESCE(instructions, '')"
	}
	query := `SELECT rowid, id, status, source, reason, error, ` + instructions + `,
		input_tokens, output_tokens, reasoning_tokens, cache_read_tokens, cache_write_tokens, started_at, ended_at
		FROM turns WHERE session_id = ? AND rowid > ? ORDER BY rowid LIMIT ?`
	args := []any{q.SessionID, cursor(q.After), limit}
	if q.Before != "" {
		query = `SELECT rowid, id, status, source, reason, error, ` + instructions + `,
			input_tokens, output_tokens, reasoning_tokens, cache_read_tokens, cache_write_tokens, started_at, ended_at
			FROM turns WHERE session_id = ? ORDER BY rowid DESC LIMIT ?`
		args = []any{q.SessionID, limit + 1}
		if before > 0 {
			query = `SELECT rowid, id, status, source, reason, error, ` + instructions + `,
				input_tokens, output_tokens, reasoning_tokens, cache_read_tokens, cache_write_tokens, started_at, ended_at
				FROM turns WHERE session_id = ? AND rowid < ? ORDER BY rowid DESC LIMIT ?`
			args = []any{q.SessionID, before, limit + 1}
		}
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", readErr(ctx, err)
	}
	defer rows.Close()
	turns := make([]Turn, 0, limit+1)
	rowIDs := make([]int64, 0, limit+1)
	var last int64
	for rows.Next() {
		var t Turn
		var rowID int64
		var a, b, c, read, write, ended sql.NullInt64
		var started int64
		if err := rows.Scan(&rowID, &t.ID, &t.Status, &t.Source, &t.Reason, &t.Error, &t.Instructions,
			&a, &b, &c, &read, &write, &started, &ended); err != nil {
			return nil, "", readErr(ctx, err)
		}
		last = rowID
		rowIDs = append(rowIDs, rowID)
		t.Usage, t.StartedAt = scanUsage(a, b, c, read, write), fromMillis(started)
		if ended.Valid {
			e := fromMillis(ended.Int64)
			t.EndedAt = &e
		}
		if t.Status == "running" && r.workers[q.SessionID] == nil {
			t.Status = PhaseSuspended
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", readErr(ctx, err)
	}
	if q.Before != "" {
		hasOlder := len(turns) > limit
		if hasOlder {
			turns = turns[:limit]
		}
		for left, right := 0, len(turns)-1; left < right; left, right = left+1, right-1 {
			turns[left], turns[right] = turns[right], turns[left]
			rowIDs[left], rowIDs[right] = rowIDs[right], rowIDs[left]
		}
		if hasOlder && len(turns) > 0 {
			return turns, strconv.FormatInt(rowIDs[0], 10), nil
		}
		return turns, "", nil
	}
	next := ""
	if len(turns) == limit {
		next = strconv.FormatInt(last, 10)
	}
	return turns, next, nil
}

// RenameSession changes a session's display name. Allowed at any time.
func (r *Runtime) RenameSession(ctx context.Context, req RenameSessionRequest) (Session, error) {
	if req.Name == "" {
		return Session{}, errorf(CodeInvalidArgument, "name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out Session
	err := r.mutate(req.RequestID, "rename_session", req, &out, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(bg, "UPDATE sessions SET name = ?, updated_at = ? WHERE id = ?", req.Name, nowMillis(), req.SessionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNotFound
		}
		if err := appendEvent(tx, event(req.SessionID, EventSessionRenamed, map[string]string{"name": req.Name})); err != nil {
			return err
		}
		out, err = r.sessionView(tx, req.SessionID)
		return err
	})
	return out, err
}

// DeleteSession requires this session and its owned children to have no
// unfinished work. It deletes only this session's history/events, preserving
// child histories and their permanent non-delegating role, then stops background processes its bash calls started in this runtime and waits
// until they are reaped or ctx ends. Log files and workspace files are left
// alone. If ctx ends, cleanup continues and Close waits for it. The method can
// return process_observation_failed after deletion committed when process
// ownership is uncertain; it does not claim the command stopped.
func (r *Runtime) DeleteSession(ctx context.Context, req DeleteSessionRequest) error {
	r.mu.Lock()
	_, err := r.mutateFresh(req.RequestID, "delete_session", req, nil, func(tx *sql.Tx) error {
		s, err := loadSession(tx, req.SessionID)
		if err != nil {
			return err
		}
		if s.phase != PhaseIdle || r.workers[s.id] != nil {
			return errBusy
		}
		if err := r.childrenIdle(tx, s.id); err != nil {
			return err
		}
		_, err = tx.ExecContext(bg, "DELETE FROM sessions WHERE id = ?", s.id)
		return err
	})
	r.mu.Unlock()
	if err != nil {
		return err
	}
	// Outside r.mu: this can take the kill grace. If ctx ends first the
	// processes are still stopped, and Close waits for them.
	return r.procs.stopSession(ctx, req.SessionID)
}

// GetConfig returns detached effective settings and explicit override intent.
// Edit Overrides and pass it to UpdateConfig; omitted fields resume inheritance.
func (r *Runtime) GetConfig(ctx context.Context, sessionID string) (SessionSettings, error) {
	if err := r.readable(); err != nil {
		return SessionSettings{}, err
	}
	s, err := loadSession(r.reader(ctx), sessionID)
	if err != nil {
		return SessionSettings{}, readErr(ctx, err)
	}
	return SessionSettings{Effective: s.cfg.withDefaults(), Overrides: s.overrides, Resources: s.resources}, nil
}

// UpdateConfig replaces overrides and refreshes prompt resources for one fully
// idle session. Other sessions may keep working. The main model/account of a
// used chat changes only through explicit SwitchModel. A child session can never
// enable native delegation. Read errors leave its previous settings installed.
func (r *Runtime) UpdateConfig(ctx context.Context, req UpdateConfigRequest) (SessionSettings, error) {
	cfg := req.Config.snapshot()
	return r.applySessionConfiguration(ctx, SessionRequest{RequestID: req.RequestID, SessionID: req.SessionID}, &cfg)
}

// ContextInfo describes the effective instructions and context estimates.
// Token counts are conservative estimates, not tokenizer results.
type ContextInfo struct {
	Resources         PreparedResources `json:"resources"`
	InstructionsState string            `json:"instructions_state"` // prepared or base_only
	Instructions      string            `json:"instructions"`
	Model             ModelBinding      `json:"model"`
	ModelError        string            `json:"model_error,omitempty"`
	CompactionModel   string            `json:"compaction_model"`
	CompactionError   string            `json:"compaction_error,omitempty"` // Current model/settings selection failure; no provider probe.
	ContextTokens     int               `json:"context_tokens"`
	OutputReserve     int               `json:"output_reserve"`
	InputBudget       int               `json:"input_budget"`
	TriggerTokens     int               `json:"trigger_tokens"`
	RecentBudget      int               `json:"recent_budget"`
	EstimatedTokens   int               `json:"estimated_tokens"`
	AutoCompaction    bool              `json:"auto_compaction"`
	Summary           string            `json:"summary,omitempty"`
	CompactedThrough  int64             `json:"compacted_through_seq,omitempty"`
}

// InspectContext reports the worker's current main instructions/settings
// (prepared), or installed base instructions (base_only), without reading files.
// Suspended work uses latest installed settings, not historical turn snapshots;
// summarizer instructions and private callback candidates are never exposed. Estimates
// include current retained history, protected user, summary and tools, but not
// uncommitted pending guidance; they are not the exact next request size.
func (r *Runtime) InspectContext(ctx context.Context, sessionID string) (ContextInfo, error) {
	if err := r.readable(); err != nil {
		return ContextInfo{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ContextInfo{}, readErr(ctx, err)
	}
	defer tx.Rollback()
	s, err := loadSession(tx, sessionID)
	if err != nil {
		return ContextInfo{}, readErr(ctx, err)
	}
	state := "base_only"
	if w := r.workers[sessionID]; w != nil && w.prepared != nil {
		p := w.prepared
		s.cfg, s.instructions, state = p.mainConfig, p.mainInstructions, "prepared"
	}
	view, err := loadContext(tx, s)
	if err != nil {
		return ContextInfo{}, readErr(ctx, err)
	}
	info := ContextInfo{Resources: s.resources, InstructionsState: state, Summary: view.summary, CompactedThrough: view.coveredSeq, AutoCompaction: !s.cfg.Compaction.DisableAuto}
	info.CompactionModel = s.cfg.Compaction.Model
	m, err := r.resolveBound(s)
	if w := r.workers[sessionID]; w != nil && w.selectionError == nil {
		m, err = w.models.main, nil
	}
	if err != nil {
		info.ModelError = err.Error()
	}
	info.Model = m.binding()
	if info.CompactionModel == "" {
		info.CompactionModel = m.Alias
	}
	var selectionErr error
	if w := r.workers[sessionID]; w != nil {
		selectionErr = w.selectionError
	} else {
		_, selectionErr = r.selectWorkModels(s)
	}
	if selectionErr != nil {
		info.CompactionError = selectionErr.Error()
	}
	info.Instructions = s.instructions
	b := budgets(m, s.cfg)
	info.ContextTokens, info.OutputReserve, info.InputBudget = m.ContextTokens, b.reserve, b.input
	info.TriggerTokens, info.RecentBudget = b.trigger, b.recent
	info.EstimatedTokens = estimateRequest(info.Instructions, view.input(), r.enabledTools(s.cfg))
	return info, nil
}
