package sam

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type compactionSpec struct {
	id            string
	model         resolvedModel
	prompt        string
	automatic     bool
	instructions  string
	tools         []Tool
	pending       []hookNote
	step, attempt int
}
type budget struct{ input, trigger, recent, reserve int }

func budgets(m resolvedModel, c SessionConfig) budget {
	c = c.withDefaults()
	reserve := m.outputReserve(c.Compaction.OutputReserve)
	in := max(0, m.ContextTokens-reserve)
	return budget{input: in, trigger: in * c.Compaction.TriggerPercent / 100, recent: in * c.Compaction.RecentPercent / 100, reserve: reserve}
}

func (r *preparedConfig) resolveCompaction(s *sessionRow, id string, automatic bool) (*compactionSpec, error) {
	var m resolvedModel
	var err error
	if alias := s.cfg.Compaction.Model; alias != "" {
		m, err = r.resolve(alias)
	} else {
		m, err = r.resolveBound(s)
	}
	if err != nil {
		return nil, err
	}
	main, _ := r.resolve(s.cfg.Model)
	m.account = accountRef{name: s.cfg.compactionAccount(main, m)}
	return &compactionSpec{id: id, model: m, automatic: automatic}, nil
}

type opModel struct {
	ModelBinding
	Account string `json:"account,omitempty"`
}

func decodeOpModel(raw string) (opModel, error) {
	var m opModel
	err := json.Unmarshal([]byte(raw), &m)
	m.API = m.API.effective()
	return m, err
}

func beginCompaction(tx *sql.Tx, sid, tid string, spec *compactionSpec) error {
	if _, err := tx.ExecContext(bg, `INSERT INTO compactions (id,session_id,turn_id,state,prompt,model,started_at)
 VALUES (?,?,NULLIF(?,''),'running','',?,?)`, spec.id, sid, tid, mustJSON(opModel{spec.model.binding(), spec.model.accountName()}), nowMillis()); err != nil {
		return err
	}
	return appendEvent(tx, event(sid, EventCompactionStarted, CompactionPayload{Model: spec.model.Alias, Account: spec.model.accountName(), Automatic: spec.automatic}).turn(tid).op(spec.id))
}

// selectCut retains a minimum recent estimate, expanding backwards for intact
// tool/result and reasoning groups. It never keeps an entire long turn merely
// because its genuine initiating user is old.
func (v *contextView) selectCut(recent int) int {
	items := v.items
	n := len(items)
	valid := make([]bool, n+1)
	open := map[string]bool{}
	for i, h := range items {
		it := h.parsed()
		switch it.Type {
		case "function_call":
			open[it.CallID] = true
		case "function_call_output":
			delete(open, it.CallID)
		}
		valid[i+1] = len(open) == 0 && it.Type != "reasoning"
		if it.Native != nil && i+1 < n {
			next := items[i+1].parsed()
			if next.Native != nil && next.Native.MessageID == it.Native.MessageID {
				valid[i+1] = false
			}
		}
	}
	minCut, suffix := n, 0
	for i := n - 1; i >= 0; i-- {
		minCut = i
		if raw, ok := v.project(items[i]); ok {
			suffix += modelVisibleTokens(raw)
		}
		if suffix >= recent {
			break
		}
	}
	for p := minCut; p > 0; p-- {
		if valid[p] {
			return p
		}
	}
	return 0
}

func modelVisibleTokens(raw json.RawMessage) int {
	projected, ok := projectItem(raw)
	if !ok {
		return 0
	}
	return estimateTokens(modelInputBytes(projected))
}
func (h histItem) parsed() outputItem { var it outputItem; _ = json.Unmarshal(h.item, &it); return it }

type summaryResult struct {
	summary            string
	covered, protected int64
}

func installSummary(tx *sql.Tx, sid, tid string, spec *compactionSpec, res *summaryResult) error {
	if _, err := tx.ExecContext(bg, `UPDATE compactions SET state='installed',covered_seq=?,protected_user_seq=NULLIF(?,0),summary=?,ended_at=? WHERE id=?`, res.covered, res.protected, res.summary, nowMillis(), spec.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(bg, `UPDATE sessions SET summary_id=? WHERE id=?`, spec.id, sid); err != nil {
		return err
	}
	p := CompactionPayload{Model: spec.model.Alias, Account: spec.model.accountName(), Automatic: spec.automatic, CoveredSeq: res.covered}
	return appendEvent(tx, event(sid, EventCompactionCompleted, p).turn(tid).op(spec.id))
}

func rawInput(items ...any) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		out = append(out, json.RawMessage(mustJSON(it)))
	}
	return out
}

func outputText(output []json.RawMessage) (text, refusal string) {
	var sb, rb strings.Builder
	for _, raw := range output {
		var it outputItem
		if json.Unmarshal(raw, &it) != nil || it.Type != "message" {
			continue
		}
		for _, c := range it.Content {
			sb.WriteString(c.Text)
		}
		rb.WriteString(it.refusal())
	}
	return sb.String(), rb.String()
}

func portableText(view *contextView, items []histItem) string {
	var sb strings.Builder
	if view.summary != "" {
		fmt.Fprintf(&sb, "Summary of even earlier conversation:\n%s\n\n", view.summary)
	}
	sb.WriteString("Conversation to summarize:\n")
	for _, h := range items {
		if line := h.portable(); line != "" {
			sb.WriteString("\n" + line + "\n")
		}
	}
	return sb.String()
}
