package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	sam "github.com/vihanggodbole/sam-g-oss"
	"github.com/vihanggodbole/sam-g-oss/internal/hostpath"
	"github.com/vihanggodbole/sam-g-oss/server"
)

type (
	catalogKey      struct{ provider, account string }
	catalogProposal struct {
		id      string
		base    [32]byte
		models  []sam.DiscoveredModel
		ready   bool
		applied *server.InstallCatalogRequest
		result  server.InstallCatalogResult
	}
)

// One file remains authoritative. Proposals are only the latest explicit lookup
// per scope, discarded on restart/reload; every Discover still performs a GET.
// The mutex serializes file reload/save, never provider I/O or runtime reads.
type catalogHost struct {
	mu     sync.Mutex
	path   string
	rt     *sam.Runtime
	file   fileConfig
	latest map[catalogKey]*catalogProposal
}

func newCatalogHost(path string, rt *sam.Runtime, file fileConfig) (*catalogHost, error) {
	path, err := hostpath.Resolve(path)
	if err == nil {
		path, err = filepath.Abs(path)
	}
	return &catalogHost{path: path, rt: rt, file: file, latest: map[catalogKey]*catalogProposal{}}, err
}

func catalogConflict(message string) error {
	return &sam.Error{Code: sam.CodeConflict, Message: message}
}

func (h *catalogHost) Discover(ctx context.Context, provider, account string) (server.CatalogSnapshot, error) {
	if !h.mu.TryLock() {
		return server.CatalogSnapshot{}, catalogConflict("configuration is being changed; retry discovery")
	}
	key := catalogKey{provider, account}
	p := &catalogProposal{id: rand.Text(), base: sha256.Sum256(h.file.raw)}
	h.latest[key] = p // starting a newer lookup invalidates the old proposal, even if it fails
	h.mu.Unlock()
	models, err := h.rt.DiscoverModels(ctx, provider, account)
	if !h.mu.TryLock() {
		return server.CatalogSnapshot{}, catalogConflict("configuration changed during discovery; retry")
	}
	defer h.mu.Unlock()
	if h.latest[key] != p || p.base != sha256.Sum256(h.file.raw) {
		return server.CatalogSnapshot{}, catalogConflict("discovery was superseded; fetch the catalog again")
	}
	if err != nil {
		delete(h.latest, key)
		return server.CatalogSnapshot{}, err
	}
	raw, err := json.Marshal(models)
	if err != nil {
		return server.CatalogSnapshot{}, err
	}
	if err = json.Unmarshal(raw, &p.models); err != nil {
		return server.CatalogSnapshot{}, err
	}
	p.ready = true
	return server.CatalogSnapshot{Models: models, SnapshotID: p.id}, nil
}

func (h *catalogHost) Install(ctx context.Context, req server.InstallCatalogRequest) (server.InstallCatalogResult, error) {
	var result server.InstallCatalogResult
	if !h.mu.TryLock() {
		return result, catalogConflict("configuration is being changed; retry installation")
	}
	defer h.mu.Unlock()
	p := h.latest[catalogKey{req.Provider, req.Account}]
	if p == nil || !p.ready || p.id != req.SnapshotID {
		return result, catalogConflict("snapshot expired or superseded; discover models again")
	}
	if len(p.models) > 0 && req.ContextTokens < 0 {
		return result, &sam.Error{Code: sam.CodeInvalidArgument, Message: "local context budget cannot be negative"}
	}
	current, err := os.ReadFile(h.path)
	if err != nil {
		return result, err
	}
	if !bytes.Equal(current, h.file.raw) {
		return result, catalogConflict("configuration file changed externally; reload it before installing")
	}
	if p.applied != nil {
		if *p.applied == req {
			return p.result, nil
		}
		return result, catalogConflict("this snapshot was already installed with different options; discover again")
	}
	if p.base != sha256.Sum256(current) {
		return result, catalogConflict("configuration changed since discovery; discover again")
	}
	if len(p.models) == 0 && !req.ConfirmEmpty {
		return result, catalogConflict("empty catalog: explicit confirmation is required to clear this scope")
	}
	candidate, result, raw, err := catalogCandidate(h.file, req, p.models)
	if err != nil {
		return result, err
	}
	if err := h.rt.UpdateModelsAndSave(ctx, candidate.Models, func() error { return replaceConfig(h.path, current, raw) }); err != nil {
		return result, err
	}
	h.file.Config.Models, h.file.raw = candidate.Models, raw
	p.applied, p.result = &req, result
	return result, nil
}

func catalogCandidate(file fileConfig, req server.InstallCatalogRequest, models []sam.DiscoveredModel) (sam.Config, server.InstallCatalogResult, []byte, error) {
	var result server.InstallCatalogResult
	candidate := file.Config
	candidate.Models = maps.Clone(candidate.Models)
	if candidate.Models == nil {
		candidate.Models = map[string]sam.Model{}
	}
	for alias, m := range candidate.Models {
		if m.Provider == req.Provider && m.CatalogAccount != nil && *m.CatalogAccount == req.Account {
			delete(candidate.Models, alias)
			result.Removed++
		}
	}
	for _, discovered := range models {
		alias := catalogAlias(req.Provider, req.Account, discovered.ID)
		if _, collision := candidate.Models[alias]; collision {
			return candidate, result, nil, catalogConflict("managed alias collides with a manual model; rename the manual entry first")
		}
		m := sam.ConfiguredModel(req.Provider, req.Account, discovered, req.ContextTokens)
		if canRetainReasoningSummaries(file.Models[alias], m, discovered.ReasoningSummaries) {
			m.ReasoningSummaries = true
		}
		candidate.Models[alias] = m
		result.Installed++
		if m.Unavailable == "" {
			result.Usable++
		}
		if _, existed := file.Models[alias]; existed {
			result.Removed--
		}
	}
	// Patch only models in the original JSON. Preserve unknown host settings,
	// explicit paths, defaults, auth configuration and unrelated model entries.
	var document map[string]json.RawMessage
	if err := json.Unmarshal(file.raw, &document); err != nil {
		return candidate, result, nil, err
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(document["models"], &stored); err != nil && document["models"] != nil {
		return candidate, result, nil, err
	}
	if stored == nil {
		stored = map[string]json.RawMessage{}
	}
	for alias, m := range file.Models {
		if m.Provider == req.Provider && m.CatalogAccount != nil && *m.CatalogAccount == req.Account {
			delete(stored, alias)
		}
	}
	for _, d := range models {
		alias := catalogAlias(req.Provider, req.Account, d.ID)
		encoded, err := json.Marshal(candidate.Models[alias])
		if err != nil {
			return candidate, result, nil, err
		}
		stored[alias] = encoded
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return candidate, result, nil, err
	}
	document["models"] = encoded
	raw, err := json.MarshalIndent(document, "", "  ")
	return candidate, result, append(raw, '\n'), err
}

func catalogAlias(provider, account, id string) string {
	return "catalog/" + url.PathEscape(provider) + "/" + url.PathEscape(account) + "/" + url.PathEscape(id)
}

// Unknown provider metadata must not erase an operator's declaration for the
// same managed Responses model. Native Messages display is profile-owned.
func canRetainReasoningSummaries(previous, next sam.Model, reported *bool) bool {
	return reported == nil && previous.ReasoningSummaries &&
		next.API == sam.APIResponses && (previous.API == "" || previous.API == sam.APIResponses) &&
		previous.Provider == next.Provider && previous.ModelID == next.ModelID &&
		previous.CatalogAccount != nil && next.CatalogAccount != nil &&
		*previous.CatalogAccount == *next.CatalogAccount
}

// Config edits are operator-owned: no transparent merging with an external
// writer. Save to a private sibling and rename; never truncate the live file.
func replaceConfig(path string, expected, next []byte) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("automatic installation needs a regular config file, not a symlink or special file")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return fmt.Errorf("configuration changed before save")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sam-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(next); err == nil {
		err = f.Sync()
	}
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
