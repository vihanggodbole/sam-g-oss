package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	sam "github.com/vihanggodbole/sam-g-oss"
)

// models.json is the host's selected configuration, not a provider catalog or
// credential store. Load it before Open so suspended work is immediately usable
// by Resume without an all-idle Reload.
type modelConfiguration struct {
	Version int                  `json:"version"`
	Models  map[string]sam.Model `json:"models"`
}

func (a *app) loadModels(ctx context.Context) error {
	path := a.modelPath()
	raw, err := os.ReadFile(path)
	if err == nil {
		var saved modelConfiguration
		if json.Unmarshal(raw, &saved) != nil || saved.Version != 1 || saved.Models == nil {
			return fmt.Errorf("invalid example model configuration: %s; restore a valid file rather than removing conversation data", path)
		}
		a.cfg.Models = saved.Models
		return a.checkModelProviders()
	}
	if !os.IsNotExist(err) {
		return err
	}
	// One-time upgrade from the earlier example. This temporary Open is
	// passive; no Resume, discovery or provider requests occur. Saved bindings
	// and account overrides remain unchanged, including suspended operations.
	a.rt, err = sam.Open(ctx, a.cfg)
	if err != nil {
		return err
	}
	err = a.restoreModels(ctx)
	closeErr := a.rt.Close(context.Background())
	a.rt = nil
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return a.saveModels(a.cfg.Models)
}

func (a *app) checkModelProviders() error {
	for _, model := range a.cfg.Models {
		if _, ok := a.cfg.Providers[model.Provider]; !ok {
			return fmt.Errorf("saved provider %q is not configured; history has not been removed", model.Provider)
		}
	}
	return nil
}

func (a *app) modelPath() string {
	return filepath.Join(filepath.Dir(a.cfg.DatabasePath), "models.json")
}

func (a *app) saveModels(models map[string]sam.Model) error {
	raw, err := json.MarshalIndent(modelConfiguration{Version: 1, Models: models}, "", "  ")
	if err != nil {
		return err
	}
	path := a.modelPath()
	file, err := os.CreateTemp(filepath.Dir(path), ".models-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (a *app) installModel(ctx context.Context, alias string, model sam.Model) error {
	if old, ok := a.cfg.Models[alias]; ok {
		if old.Provider != model.Provider || old.ModelID != model.ModelID {
			return problem(sam.CodeConflict, "A saved model alias cannot be redirected to another provider or model.")
		}
		if sameModel(old, model) {
			return nil
		}
	}
	candidate := a.cfg
	candidate.Models = maps.Clone(a.cfg.Models)
	candidate.Models[alias] = model
	if err := a.rt.UpdateModelsAndSave(ctx, candidate.Models, func() error { return a.saveModels(candidate.Models) }); err != nil {
		return err
	}
	a.cfg.Models = candidate.Models
	return nil
}

func sameModel(a, b sam.Model) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func (a *app) configureModel(ctx context.Context, c command) (any, error) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	if c.Definition == nil {
		if err := a.checkAccount(c.Provider, c.Account); err != nil {
			return nil, err
		}
		if c.Model == "" || len(c.Model) > 256 {
			return nil, problem(sam.CodeInvalidArgument, "Choose an upstream model.")
		}
		alias := c.Provider + "/" + c.Model
		if _, exists := a.cfg.Models[alias]; !exists {
			if err := a.installModel(ctx, alias, chatModel(c.Provider, c.Model, 0)); err != nil {
				return nil, err
			}
		}
		return map[string]string{"model": alias}, nil
	}
	alias := c.Model
	if alias == "" {
		alias = c.Definition.Provider + "/" + c.Definition.ModelID
	}
	if alias == "" || c.Definition.ModelID == "" || len(alias) > 512 {
		return nil, problem(sam.CodeInvalidArgument, "Choose a valid model alias and upstream model ID.")
	}
	if _, ok := a.cfg.Providers[c.Definition.Provider]; !ok {
		return nil, problem(sam.CodeInvalidArgument, "The model's provider is not configured.")
	}
	model := *c.Definition
	if c.Provider != "" { // catalog registration; explicit editor budgets are not clamped
		if c.Provider != model.Provider {
			return nil, problem(sam.CodeInvalidArgument, "The selected provider does not match the model definition.")
		}
		if err := a.checkAccount(c.Provider, c.Account); err != nil {
			return nil, err
		}
		if _, exists := a.cfg.Models[alias]; exists {
			return map[string]string{"model": alias}, nil
		}
		model.ContextTokens = chatModel(model.Provider, model.ModelID, model.ContextTokens).ContextTokens
	}
	if err := a.installModel(ctx, alias, model); err != nil {
		return nil, err
	}
	return map[string]string{"model": alias}, nil
}
