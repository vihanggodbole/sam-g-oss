package sam

import "context"

// UpdateModels replaces only the installed model definitions for new work.
// Running turns and manual compactions retain their captured main and summary
// definitions. No session needs to be idle. Prompts, settings, providers, tools,
// hooks and client resources are unchanged. Definitions are validated, not
// existing chats: a chat's next turn may require an explicit settings repair.
// The supplied map and model slices/pointers are copied. Restart/recovery uses
// the latest configuration; no historical executable catalog is retained.
func (r *Runtime) UpdateModels(ctx context.Context, models map[string]Model) error {
	return r.applyModels(ctx, models, nil)
}

// UpdateModelsAndSave has UpdateModels' scope, with the same host persistence
// boundary as ReloadAndSave. save runs outside the state mutex, after validation
// and before publication. Work admission, active workers, reads and Stop remain
// available while saving. Once save starts, caller cancellation cannot abort
// publication. Explicit Close still wins. A real
// save/publication failure seals admission and closes the runtime because the
// host file may already contain the new models. Hosts own atomic file replacement
// and must not use save to call mutations or bind it to caller cancellation.
func (r *Runtime) UpdateModelsAndSave(ctx context.Context, models map[string]Model, save func() error) error {
	if save == nil {
		return errorf(CodeInvalidArgument, "save callback is required")
	}
	called := false
	err := r.applyModels(ctx, models, func() error { called = true; return save() })
	if err != nil && called {
		_ = r.Close(context.Background())
		return errorf(CodeStorageError, "model catalog save/publication failed; the runtime has been closed and the file may contain the new models. Inspect storage and restart: %v", err)
	}
	return err
}

func (r *Runtime) applyModels(ctx context.Context, models map[string]Model, save func() error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.configMu.TryLock() {
		return errorf(CodeConflict, "configuration preparation is already in progress; retry after it finishes")
	}
	defer r.configMu.Unlock()
	r.mu.Lock()
	candidate := Config{Models: models, Providers: r.cfg.Providers}.clone()
	err = r.admit()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if err := candidate.validateModels(); err != nil {
		return err
	}
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	if err := r.admit(); err != nil {
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	saving, locked := false, false
	defer func() {
		if !locked {
			r.mu.Lock()
		}
		defer r.mu.Unlock()
		if saving && err != nil {
			r.storageFailed(err)
		}
	}()
	if save != nil {
		saving = true
		ctx = context.WithoutCancel(ctx)
		if err := save(); err != nil {
			return err
		}
	}
	r.mu.Lock()
	locked = true
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.admit(); err != nil {
		return err
	}
	// Do not swap preparedConfig or cfg: workers read unrelated immutable
	// fields without mu. Only catalog readers (under mu or configMu) see Models.
	r.cfg.Models = candidate.Models
	return nil
}
