package sam

import "database/sql"

// canonicalInitiator uses genuine canonical sources, not wire roles or a
// protected old human message projected into the input.
func (r *Runtime) canonicalInitiator(sessionID string) (string, error) {
	var kind string
	err := r.db.QueryRowContext(bg, `SELECT kind FROM history WHERE session_id=?
 AND kind IN ('user','steering','output','tool_result') ORDER BY seq DESC LIMIT 1`, sessionID).Scan(&kind)
	if err != nil && err != sql.ErrNoRows {
		return "", storageErr(err)
	}
	if kind == kindUser || kind == kindSteering || err == sql.ErrNoRows {
		return "user", nil
	}
	return "agent", nil
}
