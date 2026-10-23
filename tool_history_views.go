package sam

import "database/sql"

// Preserve existing history cursors; event cursors use a distinct scope. The
// scope is validated after child ownership, never used as authorization.
func historyScope(sid, view string) string {
	if view == "events" {
		return sid + "/events"
	}
	return sid
}

// Both views use the same bounded scanning, search pagination and output
// budget. Event reads return ordinary Event envelopes, not hidden wire items.
func eachToolHistory(q querier, view string, search bool, fn func(HistoryItem, any) error, suffix string, args ...any) error {
	if view != "events" {
		query := histSelect(visibleItem)
		if search {
			query += notHistoryTool
		}
		return eachHistory(q, func(h histItem) error { v := h.visible(); return fn(v, v) }, query+suffix, args...)
	}
	query := `SELECT h.seq,h.turn_id,h.operation_id,h.call_id,h.kind,h.created_at,h.payload,h.step,h.attempt
  FROM events h WHERE h.session_id=?`
	if search {
		query += ` AND NOT EXISTS (SELECT 1 FROM tool_calls c WHERE c.turn_id=h.turn_id AND c.call_id=h.call_id AND c.name='history')`
	}
	sid := args[0].(string)
	return queryRows(q, func(rows *sql.Rows) error {
		e, err := scanEvent(rows, sid)
		if err != nil {
			return err
		}
		v := HistoryItem{
			Seq: e.Seq, TurnID: e.TurnID, CallID: e.CallID, Time: e.Time,
			Kind: e.Kind, Type: "event", Category: e.Category, Text: mustJSON(e),
		}
		return fn(v, e)
	}, query+suffix, args...)
}
