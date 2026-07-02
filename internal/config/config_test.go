package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("GMAIL_USER", "u@gmail.com")
	t.Setenv("GMAIL_APP_PASSWORD", "gp")
	t.Setenv("STALWART_USER", "u@lhns.de")
	t.Setenv("STALWART_APP_PASSWORD", "sp")
}

func TestDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gmail.Folder != "[Gmail]/All Mail" {
		t.Fatalf("gmail folder = %q", cfg.Gmail.Folder)
	}
	if cfg.Stalwart.Folder != "Gmail" {
		t.Fatalf("stalwart folder default = %q, want Gmail", cfg.Stalwart.Folder)
	}
	if cfg.SeedDest != SeedEmpty || !cfg.DestGuard || cfg.UIDBatch != 2000 {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if cfg.Gmail.Addr() != "imap.gmail.com:993" {
		t.Fatalf("addr = %q", cfg.Gmail.Addr())
	}
}

func TestSecretFileVariant(t *testing.T) {
	setRequired(t)
	secretPath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GMAIL_APP_PASSWORD", "")
	t.Setenv("GMAIL_APP_PASSWORD_FILE", secretPath)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gmail.Password != "from-file" {
		t.Fatalf("password = %q, want trimmed file contents", cfg.Gmail.Password)
	}
}

func TestMissingRequired(t *testing.T) {
	setRequired(t)
	t.Setenv("STALWART_APP_PASSWORD", "")
	if _, err := Load(); err == nil {
		t.Fatal("want error for missing STALWART_APP_PASSWORD")
	}
}

func TestInvalidSeedMode(t *testing.T) {
	setRequired(t)
	t.Setenv("SEED_DEST", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("want error for invalid SEED_DEST")
	}
}
