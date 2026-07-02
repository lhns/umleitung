package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "umleiter.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `
mirrors:
  - name: pierre
    source: { host: imap.example.com, user: a@example.com, password: sp }
    dest:   { host: mail.example.org, user: a@example.org, password: dp }
`

func TestMinimalConfigGetsAllDefaults(t *testing.T) {
	cfg, err := LoadFile(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	// Omitted = default, never disabled.
	if cfg.HealthAddr != ":8080" {
		t.Fatalf("health_addr default = %q, want :8080", cfg.HealthAddr)
	}
	if cfg.LogLevel != "info" || cfg.StateDir != "/state" {
		t.Fatalf("global defaults: %+v", cfg)
	}
	if cfg.LockPath != filepath.Join("/state", "umleiter.lock") {
		t.Fatalf("lock_path default = %q", cfg.LockPath)
	}
	m := cfg.Mirrors[0]
	if m.PollInterval != 15*time.Minute || m.IdleReset != 25*time.Minute {
		t.Fatalf("duration defaults: %+v", m)
	}
	if m.UIDBatch != 2000 || m.Seed != SeedEmpty || !m.DestGuard || !m.CarrySeen {
		t.Fatalf("mirror defaults: %+v", m)
	}
	if m.StatePath != filepath.Join("/state", "pierre.db") {
		t.Fatalf("state_path default = %q", m.StatePath)
	}
	if !m.Source.TLS || !m.Dest.TLS || m.Source.Port != 993 {
		t.Fatalf("endpoint defaults: %+v", m.Source)
	}
	if m.Source.Folder != "INBOX" || m.Source.Inbox != "INBOX" {
		t.Fatalf("folder defaults: %+v", m.Source)
	}
	if m.Archive.Enabled || m.Archive.Folder != "Archive" {
		t.Fatalf("archive defaults: %+v", m.Archive)
	}
	if m.Labels.Enabled || m.Labels.Propagate {
		t.Fatalf("labels defaults: %+v", m.Labels)
	}
	if m.Sent.Enabled || m.Sent.Folder != "Sent" || m.Sent.SourceFolder != `\Sent` {
		t.Fatalf("sent defaults: %+v", m.Sent)
	}
}

func TestHealthAddrExplicitNullDisables(t *testing.T) {
	cfg, err := LoadFile(write(t, "health_addr: null\n"+minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthAddr != "" {
		t.Fatalf("explicit null: health_addr = %q, want disabled", cfg.HealthAddr)
	}
	cfg, err = LoadFile(write(t, `health_addr: ""`+"\n"+minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthAddr != "" {
		t.Fatalf("explicit empty: health_addr = %q, want disabled", cfg.HealthAddr)
	}
}

func TestFullSchema(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(write(t, `
health_addr: ":9090"
log_level: debug
state_dir: /data
lock_path: /data/custom.lock
mirrors:
  - name: pierre
    state_path: /data/legacy.db
    poll_interval: 5m
    idle_reset: 20m
    uid_batch: 500
    seed: always
    dest_guard: false
    carry_seen: false
    source:
      host: imap.gmail.com
      user: p@gmail.com
      password_file: `+secret+`
      folder: '\All'
      inbox: INBOX
    dest:
      host: mail.lhns.de
      user: p@lhns.de
      password: dp
      folder: INBOX
      tls: false
    archive:
      enabled: true
      folder: Archiv
    labels:
      enabled: true
      propagate: true
      exclude: [Notes, Some/Other]
  - name: other
    source: { host: imap.gmail.com, user: o@gmail.com, password: x, folder: '\All' }
    dest:   { host: mail.lhns.de, user: o@lhns.de, password: y }
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Mirrors) != 2 {
		t.Fatalf("mirrors = %d", len(cfg.Mirrors))
	}
	m := cfg.Mirrors[0]
	if m.Source.Password != "from-file" {
		t.Fatalf("password_file not resolved: %q", m.Source.Password)
	}
	if m.PollInterval != 5*time.Minute || m.DestGuard || m.CarrySeen {
		t.Fatalf("overrides not applied: %+v", m)
	}
	if !m.Archive.Enabled || m.Archive.Folder != "Archiv" {
		t.Fatalf("archive: %+v", m.Archive)
	}
	if !m.Labels.Propagate || len(m.Labels.Exclude) != 2 {
		t.Fatalf("labels: %+v", m.Labels)
	}
	if m.Dest.TLS {
		t.Fatal("explicit tls: false ignored")
	}
	if m.StatePath != "/data/legacy.db" {
		t.Fatalf("state_path override: %q", m.StatePath)
	}
	if cfg.Mirrors[1].StatePath != filepath.Join("/data", "other.db") {
		t.Fatalf("second mirror state_path: %q", cfg.Mirrors[1].StatePath)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"no mirrors", "mirrors: []", "at least one mirror"},
		{"bad name", strings.Replace(minimal, "pierre", "Pierre Müller", 1), "name must match"},
		{"dup names", minimal + `
  - name: pierre
    source: { host: h, user: u, password: p }
    dest:   { host: h, user: u, password: p }
`, "duplicate name"},
		{"missing host", `
mirrors:
  - name: a
    source: { user: u, password: p }
    dest:   { host: h, user: u, password: p }
`, "host is required"},
		{"missing password", `
mirrors:
  - name: a
    source: { host: h, user: u }
    dest:   { host: h, user: u, password: p }
`, "password (or password_file) is required"},
		{"both passwords", `
mirrors:
  - name: a
    source: { host: h, user: u, password: p, password_file: /x }
    dest:   { host: h, user: u, password: p }
`, "not both"},
		{"propagate without enabled", minimal + "    labels: { propagate: true }\n", "labels.propagate requires"},
		{"archive folder clash", minimal + "    archive: { enabled: true, folder: INBOX }\n", "must differ from dest.folder"},
		{"sent folder clash dest", minimal + "    sent: { enabled: true, folder: INBOX }\n", "sent.folder must differ from dest.folder"},
		{"sent folder clash archive", minimal + "    archive: { enabled: true }\n    sent: { enabled: true, folder: Archive }\n", "sent.folder must differ from archive.folder"},
		{"unknown key (typo)", "helth_addr: ':8080'\n" + minimal, "field helth_addr not found"},
		{"bad duration", minimal + "    poll_interval: soon\n", "invalid duration"},
		{"dup state path", `
state_dir: /s
mirrors:
  - name: a
    state_path: /s/same.db
    source: { host: h, user: u, password: p }
    dest:   { host: h, user: u, password: p }
  - name: b
    state_path: /s/same.db
    source: { host: h, user: u, password: p }
    dest:   { host: h, user: u, password: p }
`, "already used"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadFile(write(t, c.yaml))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

func TestPathEnv(t *testing.T) {
	t.Setenv("CONFIG_PATH", "/custom/path.yaml")
	if Path() != "/custom/path.yaml" {
		t.Fatalf("Path() = %q", Path())
	}
	t.Setenv("CONFIG_PATH", "")
	if Path() != DefaultConfigPath {
		t.Fatalf("Path() default = %q", Path())
	}
}
