package sam

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// busyTimeout is how long SQLite waits for a lock held by another
// connection before reporting contention.
var busyTimeout = 5 * time.Second

var (
	errInterrupted = errors.New("interrupted by user")
	errShutdown    = errors.New("runtime shutdown")
	errStorage     = errorf(CodeStorageError, "work stopped after a persistence failure")
)

// Runtime owns one SQLite database and runs the sessions stored in it. It is
// safe to use without an HTTP server; it opens no listener and installs no
// process signal handlers. The host is responsible for keeping it open while
// work should continue and calling Close during shutdown.
type Runtime struct {
	*preparedConfig
	configMu  sync.Mutex // serialize preparation, never queue reload
	reloading bool       // admission reservation, protected by mu
	db        *sql.DB
	lock      *os.File
	client    *modelClient
	diags     []Diagnostic
	diagMu    sync.RWMutex
	fileLock  sync.RWMutex
	procs     procTable // background processes started by bash
	log       *slog.Logger

	ctx        context.Context // runtime lifetime; ends subscriptions
	cancel     context.CancelFunc
	workCtx    context.Context // parent of all session work
	workCancel context.CancelCauseFunc

	mu      sync.Mutex // admission and state transitions
	workers map[string]*worker
	closing bool
	closed  bool
	broken  atomic.Pointer[Error]

	stopped  chan struct{} // closed when the shutdown operation finished
	closeErr error         // its result, written before stopped closes
}

type worker struct {
	cancel         context.CancelCauseFunc
	done           chan struct{}
	models         workModels // definitions captured under mu at admission, not persisted
	selectionError error
	main           *turnRun
	prepared       *preparedRequest // immutable; publication/inspection under mu
	compaction     *compactionSpec
	rejected       string // rejected credential, live memory only
	recovered      bool
}

// Open snapshots cfg, validates it, acquires exclusive ownership of the
// database, applies migrations, loads latest configuration and reconciles
// unfinished work as suspended without launching it. Resume explicitly opts
// into recovery with possibly repeated external effects. Legacy unfinished
// records without checkpoints are interrupted. A competing owner fails.
// ctx bounds opening only; it does not become the lifetime of returned work.
// Open does not start a server or change process signal handling.
func Open(ctx context.Context, cfg Config) (*Runtime, error) {
	cfg = cfg.clone()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return nil, errorf(CodeInvalidArgument, "database path: %v", err)
	}
	// Keep the configured database identity stable even if the host later chdirs.
	cfg.DatabasePath = path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, storageErr(err)
	}
	path, lock, err := lockOwned(path, "database")
	if err != nil {
		return nil, storageErr(err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err == nil {
		db.SetMaxOpenConns(1)
		err = migrate(ctx, db)
	}
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		_ = lock.Close()
		return nil, storageErr(err)
	}
	r := newRuntime(cfg, db, lock)
	if err := r.validateDefaults(); err != nil {
		_ = r.release()
		return nil, err
	}
	if err := r.prepareOpen(ctx); err != nil {
		_ = r.release()
		return nil, err
	}
	if err := r.recover(); err != nil {
		_ = r.release()
		return nil, err
	}
	return r, nil
}

// sqliteDSN builds an escaped file: URI so that characters such as ?, #
// and % stay part of the filename protected by the ownership lock.
// Connection pragmas are separate query values. Transactions begin with
// BEGIN IMMEDIATE, taking the write lock up front: upgrading a deferred
// read transaction to a write can fail with SQLITE_BUSY at once, without
// the busy wait.
func sqliteDSN(path string) string {
	q := url.Values{"_txlock": {"immediate"}, "_pragma": {
		"foreign_keys(1)", "journal_mode(WAL)", "synchronous(FULL)",
		fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()),
	}}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

func newRuntime(cfg Config, db *sql.DB, lock *os.File) *Runtime {
	r := &Runtime{preparedConfig: &preparedConfig{cfg: cfg, files: map[string]string{}}, db: db, lock: lock, workers: map[string]*worker{}, log: slog.Default()}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	r.client = &modelClient{
		http: hc, auth: cfg.Auth, backoff: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		maxEvent: cmp.Or(cfg.MaxProviderEventBytes, DefaultMaxProviderEventBytes),
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.workCtx, r.workCancel = context.WithCancelCause(r.ctx)
	r.procs.onFailure = r.recordProcessFailure
	r.diags = r.preparedConfig.registerTools(cfg.Tools, r)
	return r
}

// Diagnostic is a structured problem that did not prevent Open or was
// discovered after an operation was accepted. Category is a presentation hint;
// Code is the stable machine-readable classification.
type Diagnostic struct {
	Category  string `json:"category"`
	Code      string `json:"code"`
	Tool      string `json:"tool,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Message   string `json:"message"`
}

// Diagnostics returns a snapshot of startup problems and asynchronous
// lifecycle failures, including invalid host-tool registrations and uncertain
// background-process cleanup.
func (r *Runtime) Diagnostics() []Diagnostic {
	r.diagMu.RLock()
	defer r.diagMu.RUnlock()
	return slices.Clone(r.diags)
}

func (r *Runtime) recordProcessFailure(sessionID string, err error) {
	d := Diagnostic{
		Category:  CategoryError,
		Code:      string(CodeProcessObservation),
		Tool:      "bash",
		SessionID: sessionID,
		Message:   err.Error(),
	}
	r.diagMu.Lock()
	r.diags = append(r.diags, d)
	r.diagMu.Unlock()
	r.log.Error("sam: bash process cleanup could not be confirmed", "session_id", sessionID, "error", err)
}

// registerTools installs the built-ins, then host tools, which replace a
// built-in of the same name. An invalid host registration is skipped with a
// diagnostic and disables its name entirely, including a built-in it tried
// to replace, so a broken override never silently falls back.
func (r *preparedConfig) registerTools(host []Tool, owner *Runtime) []Diagnostic {
	var diags []Diagnostic
	r.tools, r.skipped = map[string]Tool{}, map[string]bool{}
	var valid []Tool
	for i, t := range host {
		msg := toolProblem(t)
		if msg == "" {
			valid = append(valid, t)
			continue
		}
		if t.Name == "" {
			msg = fmt.Sprintf("tool registration %d: %s", i, msg)
		}
		r.skipped[t.Name] = true
		d := Diagnostic{Category: CategoryError, Code: "invalid_tool", Tool: t.Name, Message: msg + "; the tool was skipped"}
		diags = append(diags, d)
	}
	for _, t := range append(append(fileTools(&owner.fileLock), bashTool(&owner.procs), historyTool(owner), subagentsTool(owner)), valid...) {
		if r.skipped[t.Name] {
			continue
		}
		if i := slices.IndexFunc(r.toolList, func(x Tool) bool { return x.Name == t.Name }); i >= 0 {
			r.toolList[i] = t
		} else {
			r.toolList = append(r.toolList, t)
		}
		r.tools[t.Name] = t
	}
	return diags
}

// toolProblem returns why a host tool registration is unusable, or "".
func toolProblem(t Tool) string {
	if t.Name == "" {
		return "name is empty"
	}
	if t.Run == nil {
		return "Run is nil"
	}
	var schema map[string]any
	if err := json.Unmarshal(t.Parameters, &schema); err != nil || schema["type"] != "object" {
		return `parameters must be a JSON schema object with "type": "object"`
	}
	return ""
}

func (r *preparedConfig) validateDefaults() error {
	if r.cfg.Defaults.Model == "" {
		return r.validateSessionValues(r.cfg.Defaults)
	}
	return r.validateStoredConfig(r.cfg.Defaults)
}

func (r *Runtime) release() error {
	r.cancel()
	err := r.db.Close()
	if lerr := r.lock.Close(); err == nil {
		err = lerr
	}
	return err
}

// Close seals admission, cancels/joins workers and suspends eligible progress
// (unlike durable user Interrupt). Known results remain saved. It stops
// background processes where ownership can be confirmed, then closes storage
// and releases ownership. An observation failure is returned rather than
// reported as successful cleanup. All of this is one shutdown operation,
// started by the first Close; every Close call waits for that same operation
// and returns its result. If ctx ends first, Close returns a closed error
// while shutdown continues in the background: the runtime keeps its database
// until every worker settles and each process is reaped or explicitly
// relinquished with an observation error, then releases it. Call Close again
// to wait for the final result.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped == nil {
		r.closing = true
		r.workCancel(errShutdown)
		var dones []chan struct{}
		for _, w := range r.workers {
			dones = append(dones, w.done)
		}
		r.stopped = make(chan struct{})
		go r.shutdown(dones)
	}
	stopped := r.stopped
	r.mu.Unlock()
	select {
	case <-stopped:
		return r.closeErr
	case <-ctx.Done():
		return errorf(CodeClosed, "close timed out waiting for active work or background processes to stop; "+
			"shutdown continues and the runtime keeps its database until it finishes. Call Close again to wait for it")
	}
}

// shutdown is the single shutdown operation: no worker is left to launch a
// background process once dones are closed, and only this goroutine closes
// storage and releases ownership.
func (r *Runtime) shutdown(dones []chan struct{}) {
	for _, d := range dones {
		<-d
	}
	processErr := r.procs.stopAll()
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.closeErr = errors.Join(processErr, storageErr(r.release()))
	close(r.stopped)
}

// admit reports whether new work may be accepted. Callers hold r.mu.
func (r *Runtime) admit() error {
	if r.closing {
		return errClosed
	}
	if e := r.broken.Load(); e != nil {
		return e
	}
	if r.reloading {
		return errorf(CodeConflict, "configuration reload is being prepared; retry after it finishes")
	}
	return nil
}

// Ready reports whether the runtime currently accepts new work. It returns nil
// while ready, CodeClosed after Close begins, or CodeStorageError if persistence
// failed and admission was stopped. A transient storage-busy rejection does not
// make the runtime unhealthy. During configuration preparation it returns a
// transient CodeConflict; retry after preparation finishes.
func (r *Runtime) Ready() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.admit()
}

func (r *Runtime) readable() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errClosed
	}
	return nil
}

// storageFailed stops admission and cancels all active work after a
// persistence failure. It never takes r.mu.
func (r *Runtime) storageFailed(err error) error {
	e := &Error{Code: CodeStorageError, Message: "persistence failed: " + err.Error()}
	if r.broken.CompareAndSwap(nil, e) {
		r.log.Error("sam: persistence failed; admission stopped and active work cancelled", "error", err)
		r.workCancel(errStorage)
	}
	return e
}

// launch starts a session worker. Callers hold r.mu and have committed the
// transition that makes the session busy.
func (r *Runtime) launch(sessionID string, recovered bool) {
	ctx, cancel := context.WithCancelCause(r.workCtx)
	w := &worker{cancel: cancel, done: make(chan struct{}), recovered: recovered}
	r.captureWork(sessionID, w)
	r.workers[sessionID] = w
	go r.executeSession(ctx, sessionID, w)
}

// detachWorker runs under mu after producers and tool calls have joined. The
// identity check prevents an exiting worker from touching a newer reservation.
func (r *Runtime) detachWorker(sid string, w *worker) {
	if r.workers[sid] != w {
		return
	}
	w.cancel(nil)
	w.main, w.prepared, w.compaction = nil, nil, nil
	w.rejected = ""
	delete(r.workers, sid)
	close(w.done)
}

// recover converts only legacy work without checkpoints to interrupted, then
// reconciles new-format executions without launching them. Historical PIDs
// are never adopted or signalled.
func (r *Runtime) recover() error {
	return r.tx(func(tx *sql.Tx) error {
		var ids []string
		err := queryRows(tx, func(rows *sql.Rows) error {
			var id string
			err := rows.Scan(&id)
			ids = append(ids, id)
			return err
		}, `SELECT id FROM sessions WHERE id NOT IN (SELECT session_id FROM executions) AND (
			id IN (SELECT session_id FROM turns WHERE status = 'running')
			OR id IN (SELECT session_id FROM compactions WHERE state = 'running')
			OR id IN (SELECT session_id FROM tool_calls WHERE history_seq IS NULL))`)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := recoverSession(tx, id); err != nil {
				return err
			}
		}
		if err := r.reconcileExecutions(tx); err != nil {
			return err
		}
		return r.recoverSubagents(tx)
	})
}

func recoverSession(tx *sql.Tx, id string) error {
	var turnIDs []string
	err := queryRows(tx, func(rows *sql.Rows) error {
		var t string
		err := rows.Scan(&t)
		turnIDs = append(turnIDs, t)
		return err
	}, "SELECT id FROM turns WHERE session_id = ? AND status = 'running'", id)
	if err != nil {
		return err
	}
	now := nowMillis()
	for _, t := range turnIDs {
		if err := noteUnfinishedOutput(tx, id, t); err != nil {
			return err
		}
		if _, err := tx.ExecContext(bg, `UPDATE turns SET status = 'interrupted', reason = 'recovery', ended_at = ? WHERE id = ?`, now, t); err != nil {
			return err
		}
		if err := appendEvent(tx, event(id, EventTurnInterrupted, TurnEndedPayload{Reason: "recovery"}).turn(t)); err != nil {
			return err
		}
	}
	if err := recoverCompactions(tx, id, now); err != nil {
		return err
	}
	if err := closeOpenCalls(tx, id); err != nil {
		return err
	}
	if err := setPaused(tx, id, true); err != nil {
		return err
	}
	return appendEvent(tx, event(id, EventSessionPaused, map[string]string{"reason": "recovery"}))
}

func recoverCompactions(tx *sql.Tx, sessionID string, now int64) error {
	type running struct{ id, turnID, model string }
	var ops []running
	err := queryRows(tx, func(rows *sql.Rows) error {
		var c running
		err := rows.Scan(&c.id, &c.turnID, &c.model)
		ops = append(ops, c)
		return err
	}, "SELECT id, COALESCE(turn_id, ''), model FROM compactions WHERE session_id = ? AND state = 'running'", sessionID)
	if err != nil {
		return err
	}
	const msg = "runtime stopped during compaction"
	for _, c := range ops {
		if _, err := tx.ExecContext(bg, "UPDATE compactions SET state = 'interrupted', error = ?, ended_at = ? WHERE id = ?", msg, now, c.id); err != nil {
			return err
		}
		var b opModel
		_ = json.Unmarshal([]byte(c.model), &b) // display only
		// Automatic compactions run inside a turn; manual ones have no turn.
		p := CompactionPayload{Model: b.Model, Account: b.Account, Automatic: c.turnID != "", Error: msg}
		if err := appendEvent(tx, event(sessionID, EventCompactionCancelled, p).turn(c.turnID).op(c.id)); err != nil {
			return err
		}
	}
	return nil
}

// noteUnfinishedOutput is only for legacy records without attempt identity.
// It adds the interrupted-response runtime note when the
// turn's last published events are answer, refusal, or reasoning-summary
// deltas that no completed model step followed (each completed step records
// a usage event), so the next model request knows partial output may have
// been shown.
func noteUnfinishedOutput(tx *sql.Tx, sessionID, turnID string) error {
	rows, err := tx.QueryContext(bg, "SELECT kind, payload FROM events WHERE session_id = ? AND turn_id = ? ORDER BY seq DESC",
		sessionID, turnID)
	if err != nil {
		return err
	}
	var text, reasoning []string
	for rows.Next() {
		var k, p string
		if err := rows.Scan(&k, &p); err != nil {
			rows.Close()
			return err
		}
		var d DeltaPayload
		if (k != EventTextDelta && k != EventRefusalDelta && k != EventReasoningDelta) || json.Unmarshal([]byte(p), &d) != nil {
			break
		}
		if k == EventReasoningDelta {
			reasoning = append(reasoning, d.Text)
		} else {
			text = append(text, d.Text)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(text)+len(reasoning) == 0 {
		return err
	}
	slices.Reverse(text)
	slices.Reverse(reasoning)
	return appendPartialNote(tx, sessionID, turnID, partialOutput{strings.Join(text, ""), strings.Join(reasoning, "")})
}

// ProviderInfo describes a configured provider without credentials. For a
// login provider (Auth set), HasCredential reports whether any account of
// that provider is logged in; list them with AuthManager.Accounts.
type ProviderInfo struct {
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	Auth          string `json:"auth,omitempty"`
	APIKeyEnv     string `json:"api_key_env,omitempty"`
	HasCredential bool   `json:"has_credential"`
}

// ModelInfo describes a configured model and its declared capabilities.
type ModelInfo struct {
	Name string `json:"name"`
	Model
}

// Providers lists configured providers in name order. It returns credential
// references and whether a credential is currently present, never the secret
// itself; no provider discovery request is made.
func (r *Runtime) Providers() []ProviderInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []ProviderInfo{}
	for _, name := range sortedKeys(r.cfg.Providers) {
		p := r.cfg.Providers[name]
		has := p.APIKey != "" || (p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "")
		if p.Auth != "" {
			has = r.cfg.Auth.hasAccounts(p.Auth)
		}
		out = append(out, ProviderInfo{Name: name, BaseURL: p.BaseURL, Auth: p.Auth, APIKeyEnv: p.APIKeyEnv, HasCredential: has})
	}
	return out
}

// Models lists configured models in name order, including the capabilities
// declared by the host. No remote catalog is queried.
func (r *Runtime) Models() []ModelInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []ModelInfo{}
	for _, name := range sortedKeys(r.cfg.Models) {
		m := r.cfg.Models[name]
		m.ReasoningEfforts = slices.Clone(m.ReasoningEfforts)
		if m.CatalogAccount != nil {
			m.CatalogAccount = new(*m.CatalogAccount)
		}
		out = append(out, ModelInfo{Name: name, Model: m})
	}
	return out
}

// Auth returns the AuthManager from Config.Auth, or nil. The runtime uses
// it but does not own it.
func (r *Runtime) Auth() *AuthManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.Auth
}

// Tools lists registered tools and copies of their argument schemas. It does
// not expose host implementations or run any tools.
func (r *Runtime) Tools() []ToolInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ToolInfo, 0, len(r.toolList))
	for _, t := range r.toolList {
		out = append(out, ToolInfo{Name: t.Name, Description: t.Description, Parameters: bytes.Clone(t.Parameters), ReplayUnsafe: t.ReplayUnsafe})
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// enabledTools returns the session's enabled tools in registration order.
func (r *preparedConfig) enabledTools(c SessionConfig) []Tool {
	var out []Tool
	for _, t := range r.toolList {
		if !slices.Contains(c.DisabledTools, t.Name) {
			out = append(out, t)
		}
	}
	return out
}
