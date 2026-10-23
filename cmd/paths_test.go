package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".sam-g")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	for _, test := range []struct {
		name, config, database, auth string
	}{
		{"defaults", `{"auth":{}}`, filepath.Join(dir, "sessions.db"), filepath.Join(dir, "auth", "auth.db")},
		{"home", `{"database_path":"~/.sam-g/custom.db","auth":{"database_path":"~/.sam-g/custom-auth/auth.db"}}`, filepath.Join(dir, "custom.db"), filepath.Join(dir, "custom-auth", "auth.db")},
		{"explicit", `{"database_path":"custom/sessions.db","auth":{"database_path":"custom/auth.db"}}`, "custom/sessions.db", "custom/auth.db"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig("~/.sam-g/config.json")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DatabasePath != test.database || cfg.Auth.DatabasePath != test.auth {
				t.Fatalf("paths: %q, %q", cfg.DatabasePath, cfg.Auth.DatabasePath)
			}
		})
	}
	// Loading settings must not create a database or turn auth on implicitly.
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil || cfg.Auth != nil {
		t.Fatalf("load without auth: %+v, %v", cfg.Auth, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("loading created state: %v, %v", entries, err)
	}
}
