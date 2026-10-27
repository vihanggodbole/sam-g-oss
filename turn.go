package sam

import (
	"database/sql"
	"embed"
	"encoding/json"
)

//go:embed prompts/*.md
var promptFS embed.FS

func prompt(name string) string {
	b, err := promptFS.ReadFile("prompts/" + name)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// userInput is what a person sends to start a turn: text, images or both.
type userInput struct {
	text   string
	images []Image
}

func (in userInput) empty() bool { return in.text == "" && len(in.images) == 0 }

// startTurn accepts genuine input, the operation and its next action together.
// Callers reserve a worker before releasing the transition lock.
func (r *Runtime) startTurn(tx *sql.Tx, s *sessionRow, in userInput, source string) (string, error) {
	models, err := r.selectWorkModels(s)
	if err != nil {
		return "", err
	}
	m := models.main
	if len(in.images) > 0 && m.NoImages {
		return "", errorf(CodeInvalidArgument, "model %q cannot read images; send text, or choose a model that can see images", m.Alias)
	}
	if err := bindModel(tx, s, m); err != nil {
		return "", err
	}
	id := newID("turn")
	settings := mustJSON(map[string]any{"model": m.binding(), "account": s.cfg.Account, "config": s.cfg})
	if _, err := tx.ExecContext(bg, `INSERT INTO turns (id,session_id,status,source,started_at,instructions,settings)
 VALUES (?,?,'running',?,?,?,?)`, id, s.id, source, nowMillis(), s.instructions, settings); err != nil {
		return "", err
	}
	if err := touchActivity(tx, s.id); err != nil {
		return "", err
	}
	reasoning := "unavailable"
	if s.cfg.ReasoningSummary {
		reasoning = "summaries"
	}
	p := TurnStartedPayload{Source: source, Prompt: in.text, Images: len(in.images), Model: m.Alias, Account: s.cfg.Account, Reasoning: reasoning}
	if err := appendEvent(tx, event(s.id, EventTurnStarted, p).turn(id)); err != nil {
		return "", err
	}
	if err := applyPending(tx, s.id, id); err != nil {
		return "", err
	}
	if !in.empty() {
		if _, err := appendHistory(tx, s.id, id, kindUser, "", userMessage(in.text, in.images...)); err != nil {
			return "", err
		}
	}
	if err := saveExecution(tx, &execution{SessionID: s.id, TurnID: id, Action: actionPrepare, Step: 1, Progress: freshProgress()}); err != nil {
		return "", err
	}
	return id, setPaused(tx, s.id, false)
}

func bindModel(tx *sql.Tx, s *sessionRow, m resolvedModel) error {
	if s.binding != nil {
		return nil
	}
	b := m.binding()
	s.binding = &b
	s.pinSelection()
	_, err := tx.ExecContext(bg, `UPDATE sessions SET bound_model=?,bound_provider=?,bound_base_url=?,bound_model_id=?,bound_api=?,overrides=? WHERE id=?`, b.Model, b.Provider, b.BaseURL, b.ModelID, b.API, mustJSON(s.overrides), s.id)
	return err
}

func applyPending(tx *sql.Tx, sid, tid string) error {
	type message struct{ id, text string }
	var messages []message
	err := queryRows(tx, func(rows *sql.Rows) error {
		var m message
		err := rows.Scan(&m.id, &m.text)
		messages = append(messages, m)
		return err
	}, `SELECT id,text FROM steering WHERE session_id=? AND status='pending' ORDER BY seq`, sid)
	if err != nil {
		return err
	}
	for _, m := range messages {
		if _, err := appendHistory(tx, sid, tid, kindSteering, "", userMessage(m.text)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(bg, `UPDATE steering SET status='applied',turn_id=? WHERE id=?`, tid, m.id); err != nil {
			return err
		}
		if err := appendEvent(tx, event(sid, EventSteeringApplied, SteeringPayload{SteeringID: m.id, Text: m.text}).turn(tid)); err != nil {
			return err
		}
	}
	return nil
}

func pendingCount(q querier, sid string) (int, error) {
	var n int
	err := q.QueryRowContext(bg, `SELECT COUNT(*) FROM steering WHERE session_id=? AND status='pending'`, sid).Scan(&n)
	return n, err
}

// turnRun is live turn state, including a whole-turn account pin. It is never
// restored from an old settings snapshot on recovery.
type turnRun struct {
	sessionID, turnID, workspace string
	cfg                          SessionConfig
	model                        resolvedModel
	summaryModel                 resolvedModel
	summaryError                 error
	tools                        map[string]Tool
	toolList                     []Tool
	instructions                 string
	step, attempt                int
	pendingTurn                  hookRun
	turnCollected                bool
	initiator                    string
}

func (r *Runtime) prepareTurn(sessionID, turnID string, models workModels) (*turnRun, error) {
	s, err := loadSession(r.db, sessionID)
	if err != nil {
		return nil, err
	}
	m, err := r.withAccount(models.main, s.cfg.Account)
	if err != nil {
		return nil, err
	}
	summary, summaryErr := r.withAccount(models.summary, models.summary.account.name)
	if summaryErr == nil && summary.auth == m.auth && summary.account.name == m.account.name {
		// The summarizer must use the main turn's login generation, not a
		// replacement login that appeared between the two account resolutions.
		summary.account = m.account
	}
	t := &turnRun{summaryModel: summary, summaryError: summaryErr, sessionID: sessionID, turnID: turnID, workspace: s.workspace, cfg: s.cfg.withDefaults(), model: m, instructions: s.instructions, tools: map[string]Tool{}}
	t.toolList = r.enabledTools(s.cfg)
	for _, tool := range t.toolList {
		t.tools[tool.Name] = tool
	}
	return t, nil
}

func (r *Runtime) loadView(sid string) (*sessionRow, *contextView, error) {
	s, err := loadSession(r.db, sid)
	if err != nil {
		return nil, nil, storageErr(err)
	}
	view, err := loadContext(r.db, s)
	return s, view, storageErr(err)
}

// saveOutput saves complete provider output and planned calls. Its caller also
// ends the attempt and advances/finishes the execution in this transaction.
func saveOutput(tx *sql.Tx, t *turnRun, res *modelResult) (int, error) {
	count := 0
	for _, raw := range res.output {
		var it outputItem
		if err := json.Unmarshal(raw, &it); err != nil {
			return 0, err
		}
		if _, err := appendHistory(tx, t.sessionID, t.turnID, kindOutput, t.model.provenance(), raw); err != nil {
			return 0, err
		}
		if text := it.refusal(); text != "" {
			e := event(t.sessionID, EventRefusal, RefusalPayload{Code: codeModelRefusal, Text: text}).turn(t.turnID)
			e.Step, e.Attempt = t.step, t.attempt
			if err := appendEvent(tx, e); err != nil {
				return 0, err
			}
		}
		if it.Type != "function_call" {
			continue
		}
		target, ok := res.dispatch[it.CallID]
		if !ok {
			return 0, errorf(CodeProviderError, "completed tool call has no saved dispatch authorization")
		}
		_, err := tx.ExecContext(bg, `INSERT INTO tool_calls (session_id,turn_id,call_id,idx,name,arguments,status,step,namespace,dispatch_target,replay_unsafe)
  VALUES (?,?,?,?,?,?,'planned',?,?,?,?)`, t.sessionID, t.turnID, it.CallID, count, it.Name, it.Arguments, t.step, it.Namespace, target, t.tools[target].ReplayUnsafe)
		if err != nil {
			return 0, err
		}
		count++
	}
	return count, recordUsage(tx, t.sessionID, t.turnID, "turns", t.turnID, res.usage, false)
}

const codeModelRefusal = "model_refusal"

func historyIncompatible(m resolvedModel, detail string) string {
	who := "model " + m.Alias
	if m.auth != "" {
		who = "account " + m.account.name + " of " + m.auth
	}
	return "the provider rejected saved conversation history for " + who + " (" + detail + "). History is unchanged and the session is paused. " +
		"Opaque reasoning saved under one account may not be accepted by another account. To continue, select the account that produced it " +
		"with an explicit idle SwitchModel handoff, or start a new session. Compaction does not remove recent reasoning from requests."
}

func recordUsage(tx *sql.Tx, sid, tid, table, id string, u Usage, compaction bool) error {
	if compaction && u == (Usage{}) {
		return nil
	}
	if err := u.validate(); err != nil {
		return err
	}
	var a, b, c, read, write sql.NullInt64
	if err := tx.QueryRowContext(bg, "SELECT input_tokens, output_tokens, reasoning_tokens, cache_read_tokens, cache_write_tokens FROM "+table+" WHERE id = ?", id).Scan(&a, &b, &c, &read, &write); err != nil {
		return err
	}
	total, err := addUsage(scanUsage(a, b, c, read, write), u)
	if err != nil {
		return err
	}
	ia, oa, ra, cr, cw := nullableUsage(total)
	if _, err := tx.ExecContext(bg, "UPDATE "+table+" SET input_tokens = ?, output_tokens = ?, reasoning_tokens = ?, cache_read_tokens = ?, cache_write_tokens = ? WHERE id = ?", ia, oa, ra, cr, cw, id); err != nil {
		return err
	}
	ev := event(sid, EventUsage, UsagePayload{Usage: u, Compaction: compaction}).turn(tid)
	x, err := loadExecution(tx, sid)
	if err != nil {
		return err
	}
	if x != nil {
		ev.Step, ev.Attempt = x.Step, x.Progress.Attempts
		if compaction {
			ev.OperationID = x.CompactionID
		}
	}
	return appendEvent(tx, ev)
}

type partialOutput struct{ text, reasoning string }

func (p partialOutput) empty() bool { return p.text == "" && p.reasoning == "" }
func appendPartialNote(tx *sql.Tx, sid, tid string, p partialOutput) error {
	note := "[Runtime note] Your previous response was interrupted before it completed."
	if p.reasoning != "" {
		note += "\n\nThe partial reasoning summary shown to the user was:\n\n" + p.reasoning
	}
	if p.text != "" {
		note += "\n\nThe partial text shown to the user was:\n\n" + p.text
	}
	_, err := appendHistory(tx, sid, tid, kindNote, "", userMessage(note))
	return err
}

func pauseIdle(tx *sql.Tx, s *sessionRow, pause bool, reason string) error {
	if err := setPaused(tx, s.id, s.paused || pause); err != nil {
		return err
	}
	if pause && !s.paused {
		return appendEvent(tx, event(s.id, EventSessionPaused, map[string]string{"reason": reason}))
	}
	return nil
}
