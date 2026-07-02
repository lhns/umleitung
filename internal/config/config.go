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
	Gmail    Endpoint
	Stalwart Endpoint

	PollInterval time.Duration // safety-net full reconcile interval
	IdleReset    time.Duration // re-IDLE before Gmail's ~29min forced logout

	StatePath string
	LockPath  string

	SeedDest  SeedMode
	DestGuard bool
	UIDBatch  int
	CarrySeen bool

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
		Gmail: Endpoint{
			Host:     str("GMAIL_HOST", "imap.gmail.com"),
			Port:     num("GMAIL_PORT", 993),
			User:     str("GMAIL_USER", ""),
			Password: secret("GMAIL_APP_PASSWORD"),
			Folder:   str("GMAIL_FOLDER", "[Gmail]/All Mail"),
			TLS:      boolean("GMAIL_TLS", true),
		},
		Stalwart: Endpoint{
			Host:     str("STALWART_HOST", "mail.lhns.de"),
			Port:     num("STALWART_PORT", 993),
			User:     str("STALWART_USER", ""),
			Password: secret("STALWART_APP_PASSWORD"),
			Folder:   str("STALWART_FOLDER", "Gmail"),
			TLS:      boolean("STALWART_TLS", true),
		},
		PollInterval: time.Duration(num("POLL_INTERVAL", 900)) * time.Second,
		IdleReset:    time.Duration(num("IDLE_RESET", 1500)) * time.Second,
		StatePath:    str("STATE_PATH", "/state/umleiter.db"),
		LockPath:     str("LOCK_PATH", "/state/umleiter.lock"),
		SeedDest:     SeedMode(strings.ToLower(str("SEED_DEST", string(SeedEmpty)))),
		DestGuard:    boolean("DEST_GUARD", true),
		UIDBatch:     num("UID_BATCH", 2000),
		CarrySeen:    boolean("CARRY_SEEN", true),
		HealthAddr:   str("HEALTH_ADDR", ":8080"),
		LogLevel:     strings.ToLower(str("LOG_LEVEL", "info")),
	}

	if cfg.Gmail.User == "" {
		errs = append(errs, "GMAIL_USER is required")
	}
	if cfg.Gmail.Password == "" {
		errs = append(errs, "GMAIL_APP_PASSWORD (or GMAIL_APP_PASSWORD_FILE) is required")
	}
	if cfg.Stalwart.User == "" {
		errs = append(errs, "STALWART_USER is required")
	}
	if cfg.Stalwart.Password == "" {
		errs = append(errs, "STALWART_APP_PASSWORD (or STALWART_APP_PASSWORD_FILE) is required")
	}
	switch cfg.SeedDest {
	case SeedEmpty, SeedAlways, SeedNever:
	default:
		errs = append(errs, fmt.Sprintf("SEED_DEST: must be empty|always|never, got %q", cfg.SeedDest))
	}
	if cfg.UIDBatch < 1 {
		errs = append(errs, "UID_BATCH must be >= 1")
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}
