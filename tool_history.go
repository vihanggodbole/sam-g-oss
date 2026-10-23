package sam

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
	"unicode/utf8"
)

// The history tool lets the model search and read the original, retained
// history of its own session or an owned child, including entries that a
// compaction summary replaced in model requests. Ownership comes from the
// runtime-supplied session. Both history and event views reuse the public visible
// projections, so encrypted reasoning is never exposed.

const (
	historyToolName  = "history"
	historyPageSize  = 10 // matching entries per search page (agreed contract; not configurable)
	historyBatchRows = 64 // rows per SQLite query; rows are released between batches
	excerptContext   = 80 // bytes of context on each side of a match
)

// notHistoryTool excludes the history tool's own calls and results from
// search, so a search does not rediscover earlier search output. Reads
// return every visible entry, including earlier lookups.
const notHistoryTool = ` AND COALESCE(c.name, '') <> '` + historyToolName + `'
	AND NOT (json_extract(h.item, '$.type') = 'function_call' AND json_extract(h.item, '$.name') = '` + historyToolName + `')`

// errStopScan ends a row scan early without an error.
var errStopScan = errors.New("stop scan")

func historyTool(r *Runtime) Tool {
	return Tool{
		Name:        historyToolName,
		Description: "Search or read visible conversation history or diagnostic events of this session or an owned subagent.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["search","read"],"description":"Search returns at most 10 newest matches with excerpts; read returns full entries and fails if the output limit is exceeded. Narrow the range when needed."},` +
			`"subagent_id":{"type":"string","description":"Optional owned child session ID; omitted reads the caller's own session."},` +
			`"view":{"type":"string","enum":["history","events"],"description":"Default history: saved visible messages, reasoning summaries and tools (never opaque reasoning). Events: running output, retries, failures and lifecycle diagnostics. Each view has its own seq numbers."},` +
			`"query":{"type":"string","description":"search: Go regular expression (RE2; (?i) ignores case, no lookaround/backreferences). History searches visible text and tool arguments; events searches event JSON. Search excludes history tool calls/results; read includes them."},` +
			`"cursor":{"type":"string","description":"search: next_cursor from the same session, view and exact query; continues to older matches"},` +
			`"from_seq":{"type":"integer","description":"read: first entry seq"},` +
			`"to_seq":{"type":"integer","description":"read: last entry seq (inclusive)"},` +
			`"around_seq":{"type":"integer","description":"read: entry seq to read with its neighbours"},` +
			`"before":{"type":"integer","description":"read with around_seq: entries before it (default 0)"},` +
			`"after":{"type":"integer","description":"read with around_seq: entries after it (default 0)"}},` +
			`"required":["action"],"additionalProperties":false}`),
		Run: func(ctx context.Context, c ToolCall) (string, error) { return "", r.runHistoryTool(ctx, c) },
	}
}

type historyArgs struct {
	SubagentID string          `json:"subagent_id"`
	View       string          `json:"view"`
	Action     string          `json:"action"`
	Query      string          `json:"query"`
	Cursor     string          `json:"cursor"`
	FromSeq    *int64          `json:"from_seq"`
	ToSeq      *int64          `json:"to_seq"`
	AroundSeq  *int64          `json:"around_seq"`
	Before     *int64          `json:"before"`
	After      *int64          `json:"after"`
	Limit      json.RawMessage `json:"limit"` // rejected with an explanation
}

func (r *Runtime) runHistoryTool(ctx context.Context, c ToolCall) error {
	var a historyArgs
	if err := decodeArgs(c.Arguments, &a); err != nil {
		return err
	}
	if err := a.check(); err != nil {
		return err
	}
	if a.SubagentID != "" {
		if err := ownedSubagent(r.reader(ctx), c.SessionID, a.SubagentID); err != nil {
			return err
		}
		c.SessionID = a.SubagentID
	}
	if a.Action == "search" {
		return r.searchHistory(ctx, c, a)
	}
	return r.readHistory(ctx, c, a)
}

func (a historyArgs) check() error {
	if a.View != "" && a.View != "history" && a.View != "events" {
		return errors.New("view must be history or events")
	}
	if a.Limit != nil {
		return errors.New("limit is not supported: search always returns at most 10 matching entries; pass next_cursor as cursor for more")
	}
	read := a.FromSeq != nil || a.ToSeq != nil || a.AroundSeq != nil || a.Before != nil || a.After != nil
	switch a.Action {
	case "search":
		if a.Query == "" {
			return errors.New("search requires a non-empty query (a Go regular expression)")
		}
		if read {
			return errors.New(`from_seq, to_seq, around_seq, before, and after are only used with action "read"`)
		}
		return nil
	case "read":
		if a.Query != "" || a.Cursor != "" {
			return errors.New(`query and cursor are only used with action "search"`)
		}
		return a.checkRead()
	}
	return errors.New(`action must be "search" or "read"`)
}

func (a historyArgs) checkRead() error {
	span := a.FromSeq != nil || a.ToSeq != nil
	switch {
	case span && a.AroundSeq != nil:
		return errors.New("use either from_seq/to_seq or around_seq with before/after, not both")
	case span:
		if a.FromSeq == nil || a.ToSeq == nil {
			return errors.New("read with a range needs both from_seq and to_seq")
		}
		if a.Before != nil || a.After != nil {
			return errors.New("before and after are only used with around_seq")
		}
		if *a.FromSeq < 1 || *a.ToSeq < *a.FromSeq {
			return fmt.Errorf("invalid range %d..%d: seq numbers start at 1 and to_seq must not be below from_seq", *a.FromSeq, *a.ToSeq)
		}
	case a.AroundSeq != nil:
		if *a.AroundSeq < 1 {
			return errors.New("around_seq must be at least 1")
		}
		if (a.Before != nil && *a.Before < 0) || (a.After != nil && *a.After < 0) {
			return errors.New("before and after must not be negative")
		}
	default:
		return errors.New("read needs from_seq and to_seq, or around_seq (optionally with before and after)")
	}
	return nil
}

// historyMatch is one search result: a matching entry, once per page, with
// an excerpt around its first match.
type historyMatch struct {
	Source   *ExtensionSource `json:"source,omitempty"`
	Seq      int64            `json:"seq"`
	TurnID   string           `json:"turn_id,omitempty"`
	Kind     string           `json:"kind"`
	Type     string           `json:"type"`
	Role     string           `json:"role,omitempty"`
	Name     string           `json:"name,omitempty"`
	CallID   string           `json:"call_id,omitempty"`
	Category string           `json:"category"`
	Status   string           `json:"status,omitempty"`
	Code     string           `json:"code,omitempty"`
	Time     time.Time        `json:"time"`
	Excerpt  string           `json:"excerpt"`
}

// searchHistory scans history newest first in bounded batches, keeping only
// the page's matches. Rows are released between batches, so a long search
// does not hold the shared connection. Scanning downward from the cursor
// keeps pages within the first request's snapshot: newer entries have
// higher seq numbers and never appear on later pages.
func (r *Runtime) searchHistory(ctx context.Context, c ToolCall, a historyArgs) error {
	re, err := regexp.Compile(a.Query)
	if err != nil {
		return fmt.Errorf("invalid regular expression: %v. Patterns use Go regexp (RE2) syntax, "+
			"which has no lookahead, lookbehind, or backreferences", err)
	}
	below := int64(math.MaxInt64)
	if a.Cursor != "" {
		if below, err = decodeCursor(a.Cursor, historyScope(c.SessionID, a.View), a.Query); err != nil {
			return err
		}
	}
	db := r.reader(ctx)
	page := make([]historyMatch, 0, historyPageSize)
	next := ""
	for next == "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := 0
		err := eachToolHistory(db, a.View, true, func(v HistoryItem, _ any) error {
			n, below = n+1, v.Seq
			text := searchText(v)
			loc := re.FindStringIndex(text)
			if loc == nil {
				return nil
			}
			if len(page) == historyPageSize { // lookahead: another match exists
				next = encodeCursor(historyScope(c.SessionID, a.View), a.Query, page[len(page)-1].Seq)
				return errStopScan
			}
			page = append(page, historyMatch{
				Source: v.Source, Seq: v.Seq, TurnID: v.TurnID, Kind: v.Kind, Type: v.Type, Role: v.Role, Name: v.Name, CallID: v.CallID,
				Category: v.Category, Status: v.Status, Code: v.Code, Time: v.Time, Excerpt: excerpt(text, loc),
			})
			return nil
		}, " AND h.seq < ? ORDER BY h.seq DESC LIMIT ?", c.SessionID, below, historyBatchRows)
		if err != nil && !errors.Is(err, errStopScan) {
			return err
		}
		if n < historyBatchRows {
			break
		}
	}
	out := struct {
		Matches    []historyMatch `json:"matches"`
		NextCursor string         `json:"next_cursor,omitempty"`
	}{page, next}
	_, err = c.Output.Write([]byte(mustJSON(out)))
	return err
}

// searchText is the visible text a search matches: the tool name and
// arguments of a call, otherwise the entry's text.
func searchText(v HistoryItem) string {
	if v.Type == "function_call" {
		return v.Name + " " + v.Arguments
	}
	return v.Text
}

// excerpt returns a short preview around the match at loc.
func excerpt(text string, loc []int) string {
	start, end := loc[0], min(loc[1], loc[0]+2*excerptContext)
	from, to := max(0, start-excerptContext), min(len(text), end+excerptContext)
	for from > 0 && !utf8.RuneStart(text[from]) {
		from--
	}
	for to < len(text) && !utf8.RuneStart(text[to]) {
		to++
	}
	s := text[from:to]
	if from > 0 {
		s = "…" + s
	}
	if to < len(text) {
		s += "…"
	}
	return s
}

// historyCursor continues a search below seq Below. It is plain, unsigned
// state for a trusted local tool; it only records which session and query
// it belongs to so a mismatch fails clearly.
type historyCursor struct {
	Session string `json:"s"`
	Query   string `json:"q"` // short query hash
	Below   int64  `json:"b"`
}

func queryHash(q string) string {
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:8])
}

func encodeCursor(session, query string, below int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(mustJSON(historyCursor{session, queryHash(query), below})))
}

func decodeCursor(s, session, query string) (int64, error) {
	var cur historyCursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(raw, &cur) != nil || cur.Session == "" || cur.Below < 1 {
		return 0, errors.New("malformed cursor: pass next_cursor exactly as returned, or search again without a cursor")
	}
	if cur.Session != session {
		return 0, errors.New("cursor belongs to another session or view; search again without a cursor")
	}
	if cur.Query != queryHash(query) {
		return 0, errors.New("cursor was returned for a different query; repeat that exact query with it, or search again without a cursor")
	}
	return cur.Below, nil
}

// readHistory writes full visible entries in seq order. Rows are read in
// bounded batches and written after each batch's rows are closed, because
// streaming output records events in the same database. A batch stops
// buffering once it alone exceeds the output limit, which then fails the call.
func (r *Runtime) readHistory(ctx context.Context, c ToolCall, a historyArgs) error {
	db := r.reader(ctx)
	from, to, err := readBounds(db, c.SessionID, a)
	if err != nil {
		return err
	}
	written := false
	for after := from - 1; after < to; {
		if err := ctx.Err(); err != nil {
			return err
		}
		var buf []byte
		n := 0
		err := eachToolHistory(db, a.View, false, func(v HistoryItem, raw any) error {
			n, after = n+1, v.Seq
			if len(buf) > 0 || written {
				buf = append(buf, ',')
			}
			buf = append(buf, mustJSON(raw)...)
			if len(buf) > c.Output.limit {
				return errStopScan
			}
			return nil
		}, " AND h.seq > ? AND h.seq <= ? ORDER BY h.seq LIMIT ?",
			c.SessionID, after, to, historyBatchRows)
		if err != nil && !errors.Is(err, errStopScan) {
			return err
		}
		if len(buf) > 0 {
			if !written {
				buf = append([]byte(`{"entries":[`), buf...)
				written = true
			}
			if _, err := c.Output.Write(buf); err != nil {
				return err
			}
		}
		if n < historyBatchRows {
			break
		}
	}
	if !written {
		return fmt.Errorf("no history entries between seq %d and %d in this session", from, to)
	}
	_, err = c.Output.Write([]byte("]}"))
	return err
}

// readBounds resolves the inclusive seq range to read. around_seq must name
// an existing entry; before/after count the entries next to it.
func readBounds(db querier, sessionID string, a historyArgs) (int64, int64, error) {
	if a.AroundSeq == nil {
		to := *a.ToSeq
		if a.View == "events" {
			// Reading emits tool.output events too; never chase our own output.
			var last int64
			if err := db.QueryRowContext(bg, `SELECT COALESCE(MAX(seq),0) FROM events WHERE session_id=?`, sessionID).Scan(&last); err != nil {
				return 0, 0, err
			}
			to = min(to, last)
		}
		return *a.FromSeq, to, nil
	}
	around := *a.AroundSeq
	fromSQL := histFrom
	if a.View == "events" {
		fromSQL = " FROM events h WHERE h.session_id = ?"
	}
	var one int
	err := db.QueryRowContext(bg, "SELECT 1"+fromSQL+" AND h.seq = ?", sessionID, around).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("history entry %d does not exist in this session", around)
	}
	if err != nil {
		return 0, 0, err
	}
	from, to := around, around
	edge := func(agg, cmp, order string, n *int64, out *int64) error {
		if n == nil || *n == 0 {
			return nil
		}
		var v sql.NullInt64
		err := db.QueryRowContext(bg, "SELECT "+agg+"(seq) FROM (SELECT h.seq"+fromSQL+
			" AND h.seq "+cmp+" ? ORDER BY h.seq "+order+" LIMIT ?)", sessionID, around, *n).Scan(&v)
		if v.Valid {
			*out = v.Int64
		}
		return err
	}
	if err := edge("MIN", "<", "DESC", a.Before, &from); err != nil {
		return 0, 0, err
	}
	return from, to, edge("MAX", ">", "ASC", a.After, &to)
}
