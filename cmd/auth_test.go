package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthCommandRejectsUnknownBeforeLoadingConfig(t *testing.T) {
	config := filepath.Join(t.TempDir(), "missing.json")
	for _, command := range []string{"migrate", "unknown"} {
		err := authCommand([]string{command, "-config", config}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "unknown auth command") {
			t.Fatalf("%s: expected command validation before config access, got %v", command, err)
		}
	}
}
