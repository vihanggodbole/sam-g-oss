// Package hostpath resolves paths for the bundled hosts, not the embedded runtime.
package hostpath

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve expands a leading ~/ using the host user's home directory. Explicit
// relative paths retain their working-directory meaning. No shell expansion is
// performed, and no files or directories are created.
func Resolve(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")), nil
}
