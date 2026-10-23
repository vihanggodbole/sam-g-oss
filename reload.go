package sam

import (
	"cmp"
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
)

// Reload atomically replaces runtime configuration and refreshes all installed
// prompt files. Every session must be fully idle, including follow-up handoffs
// and stopping workers. Busy reloads are rejected, never queued or interrupting.
// DatabasePath cannot change. HTTPClient, Auth and tool resources stay host-owned:
// replacing a reference does not close it or stop acknowledged background work.
// Reload is host-local: Go functions cannot be uploaded through JSON/HTTP.
func (r *Runtime) Reload(ctx context.Context, cfg Config) error {
	return r.applyConfiguration(ctx, cfg, nil)
}

// ReloadAndSave reserves the same all-idle boundary as Reload. save runs after
// validation, before publication, without the runtime mutex held. No new work
// can start until both finish. Once save is called, any failure closes the
// runtime: the host file may contain the new configuration. Cancellation is
// admission-only once saving starts: publication finishes despite a disconnected
// caller. Explicit runtime shutdown still wins. Hosts own atomic file replacement
// and must not use save to call runtime mutations or bind it to caller cancellation.
func (r *Runtime) ReloadAndSave(ctx context.Context, cfg Config, save func() error) error {
	if save == nil {
		return errorf(CodeInvalidArgument, "save callback is required")
	}
	called := false
	err := r.applyConfiguration(ctx, cfg, func() error { called = true; return save() })
	if err != nil && called {
		_ = r.Close(context.Background())
		return errorf(CodeStorageError, "configuration save/publication failed; the runtime has been closed and the file may contain the new configuration. Inspect storage and restart: %v", err)
	}
	return err
}

func (r *Runtime) fullyIdle() error { return r.fullyIdleIn(r.db) }

func (r *Runtime) fullyIdleIn(q querier) error {
	if len(r.workers) != 0 {
		return errorf(CodeConflict, "configuration reload requires every session to be fully idle; wait for all work to settle")
	}
	var n int
	if err := q.QueryRowContext(bg, "SELECT COUNT(*) FROM executions").Scan(&n); err != nil {
		return storageErr(err)
	}
	if n != 0 {
		return errBusy
	}
	return nil
}

// reserveConfiguration excludes new mutations while a global replacement is
// prepared, but holds no mutex or SQL transaction through prompt I/O.
// Close can still begin and abort publication.
func (r *Runtime) reserveConfiguration() ([]*sessionRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(); err != nil {
		return nil, err
	}
	if err := r.fullyIdle(); err != nil {
		return nil, err
	}
	sessions, err := allSessions(r.db)
	if err != nil {
		return nil, storageErr(err)
	}
	r.reloading = true
	return sessions, nil
}

func (r *Runtime) applyConfiguration(ctx context.Context, cfg Config, save func() error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.configMu.TryLock() {
		return errorf(CodeConflict, "configuration preparation is already in progress; retry after it finishes")
	}
	defer r.configMu.Unlock()
	sessions, err := r.reserveConfiguration()
	if err != nil {
		return err
	}
	locked, saving := false, false
	defer func() {
		if !locked {
			r.mu.Lock()
		}
		// Seal admission before releasing either the reservation or the mutex.
		// Blocking Close is performed by ReloadAndSave only after these unlock.
		if saving && err != nil {
			r.storageFailed(err)
		}
		r.reloading = false
		r.mu.Unlock()
	}()
	candidate, diags, err := r.prepareReplacement(cfg, sessions)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if save != nil {
		saving = true
		ctx = context.WithoutCancel(ctx)
		if err := save(); err != nil {
			return err
		}
	}
	r.mu.Lock()
	locked = true
	r.reloading = false // the mutex still excludes admission during publication
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.admit(); err != nil {
		return err
	}
	if err := r.fullyIdle(); err != nil {
		return err
	}
	persist := func(tx *sql.Tx) error {
		for _, s := range sessions {
			s.updatedAt = nowMillis()
			if err := savePrepared(tx, s); err != nil {
				return err
			}
			settings := SessionSettings{Effective: s.cfg.withDefaults(), Overrides: s.overrides, Resources: s.resources}
			if err := appendEvent(tx, event(s.id, EventConfigChanged, struct {
				SessionSettings
				Reason string `json:"reason"`
			}{settings, "reload"})); err != nil {
				return err
			}
		}
		return nil
	}
	err = r.admitTx(persist)
	if err != nil {
		return err
	}
	r.publishConfiguration(candidate, diags)
	return nil
}

// Called with the transition mutex held, after the durable settings commit.
func (r *Runtime) publishConfiguration(candidate *preparedConfig, diags []Diagnostic) {
	r.preparedConfig = candidate
	cfg := candidate.cfg
	client := *r.client
	client.http = cfg.HTTPClient
	if client.http == nil {
		client.http = &http.Client{}
	}
	client.auth = cfg.Auth
	client.maxEvent = cmp.Or(cfg.MaxProviderEventBytes, DefaultMaxProviderEventBytes)
	r.client = &client
	r.diagMu.Lock()
	// Preserve asynchronous process diagnostics, replacing registration diagnostics.
	kept := r.diags[:0]
	for _, d := range r.diags {
		if d.Code != "invalid_tool" {
			kept = append(kept, d)
		}
	}
	r.diags = append(kept, diags...)
	r.diagMu.Unlock()
}

func (r *Runtime) prepareReplacement(cfg Config, sessions []*sessionRow) (*preparedConfig, []Diagnostic, error) {
	cfg = cfg.clone()
	oldPath, err := filepath.Abs(r.cfg.DatabasePath)
	if err != nil {
		return nil, nil, err
	}
	newPath, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return nil, nil, err
	}
	if newPath != oldPath {
		return nil, nil, errorf(CodeInvalidArgument, "reload cannot relocate the open database; close and open a runtime for another database")
	}
	// Preserve the absolute identity at every publication, not just Open.
	cfg.DatabasePath = oldPath
	if err := cfg.validate(); err != nil {
		return nil, nil, err
	}
	p := &preparedConfig{cfg: cfg, files: map[string]string{}}
	diags := p.registerTools(cfg.Tools, r)
	if err := p.prepareAbsolutePrompts(); err != nil {
		return nil, nil, err
	}
	if err := p.validateDefaults(); err != nil {
		return nil, nil, err
	}
	for _, s := range sessions {
		previous := s.cfg
		s.pinSelection()
		s.cfg = s.overrides.apply(cfg.Defaults)
		if s.binding != nil && (s.cfg.Model != s.binding.Model || s.cfg.Account != previous.Account) {
			return nil, nil, errBoundModel
		}
		if err := p.validateExistingSession(s, r.preparedConfig); err != nil {
			return nil, nil, err
		}
		if err := p.preparePrompts(s); err != nil {
			return nil, nil, err
		}
	}
	return p, diags, nil
}
