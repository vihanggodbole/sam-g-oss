package sam

// workModels keeps only the two definitions needed by one admitted operation.
// It is live memory, not a catalog version or an executable recovery snapshot.
type workModels struct {
	id            string
	main, summary resolvedModel
}

func (p *preparedConfig) selectWorkModels(s *sessionRow) (workModels, error) {
	var models workModels
	main, err := p.resolveBound(s)
	if err != nil {
		return models, err
	}
	spec, err := p.resolveCompaction(s, "", false)
	if err != nil {
		return models, err
	}
	if err := validateModelOptions(s.cfg, main, spec.model); err != nil {
		return models, errorf(ErrorCodeOf(err), "This chat's settings need repair before new work: %v. Update its settings or explicitly switch its model", err)
	}
	models.main, models.summary = main, spec.model
	return models, nil
}

// Called under mu immediately after admission commits, before publication can
// intervene. The same worker may own a newly admitted follow-up. Lazy request
// preparation and retries use these definitions, never the installed catalog.
func (r *Runtime) captureWork(sid string, w *worker) {
	x, err := loadExecution(r.db, sid)
	if err != nil {
		w.selectionError = r.storageFailed(err)
		return
	}
	if x == nil || x.Action == actionStop {
		return
	}
	id := x.TurnID
	if id == "" {
		id = x.CompactionID
	}
	if w.models.id == id {
		return
	}
	s, err := loadSession(r.db, sid)
	if err != nil {
		w.selectionError = r.storageFailed(err)
		return
	}
	w.models, w.selectionError = r.selectWorkModels(s)
	w.models.id = id
}
