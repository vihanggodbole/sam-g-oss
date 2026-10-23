package sam

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"strings"
	"time"
)

// SessionSummary is one compact row for a chat list: identity, place in the
// parent/child tree and the state shown beside a name. It is a read view of
// the same session GetSession returns in full, not a separate record.
// ParentName and ParentWorkspace are empty when the parent no longer exists.
// ChildCount counts direct children in the same workspace; Children holds the
// most recently active of them on a workspace page only.
type SessionSummary struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Workspace       string           `json:"workspace"`
	ParentID        string           `json:"parent_id,omitempty"`
	ParentName      string           `json:"parent_name,omitempty"`
	ParentWorkspace string           `json:"parent_workspace,omitempty"`
	IsSubagent      bool             `json:"is_subagent,omitempty"`
	Phase           string           `json:"phase"`
	Paused          bool             `json:"paused"`
	PendingSteering int              `json:"pending_steering"`
	ActiveToolCount int              `json:"active_tool_count"`
	ActivityAt      time.Time        `json:"activity_at"`
	CreatedAt       time.Time        `json:"created_at"`
	ChildCount      int              `json:"child_count"`
	Children        []SessionSummary `json:"children,omitempty"`
}

// RecentQuery selects one page of session summaries, most recent
// conversation activity first. Activity is creation, accepted input
// (prompt, continue, steering or a child notice) and saved history entries;
// renames, settings, pauses and reads are not activity. Modes, by priority:
//
//   - Search: case-insensitive substring of session names or workspace paths
//     across all workspaces (not message contents). ASCII case folding only.
//   - Active: sessions with unfinished work (running, compacting, suspended,
//     stopping).
//   - Parent: direct children of Parent, limited to Workspace when set.
//   - Workspace: the workspace's top-level rows (sessions whose parent is
//     absent or in another workspace), ordered by their own or any direct
//     child's activity, each with up to RecentChildren nested children.
//   - none: every session.
//
// Pages use a keyset cursor (activity time, ID). Activity only moves forward,
// so a session that becomes active after a page was read moves above the
// cursor: later pages never repeat it, and rereading the first page finds it.
type RecentQuery struct {
	Workspace string
	Parent    string
	Search    string
	Active    bool
	After     string
	Limit     int // default and maximum 200
}

// RecentChildren is how many nested children a workspace page includes per row.
const RecentChildren = 5

// RecentPage is a page of summaries; pass Next as RecentQuery.After to
// continue. An empty Next means there are no more rows.
type RecentPage struct {
	Sessions []SessionSummary `json:"sessions"`
	Next     string           `json:"next,omitempty"`
}

// WorkspaceSummary describes one workspace without listing its sessions.
// ActivityAt is the newest activity of any session there (including children
// counted by their parent); UpdatedAt is the newest metadata change (name,
// pause, settings). Both help clients decide which loaded lists to reread.
type WorkspaceSummary struct {
	Workspace  string    `json:"workspace"`
	Sessions   int       `json:"sessions"`
	Active     int       `json:"active"`
	ActivityAt time.Time `json:"activity_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// WorkspacePage is a page of workspaces, most recent activity first.
type WorkspacePage struct {
	Workspaces []WorkspaceSummary `json:"workspaces"`
	Next       string             `json:"next,omitempty"`
}

const summarySelect = `SELECT s.id, s.name, s.workspace, COALESCE(s.parent_id, ''), COALESCE(p.name, ''), COALESCE(p.workspace, ''),
	s.is_subagent, x.action, COALESCE(x.compaction_id, ''), s.paused,
	(SELECT COUNT(*) FROM steering WHERE session_id = s.id AND status = 'pending'),
	(SELECT COUNT(*) FROM tool_calls WHERE session_id = s.id AND status = 'started' AND history_seq IS NULL),
	(SELECT COUNT(*) FROM sessions c WHERE c.parent_id = s.id AND c.workspace = s.workspace),
	s.activity_at, s.tree_activity_at, s.created_at
FROM sessions s LEFT JOIN executions x ON x.session_id = s.id LEFT JOIN sessions p ON p.id = s.parent_id `

// RecentSessions returns one bounded page of session summaries. Each row
// costs a few indexed lookups; hidden sessions are never expanded.
func (r *Runtime) RecentSessions(ctx context.Context, q RecentQuery) (RecentPage, error) {
	if err := r.readable(); err != nil {
		return RecentPage{}, err
	}
	limit := pageLimit(q.Limit)
	r.mu.Lock()
	defer r.mu.Unlock()
	query, tree, args, err := recentSQL(q, limit, r.workerIDs())
	if err != nil {
		return RecentPage{}, err
	}
	db := r.reader(ctx)
	rows, err := r.summaries(db, tree, query, args...)
	if err != nil {
		return RecentPage{}, readErr(ctx, err)
	}
	if tree {
		if err := r.attachChildren(db, q.Workspace, rows); err != nil {
			return RecentPage{}, readErr(ctx, err)
		}
	}
	page := RecentPage{Sessions: make([]SessionSummary, len(rows))}
	for i, row := range rows {
		page.Sessions[i] = row.SessionSummary
	}
	if len(rows) == limit {
		page.Next = rows[limit-1].cursor
	}
	return page, nil
}

// recentSQL builds one keyset page query for q. tree reports a workspace page,
// which orders top-level rows by tree_activity_at and nests children.
func recentSQL(q RecentQuery, limit int, workers string) (query string, tree bool, args []any, err error) {
	at, id, err := keyset(q.After)
	if err != nil {
		return "", false, nil, err
	}
	var where string
	switch {
	case q.Search != "":
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q.Search) + "%"
		where, args = `(s.name LIKE ? ESCAPE '\' OR s.workspace LIKE ? ESCAPE '\')`, []any{like, like}
	case q.Active:
		where, args = "s.id IN (SELECT session_id FROM executions UNION SELECT value FROM json_each(?))", []any{workers}
	case q.Parent != "":
		where, args = "s.parent_id = ? AND (? = '' OR s.workspace = ?)", []any{q.Parent, q.Workspace, q.Workspace}
	case q.Workspace != "":
		where, args, tree = "s.workspace = ? AND (p.id IS NULL OR p.workspace != s.workspace)", []any{q.Workspace}, true
	default:
		where = "1"
	}
	key := "s.activity_at"
	if tree {
		key = "s.tree_activity_at"
	}
	query = summarySelect + "WHERE " + where + " AND (" + key + ", s.id) < (?, ?) ORDER BY " + key + " DESC, s.id DESC LIMIT ?"
	return query, tree, append(args, at, id, limit), nil
}

// summaryRow keeps the page cursor beside a summary.
type summaryRow struct {
	SessionSummary
	cursor string
}

// summaries scans summary rows; tree selects which activity the cursor encodes.
func (r *Runtime) summaries(q querier, tree bool, query string, args ...any) ([]summaryRow, error) {
	out := []summaryRow{}
	err := queryRows(q, func(rows *sql.Rows) error {
		var v summaryRow
		var action sql.NullString
		var compaction string
		var activity, treeActivity, created int64
		if err := rows.Scan(&v.ID, &v.Name, &v.Workspace, &v.ParentID, &v.ParentName, &v.ParentWorkspace, &v.IsSubagent,
			&action, &compaction, &v.Paused, &v.PendingSteering, &v.ActiveToolCount, &v.ChildCount, &activity, &treeActivity, &created); err != nil {
			return err
		}
		var x *execution
		if action.Valid {
			x = &execution{Action: executionAction(action.String), CompactionID: compaction}
		}
		v.Phase = executionPhase(x, r.workers[v.ID] != nil)
		v.ActivityAt, v.CreatedAt = fromMillis(activity), fromMillis(created)
		key := activity
		if tree {
			key = treeActivity
		}
		v.cursor = strconv.FormatInt(key, 10) + ":" + v.ID
		out = append(out, v)
		return nil
	}, query, args...)
	return out, err
}

// attachChildren adds each row's most recently active nested children in one
// query, however many rows the page has.
func (r *Runtime) attachChildren(q querier, workspace string, rows []summaryRow) error {
	var parents []string
	at := map[string]int{}
	for i, row := range rows {
		if row.ChildCount > 0 {
			parents = append(parents, row.ID)
			at[row.ID] = i
		}
	}
	if len(parents) == 0 {
		return nil
	}
	kids, err := r.summaries(q, false, `WITH k AS (SELECT id, ROW_NUMBER() OVER (PARTITION BY parent_id ORDER BY activity_at DESC, id DESC) AS n
		FROM sessions WHERE parent_id IN (SELECT value FROM json_each(?)) AND workspace = ?)
		`+summarySelect+`JOIN k ON k.id = s.id WHERE k.n <= ? ORDER BY k.n`, mustJSON(parents), workspace, RecentChildren)
	for _, k := range kids {
		i := at[k.ParentID]
		rows[i].Children = append(rows[i].Children, k.SessionSummary)
	}
	return err
}

func (r *Runtime) workerIDs() string {
	ids := make([]string, 0, len(r.workers))
	for id := range r.workers {
		ids = append(ids, id)
	}
	return mustJSON(ids)
}

// workspacesSQL groups the covering workspace/activity index, one entry per session.
const workspacesSQL = `SELECT s.workspace, COUNT(*), MAX(s.tree_activity_at), MAX(s.updated_at),
	SUM(x.session_id IS NOT NULL OR s.id IN (SELECT value FROM json_each(?)))
	FROM sessions s LEFT JOIN executions x ON x.session_id = s.id GROUP BY s.workspace
	HAVING (MAX(s.tree_activity_at), s.workspace) < (?, ?)
	ORDER BY MAX(s.tree_activity_at) DESC, s.workspace DESC LIMIT ?`

// Workspaces pages through workspaces by their newest activity, with counts.
// Cost grows with the number of sessions (one index entry plus updated_at
// each, no history); the response holds only workspaces.
func (r *Runtime) Workspaces(ctx context.Context, q ListQuery) (WorkspacePage, error) {
	if err := r.readable(); err != nil {
		return WorkspacePage{}, err
	}
	at, path, err := keyset(q.After)
	if err != nil {
		return WorkspacePage{}, err
	}
	limit := pageLimit(q.Limit)
	r.mu.Lock()
	defer r.mu.Unlock()
	page := WorkspacePage{Workspaces: []WorkspaceSummary{}}
	var activity int64
	err = queryRows(r.reader(ctx), func(rows *sql.Rows) error {
		var w WorkspaceSummary
		var updated int64
		if err := rows.Scan(&w.Workspace, &w.Sessions, &activity, &updated, &w.Active); err != nil {
			return err
		}
		w.ActivityAt, w.UpdatedAt = fromMillis(activity), fromMillis(updated)
		page.Workspaces = append(page.Workspaces, w)
		return nil
	}, workspacesSQL, r.workerIDs(), at, path, limit)
	if err != nil {
		return WorkspacePage{}, readErr(ctx, err)
	}
	if n := len(page.Workspaces); n == limit {
		page.Next = strconv.FormatInt(activity, 10) + ":" + page.Workspaces[n-1].Workspace
	}
	return page, nil
}

// keyset decodes an "activity:key" cursor; empty starts before everything.
func keyset(after string) (int64, string, error) {
	if after == "" {
		return math.MaxInt64, "", nil
	}
	ms, key, ok := strings.Cut(after, ":")
	n, err := strconv.ParseInt(ms, 10, 64)
	if !ok || err != nil {
		return 0, "", errorf(CodeInvalidArgument, "invalid cursor %q", after)
	}
	return n, key, nil
}
