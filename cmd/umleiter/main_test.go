package main

import (
	"net/http"
	"net/http/httptest"
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
	t.Setenv("CONFIG_PATH", writeConfig(t, cfg))

	if code := run(); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}

func TestHealthcheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int // 0 = health endpoint disabled in config
		want   int
	}{
		{"healthy", http.StatusOK, 0},
		{"stale", http.StatusServiceUnavailable, 1},
		{"disabled", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := `""`
			if tc.status != 0 {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/healthz" {
						http.NotFound(w, r)
						return
					}
					w.WriteHeader(tc.status)
				}))
				defer srv.Close()
				addr = srv.Listener.Addr().String()
			}
			t.Setenv("CONFIG_PATH", writeConfig(t, "health_addr: "+addr+"\n"+minimalMirror))
			if got := healthcheck(); got != tc.want {
				t.Fatalf("healthcheck() = %d, want %d", got, tc.want)
			}
		})
	}
}

const minimalMirror = `mirrors:
  - name: m
    source: {host: 127.0.0.1, user: u, password: p}
    dest: {host: 127.0.0.1, user: u, password: p}
`

func writeConfig(t *testing.T, cfg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunInvalidConfig(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "missing.yaml"))
	if code := run(); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
}
