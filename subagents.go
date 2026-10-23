package sam

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
)

// StartSubagentRequest creates an ordinary child session and accepts its first
// turn atomically. Workspace and omitted Config fields inherit parent settings.
// Selecting another model requires an explicit account (empty for API-key models)
// and resets inherited reasoning/reserve to defaults before applying overrides.
// Disabled tools can only be added at creation; children cannot delegate.
// Workspaces must already exist; no workspace or edit coordination is performed.
type StartSubagentRequest struct {
	RequestID string            `json:"request_id"`
	ParentID  string            `json:"parent_id"`
	Name      string            `json:"name"`
	Prompt    string            `json:"prompt"`
	Workspace string            `json:"workspace,omitempty"`
	Config    *SessionOverrides `json:"config,omitempty"`
}

// Subagent is a child session with its latest saved turn outcome. While work is
// running/suspended, LastTurn is not a claim that the entire child is finished.
type Subagent struct {
	Session        Session `json:"session"`
	Account        string  `json:"account"`
	LastTurn       *Turn   `json:"last_turn,omitempty"`
	LastHistorySeq int64   `json:"last_history_seq"`
}

// StartSubagent returns immediately after durable acceptance, independently of
// the caller's lifetime. Reusing RequestID returns the same child and never
// restarts its work. Resume rejoins suspended child executions after restart.
func (r *Runtime) StartSubagent(ctx context.Context, req StartSubagentRequest) (TurnAck, error) {
	var ack TurnAck
	if req.Config != nil {
		req.Config = new(req.Config.snapshot())
	}
	if !r.configMu.TryLock() {
		return ack, errorf(CodeConflict, "configuration preparation is in progress; retry after it finishes")
	}
	defer r.configMu.Unlock()
	r.mu.Lock()
	err := r.admit()
	var saved json.RawMessage
	if err == nil {
		err = r.admitTx(func(tx *sql.Tx) error {
			var err error
			saved, _, err = dedupe(tx, req.RequestID, "start_subagent", req)
			return err
		})
	}
	p := *r.preparedConfig
	p.files = maps.Clone(p.files)
	r.mu.Unlock()
	if err != nil {
		return ack, err
	}
	if saved != nil {
		err := json.Unmarshal(saved, &ack)
		return ack, err
	}
	s, err := r.prepareSubagent(ctx, &p, req)
	if err != nil {
		return ack, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ack, err
	}
	fresh, err := r.mutateFresh(req.RequestID, "start_subagent", req, &ack, func(tx *sql.Tx) error {
		x, err := loadExecution(tx, req.ParentID)
		if err != nil {
			return err
		}
		if x != nil && x.Action == actionStop {
			return errorf(CodeConflict, "parent is stopping")
		}
		parent, err := loadSession(tx, req.ParentID)
		if err != nil {
			return err
		}
		if parent.isSubagent {
			return errorf(CodeUnsupported, "child sessions cannot start subagents")
		}
		s.id = newID("ses")
		s.name = req.Name
		if s.name == "" {
			s.name = s.id
		}
		now := nowMillis()
		_, err = tx.ExecContext(bg, `INSERT INTO sessions
   (id,name,workspace,config,overrides,instructions,compaction_prompt,resources,created_at,updated_at,activity_at,tree_activity_at,parent_id,is_subagent)
   VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,1)`, s.id, s.name, s.workspace, mustJSON(s.cfg), mustJSON(s.overrides),
			s.instructions, s.compactionPrompt, mustJSON(s.resources), now, now, now, now, req.ParentID)
		if err != nil {
			return err
		}
		ack.SessionID = s.id
		ack.TurnID, err = r.startTurn(tx, s, req.Prompt, "prompt")
		if err != nil {
			return err
		}
		return appendEvent(tx, event(req.ParentID, EventSubagentStarted, ack))
	})
	if fresh {
		r.files = p.files
		r.launch(ack.SessionID, false)
	}
	return ack, err
}

func (r *Runtime) prepareSubagent(ctx context.Context, p *preparedConfig, req StartSubagentRequest) (*sessionRow, error) {
	if req.Prompt == "" {
		return nil, errorf(CodeInvalidArgument, "prompt is required")
	}
	parent, err := loadSession(r.reader(ctx), req.ParentID)
	if err != nil {
		return nil, readErr(ctx, err)
	}
	if parent.isSubagent {
		return nil, errorf(CodeUnsupported, "child sessions cannot start subagents")
	}
	// resolveBound rejects changed aliases instead of silently changing providers.
	if _, err := p.resolveBound(parent); err != nil {
		return nil, err
	}
	cfg, err := childConfig(parent.cfg, req.Config)
	if err != nil {
		return nil, err
	}
	workspace := req.Workspace
	if workspace == "" {
		workspace = parent.workspace
	}
	overrides := explicitConfig(cfg)
	cfg, err = r.newSessionConfig(CreateSessionRequest{Workspace: workspace, Config: &overrides})
	if err != nil {
		return nil, err
	}
	s := &sessionRow{cfg: cfg, overrides: overrides, workspace: filepath.Clean(workspace), parentID: req.ParentID, isSubagent: true}
	return s, p.preparePrompts(s)
}

func childConfig(parent SessionConfig, overrides *SessionOverrides) (SessionConfig, error) {
	cfg := parent
	if overrides != nil {
		if overrides.Model != nil && *overrides.Model != parent.Model {
			if overrides.Account == nil {
				return cfg, errorf(CodeInvalidArgument, "selecting another model requires config.account (empty for an API-key model)")
			}
			cfg.ReasoningEffort, cfg.ReasoningSummary = "", false
			cfg.Compaction.OutputReserve = 0
		}
		cfg = overrides.apply(cfg)
	}
	// A child's settings can narrow the tool set, never widen its parent's.
	cfg.DisabledTools = slices.Clone(cfg.DisabledTools)
	for _, name := range append(slices.Clone(parent.DisabledTools), "subagents") {
		if !slices.Contains(cfg.DisabledTools, name) {
			cfg.DisabledTools = append(cfg.DisabledTools, name)
		}
	}
	return cfg, nil
}

// childrenIdle runs under the transition mutex and caller's transaction, so a
// child cannot be accepted between this check and deleting its parent.
func (r *Runtime) childrenIdle(q querier, parent string) error {
	var unfinished bool
	err := queryRows(q, func(rows *sql.Rows) error {
		var id string
		var saved bool
		if err := rows.Scan(&id, &saved); err != nil {
			return err
		}
		unfinished = unfinished || saved || r.workers[id] != nil
		return nil
	}, `WITH RECURSIVE owned(id) AS (SELECT id FROM sessions WHERE parent_id=? UNION ALL
 SELECT s.id FROM sessions s JOIN owned o ON s.parent_id=o.id)
 SELECT id, EXISTS(SELECT 1 FROM executions x WHERE x.session_id=owned.id) FROM owned`, parent)
	if err != nil {
		return err
	}
	if unfinished {
		return errorf(CodeChildrenBusy, "this parent has unfinished child work; stop the children and wait before deleting it")
	}
	return nil
}

// Subagents lists direct owned children. Ordinary GetSession, History, Events,
// Steer and Interrupt operate on the returned session IDs; there is no second
// child-control protocol. Models are restricted to direct ownership by the tool.
func (r *Runtime) Subagents(ctx context.Context, parentID string) ([]Subagent, error) {
	if err := r.readable(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	db := r.reader(ctx)
	if _, err := loadSession(db, parentID); err != nil {
		return nil, readErr(ctx, err)
	}
	var ids []string
	err := queryRows(db, func(rows *sql.Rows) error {
		var id string
		err := rows.Scan(&id)
		ids = append(ids, id)
		return err
	}, `SELECT id FROM sessions WHERE parent_id=? ORDER BY created_at,id`, parentID)
	if err != nil {
		return nil, readErr(ctx, err)
	}
	out := []Subagent{}
	for _, id := range ids {
		child, err := r.subagentView(db, id)
		if err != nil {
			return nil, readErr(ctx, err)
		}
		out = append(out, child)
	}
	return out, nil
}

func (r *Runtime) subagentView(q querier, id string) (Subagent, error) {
	var out Subagent
	var err error
	out.Session, err = r.sessionView(q, id)
	if err != nil {
		return out, err
	}
	err = q.QueryRowContext(bg, `SELECT COALESCE(json_extract(config,'$.account'),''),
  COALESCE((SELECT MAX(seq) FROM history WHERE session_id=?1),0) FROM sessions WHERE id=?1`, id).
		Scan(&out.Account, &out.LastHistorySeq)
	if err != nil {
		return out, err
	}
	out.LastTurn, err = lastSubagentTurn(q, id)
	if out.LastTurn != nil && out.LastTurn.Status == "running" && r.workers[id] == nil {
		out.LastTurn.Status = PhaseSuspended
	}
	return out, err
}

func lastSubagentTurn(q querier, sid string) (*Turn, error) {
	var t Turn
	var started int64
	var ended sql.NullInt64
	err := q.QueryRowContext(bg, `SELECT id,status,source,reason,error,started_at,ended_at
  FROM turns WHERE session_id=? ORDER BY rowid DESC LIMIT 1`, sid).
		Scan(&t.ID, &t.Status, &t.Source, &t.Reason, &t.Error, &started, &ended)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	t.StartedAt = fromMillis(started)
	if ended.Valid {
		t.EndedAt = new(fromMillis(ended.Int64))
	}
	return &t, err
}

func ownedSubagent(q querier, parent, child string) error {
	var one int
	err := q.QueryRowContext(bg, `SELECT 1 FROM sessions WHERE id=? AND parent_id=?`, child, parent).Scan(&one)
	if err == sql.ErrNoRows {
		return errorf(CodeNotFound, "subagent is not an owned child of this session")
	}
	return err
}
