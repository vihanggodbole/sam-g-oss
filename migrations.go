package sam

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; the database user_version records how many ran.
var migrations = []string{
	`
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	workspace TEXT NOT NULL,
	config TEXT NOT NULL,
	bound_model TEXT,
	bound_provider TEXT,
	bound_base_url TEXT,
	bound_model_id TEXT,
	phase TEXT NOT NULL DEFAULT 'idle',
	paused INTEGER NOT NULL DEFAULT 0,
	summary_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE turns (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	status TEXT NOT NULL,
	source TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	instructions TEXT,
	settings TEXT,
	input_tokens INTEGER,
	output_tokens INTEGER,
	reasoning_tokens INTEGER,
	started_at INTEGER NOT NULL,
	ended_at INTEGER
);
CREATE UNIQUE INDEX turns_one_running ON turns(session_id) WHERE status = 'running';
CREATE INDEX turns_session ON turns(session_id, started_at);
CREATE TABLE history (
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	turn_id TEXT,
	kind TEXT NOT NULL,
	provenance TEXT NOT NULL DEFAULT '',
	item TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, seq)
);
CREATE TABLE tool_calls (
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	turn_id TEXT NOT NULL,
	call_id TEXT NOT NULL,
	idx INTEGER NOT NULL,
	name TEXT NOT NULL,
	arguments TEXT NOT NULL,
	status TEXT NOT NULL,
	output TEXT,
	history_seq INTEGER,
	started_at INTEGER,
	ended_at INTEGER,
	PRIMARY KEY (turn_id, call_id)
);
CREATE INDEX tool_calls_open ON tool_calls(session_id) WHERE history_seq IS NULL;
CREATE TABLE steering (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	text TEXT NOT NULL,
	status TEXT NOT NULL,
	turn_id TEXT,
	created_at INTEGER NOT NULL,
	UNIQUE (session_id, seq)
);
CREATE TABLE compactions (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	turn_id TEXT,
	state TEXT NOT NULL,
	prompt TEXT NOT NULL,
	model TEXT NOT NULL,
	covered_seq INTEGER,
	summary TEXT,
	error TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER,
	output_tokens INTEGER,
	reasoning_tokens INTEGER,
	started_at INTEGER NOT NULL,
	ended_at INTEGER
);
CREATE TABLE events (
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	turn_id TEXT NOT NULL DEFAULT '',
	operation_id TEXT NOT NULL DEFAULT '',
	call_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	payload TEXT NOT NULL,
	PRIMARY KEY (session_id, seq)
);
CREATE TABLE requests (
	id TEXT PRIMARY KEY,
	operation TEXT NOT NULL,
	hash TEXT NOT NULL,
	response TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`,
	// 2: look up a history item's tool-call outcome (presentation category).
	`CREATE INDEX tool_calls_history ON tool_calls(session_id, history_seq);`,
	// 3: freeze the legacy context visible at migration time, not a guessed
	// installation-time anchor. Old projection retained the globally newest
	// human entry only when covered. Inactive old summaries need no projection.
	`ALTER TABLE compactions ADD COLUMN protected_user_seq INTEGER;
UPDATE compactions SET protected_user_seq = (
	SELECT MAX(h.seq) FROM history h
	WHERE h.session_id = compactions.session_id AND h.kind IN ('user', 'steering')
) WHERE id IN (SELECT summary_id FROM sessions WHERE summary_id IS NOT NULL);
UPDATE compactions SET protected_user_seq = NULL WHERE protected_user_seq > covered_seq;`,
	// 4: NULL override intent identifies legacy full configurations. Open
	// expands them to explicit choices before admitting work; history is untouched.
	`ALTER TABLE sessions ADD COLUMN overrides TEXT;
ALTER TABLE sessions ADD COLUMN instructions TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN compaction_prompt TEXT NOT NULL DEFAULT '';`,
	// 5: extension attribution belongs to the canonical history entry.
	`ALTER TABLE history ADD COLUMN source TEXT;`,
	// 6: one durable executor per session. Preserve legacy phase-only pauses
	// before removing phase as a competing execution authority.
	`UPDATE sessions SET paused = 1 WHERE phase != 'idle';
ALTER TABLE sessions DROP COLUMN phase;
CREATE TABLE executions (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 action TEXT NOT NULL, turn_id TEXT, compaction_id TEXT, step INTEGER NOT NULL,
 turn_hook_done INTEGER NOT NULL DEFAULT 0,
 compaction_checked INTEGER NOT NULL DEFAULT 0,
 model_progress TEXT NOT NULL, stop_reason TEXT, stop_error TEXT
);
ALTER TABLE tool_calls ADD COLUMN step INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tool_calls ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tool_calls ADD COLUMN had_uncertain_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN step INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0;`,
	// 7: original wire identity and prepared-request dispatch authorization.
	// NULL targets are legacy flat calls, not newly authorized namespaced calls.
	`ALTER TABLE tool_calls ADD COLUMN namespace TEXT NOT NULL DEFAULT '';
ALTER TABLE tool_calls ADD COLUMN dispatch_target TEXT;`,
	// 8: explicit model changes keep old wire items immutable. Older context
	// is projected portably across the handoff rather than replaying opaque data.
	`ALTER TABLE sessions ADD COLUMN model_history_start INTEGER NOT NULL DEFAULT 0;`,
	// 9: children remain ordinary sessions. Deleting a parent preserves children.
	`ALTER TABLE sessions ADD COLUMN parent_id TEXT REFERENCES sessions(id) ON DELETE SET NULL;
ALTER TABLE sessions ADD COLUMN notified_turn_id TEXT NOT NULL DEFAULT '';
CREATE INDEX sessions_parent ON sessions(parent_id);`,
	// 10: delegation is a permanent session role, even after its parent is
	// deleted and the live ownership link is cleared. Receipts identify earlier
	// detached children without guessing from names or configuration.
	`ALTER TABLE sessions ADD COLUMN is_subagent INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET is_subagent=1 WHERE parent_id IS NOT NULL OR id IN (
 SELECT json_extract(response,'$.session_id') FROM requests WHERE operation='start_subagent');`,
	// 11: chat lists order by conversation activity (accepted input or a saved
	// history entry), not creation or metadata edits. tree_activity_at also
	// counts direct children so a parent rises with its delegated work.
	// Backfill from the authoritative history/turn/steering timestamps.
	`ALTER TABLE sessions ADD COLUMN activity_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN tree_activity_at INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET activity_at = MAX(created_at,
 COALESCE((SELECT MAX(created_at) FROM history WHERE session_id = sessions.id), 0),
 COALESCE((SELECT MAX(started_at) FROM turns WHERE session_id = sessions.id), 0),
 COALESCE((SELECT MAX(created_at) FROM steering WHERE session_id = sessions.id), 0));
UPDATE sessions SET tree_activity_at = MAX(activity_at,
 COALESCE((SELECT MAX(c.activity_at) FROM sessions c WHERE c.parent_id = sessions.id), 0));
CREATE INDEX sessions_workspace_activity ON sessions(workspace, tree_activity_at, id);
CREATE INDEX sessions_activity ON sessions(activity_at, id);`,
	`ALTER TABLE sessions ADD COLUMN resources TEXT NOT NULL DEFAULT '{}';`,
	// 13: protocol is part of the binding; legacy chats remain Responses.
	`ALTER TABLE sessions ADD COLUMN bound_api TEXT NOT NULL DEFAULT 'responses';`,
	// 14: provider-reported cache components; legacy NULL stays unknown.
	`ALTER TABLE turns ADD COLUMN cache_read_tokens INTEGER;
ALTER TABLE turns ADD COLUMN cache_write_tokens INTEGER;
ALTER TABLE compactions ADD COLUMN cache_read_tokens INTEGER;
ALTER TABLE compactions ADD COLUMN cache_write_tokens INTEGER;`,
	// 15: remember a call's no-replay restriction, not its executable definition.
	`ALTER TABLE tool_calls ADD COLUMN replay_unsafe INTEGER NOT NULL DEFAULT 0;`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	var authTables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='auth_meta'`).Scan(&authTables); err != nil {
		return err
	}
	if authTables != 0 {
		return errorf(CodeInvalidArgument, "conversation database_path must not name the AuthManager's private database")
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this runtime supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
