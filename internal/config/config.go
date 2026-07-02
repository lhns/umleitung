// Package config loads all Umleiter configuration from environment variables.
// Secrets support *_FILE variants (Docker/Swarm secrets mounted as files).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// SeedMode controls when the copied-key set is seeded from the destination folder.
type SeedMode string

const (
	SeedEmpty  SeedMode = "empty"  // seed only when the local copied table is empty (default)
	SeedAlways SeedMode = "always" // seed on every startup
	SeedNever  SeedMode = "never"  // never seed
)

// Endpoint describes one IMAPS endpoint (host, credentials, folder).
type Endpoint struct {
	Host     string
	Port     int
	User     string
	Password string
	Folder   string
	TLS      bool // implicit TLS (IMAPS); disable only for local testing
}

// Addr returns host:port.
func (e Endpoint) Addr() string { return fmt.Sprintf("%s:%d", e.Host, e.Port) }

// Config is the full runtime configuration.
type Config struct {
	Source Endpoint
	Dest   Endpoint

	PollInterval time.Duration // safety-net full reconcile interval
	IdleReset    time.Duration // re-IDLE before the server force-drops idle connections (Gmail: ~29min)

	StatePath string
	LockPath  string

	SeedDest  SeedMode
	DestGuard bool
	UIDBatch  int
	CarrySeen bool

	SyncLabels   bool     // mirror source label-folder membership as dest keywords
	LabelExclude []string // folder names excluded from the label scan

	ArchiveRouting bool   // route by source-INBOX membership; propagate archive moves
	SourceInbox    string // source folder whose membership means "in inbox"
	ArchiveFolder  string // destination folder for archived mail
	LabelPropagate bool   // STORE keyword changes for post-copy label changes (needs SyncLabels)

	HealthAddr string // empty = disabled
	LogLevel   string
}

// Load reads configuration from the environment, applying defaults and
// validating required values.
func Load() (*Config, error) {
	var errs []string

	str := func(name, def string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return def
	}

	secret := func(name string) string {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		if path, ok := os.LookupEnv(name + "_FILE"); ok && path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s_FILE: %v", name, err))
				return ""
			}
			return strings.TrimSpace(string(b))
		}
		return ""
	}

	num := func(name string, def int) int {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: not a number: %q", name, v))
			return def
		}
		return n
	}

	csv := func(name string) []string {
		v := os.Getenv(name)
		if v == "" {
			return nil
		}
		var out []string
		for part := range strings.SplitSeq(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	boolean := func(name string, def bool) bool {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: not a bool: %q", name, v))
			return def
		}
		return b
	}

	cfg := &Config{
		Source: Endpoint{
			Host:     str("SOURCE_HOST", ""),
			Port:     num("SOURCE_PORT", 993),
			User:     str("SOURCE_USER", ""),
			Password: secret("SOURCE_PASSWORD"),
			Folder:   str("SOURCE_FOLDER", "INBOX"),
			TLS:      boolean("SOURCE_TLS", true),
		},
		Dest: Endpoint{
			Host:     str("DEST_HOST", ""),
			Port:     num("DEST_PORT", 993),
			User:     str("DEST_USER", ""),
			Password: secret("DEST_PASSWORD"),
			Folder:   str("DEST_FOLDER", "INBOX"),
			TLS:      boolean("DEST_TLS", true),
		},
		PollInterval: time.Duration(num("POLL_INTERVAL", 900)) * time.Second,
		IdleReset:    time.Duration(num("IDLE_RESET", 1500)) * time.Second,
		StatePath:    str("STATE_PATH", "/state/umleiter.db"),
		LockPath:     str("LOCK_PATH", "/state/umleiter.lock"),
		SeedDest:     SeedMode(strings.ToLower(str("SEED_DEST", string(SeedEmpty)))),
		DestGuard:    boolean("DEST_GUARD", true),
		UIDBatch:     num("UID_BATCH", 2000),
		CarrySeen:    boolean("CARRY_SEEN", true),
		SyncLabels:   boolean("SYNC_LABELS", false),
		LabelExclude: csv("LABEL_EXCLUDE"),
		ArchiveRouting: boolean("ARCHIVE_ROUTING", false),
		SourceInbox:    str("SOURCE_INBOX", "INBOX"),
		ArchiveFolder:  str("DEST_ARCHIVE_FOLDER", "Archive"),
		LabelPropagate: boolean("LABEL_PROPAGATE", false),
		HealthAddr:   str("HEALTH_ADDR", ":8080"),
		LogLevel:     strings.ToLower(str("LOG_LEVEL", "info")),
	}

	if cfg.Source.Host == "" {
		errs = append(errs, "SOURCE_HOST is required")
	}
	if cfg.Source.User == "" {
		errs = append(errs, "SOURCE_USER is required")
	}
	if cfg.Source.Password == "" {
		errs = append(errs, "SOURCE_PASSWORD (or SOURCE_PASSWORD_FILE) is required")
	}
	if cfg.Dest.Host == "" {
		errs = append(errs, "DEST_HOST is required")
	}
	if cfg.Dest.User == "" {
		errs = append(errs, "DEST_USER is required")
	}
	if cfg.Dest.Password == "" {
		errs = append(errs, "DEST_PASSWORD (or DEST_PASSWORD_FILE) is required")
	}
	switch cfg.SeedDest {
	case SeedEmpty, SeedAlways, SeedNever:
	default:
		errs = append(errs, fmt.Sprintf("SEED_DEST: must be empty|always|never, got %q", cfg.SeedDest))
	}
	if cfg.UIDBatch < 1 {
		errs = append(errs, "UID_BATCH must be >= 1")
	}
	if cfg.LabelPropagate && !cfg.SyncLabels {
		errs = append(errs, "LABEL_PROPAGATE requires SYNC_LABELS=true")
	}
	if cfg.ArchiveRouting && cfg.ArchiveFolder == cfg.Dest.Folder {
		errs = append(errs, "DEST_ARCHIVE_FOLDER must differ from DEST_FOLDER when ARCHIVE_ROUTING is enabled")
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}
