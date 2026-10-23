package sam

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

// ReloadSession rereads the selected session's configured instruction and
// compaction prompt files, workspace instructions and skill catalog. Only that
// session must be fully idle; other sessions may keep working. It preserves
// settings, model selection, history and pause state. It does not reload host
// configuration or replace the embedded prompt.
//
// Files are read outside the state lock, then idleness is checked again before
// publication. Work may start during preparation; if still busy at publication,
// the reload fails without changing the installed prompts. Retrying an accepted
// RequestID returns its receipt without rereading files. Use a new ID for each
// deliberate reload.
func (r *Runtime) ReloadSession(ctx context.Context, req SessionRequest) (SessionSettings, error) {
	return r.applySessionConfiguration(ctx, req, nil)
}

// Session updates and prompt reloads share one target-only preparation path.
// configMu keeps shared definitions stable, without excluding ordinary work.
func (r *Runtime) applySessionConfiguration(ctx context.Context, req SessionRequest, overrides *SessionOverrides) (SessionSettings, error) {
	var out SessionSettings
	op, reason, args := "reload_session", "reload", any(req)
	if overrides != nil {
		op, reason = "update_config", "update"
		args = UpdateConfigRequest{RequestID: req.RequestID, SessionID: req.SessionID, Config: *overrides}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if !r.configMu.TryLock() {
		return out, errorf(CodeConflict, "configuration preparation is in progress; retry after it finishes")
	}
	defer r.configMu.Unlock()

	r.mu.Lock()
	var s *sessionRow
	var saved json.RawMessage
	err := r.admit()
	if err == nil {
		err = r.admitTx(func(tx *sql.Tx) error {
			var err error
			saved, _, err = dedupe(tx, req.RequestID, op, args)
			if err != nil || saved != nil {
				return err
			}
			s, err = loadSession(tx, req.SessionID)
			if err != nil {
				return err
			}
			return r.sessionConfigIdle(s)
		})
	}
	p := *r.preparedConfig
	// A per-chat refresh must not change the shared path cache or another
	// session's installed text, even when they use the exact same file.
	p.files = map[string]string{}
	r.mu.Unlock()
	if err != nil {
		return out, err
	}
	if saved != nil {
		err = json.Unmarshal(saved, &out)
		return out, err
	}
	if overrides != nil {
		if err := p.prepareSessionOverrides(s, *overrides); err != nil {
			return out, err
		}
	}
	if err := p.preparePrompts(s); err != nil {
		return out, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	err = r.mutate(req.RequestID, op, args, &out, func(tx *sql.Tx) error {
		current, err := loadSession(tx, req.SessionID)
		if err != nil {
			return err
		}
		if err := r.sessionConfigIdle(current); err != nil {
			return err
		}
		if overrides != nil {
			// A first turn may have bound this chat while file I/O was in progress.
			// Never overwrite that newly pinned selection with an ordinary settings edit.
			if current.binding != nil && (s.cfg.Model != current.binding.Model || s.cfg.Account != current.cfg.Account) {
				return errBoundModel
			}
			current.cfg, current.overrides = s.cfg, s.overrides
			current.pinSelection()
		}
		current.instructions, current.compactionPrompt = s.instructions, s.compactionPrompt
		current.resources = s.resources
		current.updatedAt = nowMillis()
		if err := savePrepared(tx, current); err != nil {
			return err
		}
		out = SessionSettings{Effective: current.cfg.withDefaults(), Overrides: current.overrides, Resources: current.resources}
		return appendEvent(tx, event(current.id, EventConfigChanged, struct {
			SessionSettings
			Reason string `json:"reason"`
		}{out, reason}))
	})
	return out, err
}

func (r *Runtime) sessionConfigIdle(s *sessionRow) error {
	if s.phase != PhaseIdle || r.workers[s.id] != nil {
		return errorf(CodeConflict, "Session has unfinished work. Resume it, or Interrupt and wait before changing its settings or reloading resources.")
	}
	return nil
}

// Only session choices change here: do not rebuild the runtime catalog, tools,
// HTTP client, or another chat's prompts for an ordinary settings save.
func (p *preparedConfig) prepareSessionOverrides(s *sessionRow, overrides SessionOverrides) error {
	if s.isSubagent && overrides.DisabledTools != nil && !slices.Contains(*overrides.DisabledTools, "subagents") {
		return errorf(CodeUnsupported, "child sessions cannot enable the subagents tool")
	}
	previous := s.cfg
	s.overrides = overrides
	s.pinSelection()
	s.cfg = s.overrides.apply(p.cfg.Defaults)
	if s.binding != nil && (s.cfg.Model != s.binding.Model || s.cfg.Account != previous.Account) {
		return errBoundModel
	}
	if err := p.validateModelEdits(previous, s.cfg); err != nil {
		return err
	}
	return p.validateExistingSession(s, p)
}
