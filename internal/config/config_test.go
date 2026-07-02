package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("SOURCE_HOST", "imap.example.com")
	t.Setenv("SOURCE_USER", "u@example.com")
	t.Setenv("SOURCE_PASSWORD", "sp")
	t.Setenv("DEST_HOST", "mail.example.org")
	t.Setenv("DEST_USER", "u@example.org")
	t.Setenv("DEST_PASSWORD", "dp")
}

func TestDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.Folder != "INBOX" || cfg.Dest.Folder != "INBOX" {
		t.Fatalf("folder defaults = %q / %q, want INBOX", cfg.Source.Folder, cfg.Dest.Folder)
	}
	if !cfg.Source.TLS || !cfg.Dest.TLS {
		t.Fatal("TLS must default to true")
	}
	if cfg.SeedDest != SeedEmpty || !cfg.DestGuard || cfg.UIDBatch != 2000 {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if cfg.Source.Addr() != "imap.example.com:993" {
		t.Fatalf("addr = %q", cfg.Source.Addr())
	}
}

func TestSecretFileVariant(t *testing.T) {
	setRequired(t)
	secretPath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOURCE_PASSWORD", "")
	t.Setenv("SOURCE_PASSWORD_FILE", secretPath)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.Password != "from-file" {
		t.Fatalf("password = %q, want trimmed file contents", cfg.Source.Password)
	}
}

func TestMissingRequired(t *testing.T) {
	for _, name := range []string{"SOURCE_HOST", "SOURCE_USER", "SOURCE_PASSWORD", "DEST_HOST", "DEST_USER", "DEST_PASSWORD"} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(name, "")
			if _, err := Load(); err == nil {
				t.Fatalf("want error for missing %s", name)
			}
		})
	}
}

func TestInvalidSeedMode(t *testing.T) {
	setRequired(t)
	t.Setenv("SEED_DEST", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("want error for invalid SEED_DEST")
	}
}
