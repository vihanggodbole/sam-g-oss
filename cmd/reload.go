package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
)

// SIGHUP and browser installs share one file authority and serialization point.
// A stalled read does not hold the runtime mutex or block process termination.
func (h *catalogHost) reload(ctx context.Context) error {
	if !h.mu.TryLock() {
		return catalogConflict("configuration is already being changed")
	}
	defer h.mu.Unlock()
	file, err := loadConfig(h.path)
	if err != nil {
		return err
	}
	file.Config.DocumentationDir = h.file.Config.DocumentationDir
	oldAuth, err := json.Marshal(h.file.Auth)
	if err != nil {
		return err
	}
	newAuth, err := json.Marshal(file.Auth)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(file.Server, h.file.Server) || !bytes.Equal(oldAuth, newAuth) {
		return fmt.Errorf("server and auth-manager settings require a restart; SIGHUP reloads runtime configuration and prompt contents only")
	}
	file.Config.Auth = h.rt.Auth()
	if err := h.rt.Reload(ctx, file.Config); err != nil {
		return err
	}
	h.file = file
	h.latest = map[catalogKey]*catalogProposal{}
	return nil
}
