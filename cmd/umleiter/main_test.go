package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: a mirror failing to start stopped the instance with exit code
// 0, so an on-failure restart policy never restarted it.
func TestRunExitsNonZeroWhenMirrorFails(t *testing.T) {
	dir := t.TempDir()
	// state_path is a directory, so the mirror fails opening its state db
	// before ever dialing.
	cfg := `health_addr: ""
state_dir: ` + filepath.ToSlash(dir) + `
mirrors:
  - name: broken
    state_path: ` + filepath.ToSlash(dir) + `
    source: {host: 127.0.0.1, user: u, password: p}
    dest: {host: 127.0.0.1, user: u, password: p}
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)

	if code := run(); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}

func TestRunInvalidConfig(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	if code := run(); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
}
