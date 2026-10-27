package sam

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// HistoryQuery selects canonical history items after a sequence number, or
// before a sequence number when Before is non-nil. A zero Before selects the
// newest page. Backward pages are returned in canonical ascending order.
type HistoryQuery struct {
	SessionID string
	After     int64
	Before    *int64
	Limit     int // default and maximum 500
}

// HistoryItem is one visible canonical history entry. Tool results include
// their saved tool-call identity when available. Opaque provider data such as
// encrypted reasoning is never included.
type HistoryItem struct {
	Source     *ExtensionSource `json:"source,omitempty"` // application attribution, never human provenance
	Seq        int64            `json:"seq"`
	TurnID     string           `json:"turn_id,omitempty"`
	Kind       string           `json:"kind"` // user, steering, output, tool_result, note, extension
	Type       string           `json:"type"` // message, reasoning, function_call, function_call_output
	Category   string           `json:"category"`
	Code       string           `json:"code,omitempty"`   // model_refusal for a refused answer
	Status     string           `json:"status,omitempty"` // tool results: completed, failed, cancelled, uncertain, not_executed
	Role       string           `json:"role,omitempty"`
	Text       string           `json:"text,omitempty"`
	Images     int              `json:"images,omitempty"` // images sent with a user message; their data is not returned
	CallID     string           `json:"call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	Namespace  string           `json:"namespace,omitempty"`
	Arguments  string           `json:"arguments,omitempty"`
	Provenance string           `json:"provenance,omitempty"`
	Time       time.Time        `json:"time"`
}

// HistoryPage is a page of history. Next continues the existing forward API;
// Older continues backward from Before, with items still in ascending sequence
// order and omitted when no still-older entries remain.
type HistoryPage struct {
	Items []HistoryItem `json:"items"`
	Next  int64         `json:"next,omitempty"`
	Older int64         `json:"older,omitempty"`
}

type histItem struct {
	source        *ExtensionSource
	seq           int64
	turnID        string
	kind          string
	provenance    string
	item          json.RawMessage
	created       int64
	callStatus    string // outcome of a tool_result item's call
	callName      string
	callNamespace string
	callArguments string
}

// histFrom selects a session's history rows with their tool-call outcome.
// Queries append further conditions, ordering, and limits.
const histFrom = ` FROM history h LEFT JOIN tool_calls c ON c.session_id = h.session_id AND c.history_seq = h.seq
	WHERE h.session_id = ?`

// Item column expressions: rawItem is the stored provider item, needed for
// model requests; visibleItem drops opaque encrypted reasoning inside SQLite,
// so visible reads never load it.
const (
	rawItem     = `h.item`
	visibleItem = `json_remove(h.item, '$.encrypted_content', '$.native')`
)

func histSelect(item string) string {
	return `SELECT h.seq, COALESCE(h.turn_id, ''), h.kind, h.provenance, ` + item + `, h.created_at, COALESCE(c.status, ''), COALESCE(h.source, 'null'), COALESCE(c.name, ''), COALESCE(c.namespace, ''), COALESCE(c.arguments, '')` + histFrom
}

// eachHistory calls fn for every row of a histSelect query while the rows
// are open. fn must not use the database.
func eachHistory(q querier, fn func(histItem) error, query string, args ...any) error {
	return queryRows(q, func(rows *sql.Rows) error {
		var h histItem
		var item, source string
		if err := rows.Scan(&h.seq, &h.turnID, &h.kind, &h.provenance, &item, &h.created, &h.callStatus, &source, &h.callName, &h.callNamespace, &h.callArguments); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(source), &h.source); err != nil {
			return err
		}
		h.item = json.RawMessage(item)
		return fn(h)
	}, query, args...)
}

func loadHistory(q querier, sessionID, item string, after int64, limit int) ([]histItem, error) {
	query := histSelect(item) + " AND h.seq > ? ORDER BY h.seq"
	args := []any{sessionID, after}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	var out []histItem
	err := eachHistory(q, func(h histItem) error { out = append(out, h); return nil }, query, args...)
	return out, err
}

// History returns a page of visible canonical history. Limit 0 or above
// 500 means 500.
func (r *Runtime) History(ctx context.Context, q HistoryQuery) (HistoryPage, error) {
	if err := r.readable(); err != nil {
		return HistoryPage{}, err
	}
	db := r.reader(ctx)
	if _, err := loadSession(db, q.SessionID); err != nil {
		return HistoryPage{}, readErr(ctx, err)
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	if q.After < 0 || (q.Before != nil && *q.Before < 0) {
		return HistoryPage{}, errorf(CodeInvalidArgument, "history cursors must be non-negative")
	}
	if q.Before != nil && q.After != 0 {
		return HistoryPage{}, errorf(CodeInvalidArgument, "after and before cannot be used together")
	}
	var items []histItem
	var hasOlder bool
	var err error
	if q.Before == nil {
		items, err = loadHistory(db, q.SessionID, visibleItem, q.After, limit)
	} else {
		items, hasOlder, err = loadHistoryBefore(db, q.SessionID, visibleItem, *q.Before, limit)
	}
	if err != nil {
		return HistoryPage{}, readErr(ctx, err)
	}
	page := HistoryPage{Items: make([]HistoryItem, 0, len(items))}
	for _, h := range items {
		page.Items = append(page.Items, h.visible())
	}
	if q.Before != nil {
		if hasOlder && len(items) > 0 {
			page.Older = items[0].seq
		}
	} else if len(items) == limit {
		page.Next = items[len(items)-1].seq
	}
	return page, nil
}

// loadHistoryBefore reads the nearest preceding page using the history index.
// It fetches one sentinel row to determine whether more older records exist.
func loadHistoryBefore(q querier, sessionID, item string, before int64, limit int) ([]histItem, bool, error) {
	query := histSelect(item) + " AND h.seq < ? ORDER BY h.seq DESC LIMIT ?"
	args := []any{sessionID, before, limit + 1}
	if before == 0 {
		query = histSelect(item) + " ORDER BY h.seq DESC LIMIT ?"
		args = []any{sessionID, limit + 1}
	}
	out := make([]histItem, 0, limit+1)
	err := eachHistory(q, func(h histItem) error { out = append(out, h); return nil }, query, args...)
	if err != nil {
		return nil, false, err
	}
	hasOlder := len(out) > limit
	if hasOlder {
		out = out[:limit]
	}
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out, hasOlder, nil
}

func (h histItem) visible() HistoryItem {
	var it outputItem
	_ = json.Unmarshal(h.item, &it)
	v := HistoryItem{
		Source: h.source, Seq: h.seq, TurnID: h.turnID, Kind: h.kind, Type: it.Type, Category: CategoryOutput, Role: it.Role,
		CallID: it.CallID, Name: it.Name, Namespace: it.Namespace, Arguments: it.Arguments, Provenance: h.provenance, Time: fromMillis(h.created),
	}
	switch it.Type {
	case "reasoning":
		parts := make([]string, 0, len(it.Summary))
		for _, s := range it.Summary {
			parts = append(parts, s.Text)
		}
		v.Text = strings.Join(parts, reasoningPartSeparator)
	case "function_call_output":
		v.Text, v.Status = it.Output, h.callStatus
		if v.Name == "" {
			v.Name = h.callName
		}
		if v.Namespace == "" {
			v.Namespace = h.callNamespace
		}
		if v.Arguments == "" {
			v.Arguments = h.callArguments
		}
		switch h.callStatus {
		case "completed":
		case "cancelled":
			v.Category = CategoryDiagnostic
		default:
			v.Category = CategoryError
		}
	default:
		v.Text, v.Images = it.text(), it.imageCount()
		if it.refusal() != "" {
			v.Category, v.Code = CategoryError, codeModelRefusal
		}
	}
	if h.kind == kindNote {
		v.Category = CategoryDiagnostic
	}
	return v
}

// portable renders an item as plain text for summarization.
func (h histItem) portable() string {
	v := h.visible()
	switch v.Type {
	case "reasoning":
		if v.Text == "" {
			return ""
		}
		return "Assistant reasoning summary: " + v.Text
	case "function_call":
		return "Tool call " + v.CallID + ": " + v.Name + " " + v.Arguments
	case "function_call_output":
		return "Tool result " + v.CallID + ": " + v.Text
	}
	if v.Role == "assistant" {
		return "Assistant: " + v.Text
	}
	if h.kind == kindExtension {
		return "Application extension (" + v.Role + "): " + v.Text
	}
	if h.kind == kindNote {
		return v.Text
	}
	return "User: " + withImageNotes(v.Text, v.Images)
}

// projectItem prepares a stored item for a model request. Reasoning items are
// replayed only with their encrypted content; other items drop provider IDs,
// which are not retrievable when requests use store=false.
func projectItem(raw json.RawMessage) (json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	var typ string
	_ = json.Unmarshal(m["type"], &typ)
	if string(m["api"]) == `"anthropic_messages"` {
		return raw, true
	}
	if typ == "reasoning" {
		var enc string
		_ = json.Unmarshal(m["encrypted_content"], &enc)
		return raw, enc != ""
	}
	if _, ok := m["id"]; !ok {
		return raw, true
	}
	delete(m, "id")
	delete(m, "status")
	return json.RawMessage(mustJSON(m)), true
}

// contextView is the model-visible context of a session: installed summary,
// the fixed protected user entry selected at installation, and later items.
type contextView struct {
	catalog         string
	portableThrough int64
	summary         string
	coveredSeq      int64
	protectedUser   *histItem
	items           []histItem
}

func loadContext(q querier, s *sessionRow) (*contextView, error) {
	v := &contextView{portableThrough: s.modelHistoryStart, catalog: s.resources.SkillCatalog}
	var protected int64
	if s.summaryID != "" {
		err := q.QueryRowContext(bg, "SELECT summary, covered_seq, COALESCE(protected_user_seq, 0) FROM compactions WHERE id = ?", s.summaryID).
			Scan(&v.summary, &v.coveredSeq, &protected)
		if err != nil {
			return nil, err
		}
	}
	var err error
	if v.items, err = loadHistory(q, s.id, rawItem, v.coveredSeq, 0); err != nil {
		return nil, err
	}
	if protected != 0 {
		err := eachHistory(q, func(h histItem) error {
			v.protectedUser = &h
			return nil
		}, histSelect(rawItem)+" AND h.seq = ?", s.id, protected)
		if err != nil {
			return nil, err
		}
		if v.protectedUser == nil {
			return nil, errors.New("installed summary references a missing protected user entry")
		}
	}
	return v, nil
}

// precedingUser selects by canonical source, not provider role. An extension
// or runtime note using a user wire role must never become a human anchor.
func precedingUser(q querier, sessionID string, through int64) (*histItem, error) {
	var user *histItem
	err := eachHistory(q, func(h histItem) error {
		user = &h
		return nil
	}, histSelect(rawItem)+" AND h.kind IN ('user', 'steering') AND h.seq <= ? ORDER BY h.seq DESC LIMIT 1", sessionID, through)
	return user, err
}

func summaryMessage(summary string) map[string]any {
	return userMessage("[Context summary] Earlier conversation was compacted into this summary:\n\n" + summary)
}

// input builds the request input items.
func (v *contextView) input() []json.RawMessage {
	out := catalogInput(v.catalog)
	if v.summary != "" {
		out = append(out, json.RawMessage(mustJSON(summaryMessage(v.summary))))
	}
	if v.protectedUser != nil {
		if p, ok := v.project(*v.protectedUser); ok {
			out = append(out, p)
		}
	}
	for _, h := range v.items {
		if p, ok := v.project(h); ok {
			out = append(out, p)
		}
	}
	return out
}

func (v *contextView) project(h histItem) (json.RawMessage, bool) {
	if h.seq <= v.portableThrough && v.portableThrough > 0 {
		var item outputItem
		if json.Unmarshal(h.item, &item) != nil {
			return nil, false
		}
		if item.Type == "message" {
			contentType := "input_text"
			if item.Role == "assistant" {
				contentType = "output_text"
			}
			if item.Role == "user" && item.imageCount() > 0 { // pictures cross a handoff as saved, with their text
				var saved struct{ Content json.RawMessage }
				_ = json.Unmarshal(h.item, &saved)
				return json.RawMessage(mustJSON(map[string]any{"type": "message", "role": "user", "content": saved.Content})), true
			}
			return json.RawMessage(mustJSON(map[string]any{"type": "message", "role": item.Role, "content": []map[string]string{{"type": contentType, "text": item.text()}}})), true
		}
		if item.Type == "reasoning" || item.Type == "function_call" || item.Type == "function_call_output" {
			text := h.portable()
			if text == "" {
				return nil, false
			}
			return json.RawMessage(mustJSON(map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": "[Earlier model context; not a new tool request] " + text}}})), true
		}
		return nil, false // no opaque or unknown provider-specific items cross a handoff
	}
	return projectItem(h.item)
}

// estimateTokens is a conservative estimate: UTF-8 bytes / 3 plus 20%.
func estimateTokens(n int) int { return (n*2 + 4) / 5 }

func estimateRequest(instructions string, input []json.RawMessage, tools []Tool) int {
	n := len(instructions)
	for _, in := range input {
		n += modelInputBytes(in)
	}
	for _, t := range tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	return estimateTokens(n)
}

// callOutcomes snapshots the existing history owner's real saved results.
// It is request metadata only, not another transcript or Responses wire field.
func (v *contextView) callOutcomes() map[string]string {
	out := map[string]string{}
	for _, h := range v.items {
		if h.seq <= v.portableThrough {
			continue
		}
		if item := h.parsed(); item.Type == "function_call_output" {
			out[item.CallID] = h.callStatus
		}
	}
	return out
}

// Size the native block sent on wire, not its internal history envelope. Other
// protocols retain their existing byte estimate; this is not tokenization.
func modelInputBytes(raw json.RawMessage) int {
	var item outputItem
	if json.Unmarshal(raw, &item) == nil && item.API == APIAnthropicMessages {
		if block, err := nativeBlock(item); err == nil {
			return len(mustJSON(block))
		}
	}
	// An image counts as its provider cost, never as its base64 text.
	n := len(raw)
	for _, c := range item.Content {
		if c.Type == "input_image" {
			n += imageCostBytes - len(c.ImageURL)
		}
	}
	return n
}
