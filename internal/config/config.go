// Package config loads Umleiter's YAML configuration.
//
// Defaults philosophy: an omitted field always gets its documented default —
// omission never disables anything. Disabling an on-by-default feature takes
// an explicit null (or ""), e.g. `health_addr: null`.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SeedMode controls when the copied-key set is seeded from the destination folder.
type SeedMode string

const (
	SeedEmpty  SeedMode = "empty"  // seed only when the local copied table is empty (default)
	SeedAlways SeedMode = "always" // seed on every startup
	SeedNever  SeedMode = "never"  // never seed
)

// DefaultConfigPath is used when CONFIG_PATH is not set.
const DefaultConfigPath = "/config/umleiter.yaml"

// Config is the resolved instance configuration.
type Config struct {
	HealthAddr string // "" = disabled (explicit null/"" in yaml)
	LogLevel   string
	StateDir   string
	LockPath   string
	Mirrors    []Mirror
}

// Mirror is one source→destination mail mirror.
type Mirror struct {
	Name         string
	StatePath    string
	PollInterval time.Duration
	IdleReset    time.Duration
	UIDBatch     int
	Seed         SeedMode
	DestGuard    bool
	CarrySeen    bool
	Source       Endpoint
	Dest         Endpoint
	Archive      Archive
	Sent         Sent
	Labels       Labels
}

// Endpoint describes one IMAPS endpoint.
type Endpoint struct {
	Host     string
	Port     int
	User     string
	Password string
	Folder   string
	Inbox    string // source only: the "in inbox" folder for archive routing
	TLS      bool
}

// Addr returns host:port.
func (e Endpoint) Addr() string { return fmt.Sprintf("%s:%d", e.Host, e.Port) }

// Archive groups the archive-routing settings.
type Archive struct {
	Enabled bool
	Folder  string
}

// Sent groups the sent-routing settings.
type Sent struct {
	Enabled      bool
	Folder       string // destination folder for sent mail
	SourceFolder string // source folder whose membership means "sent" (selector or name)
}

// Labels groups the label-sync settings.
type Labels struct {
	Enabled            bool
	Propagate          bool
	Exclude            []string
	KeywordPrefix      string // prepended to each keyword (e.g. "$label:" for Bulwark)
	KeywordReplacement string // sanitization replacement char (default "_"; "-" for Bulwark)
}

// ---- raw yaml schema (pointers/wrappers to distinguish absent from set) ----

// optString reads a raw yaml.Node to distinguish "key absent" (default
// applies) from "explicitly null or empty" (disabled). A bare yaml.Node
// field is the only construct yaml.v3 populates even for null values
// (custom unmarshalers are skipped for null).
func optString(n yaml.Node) (value string, set bool, err error) {
	if n.Kind == 0 {
		return "", false, nil // key absent
	}
	if n.Tag == "!!null" {
		return "", true, nil // explicit null -> disabled
	}
	err = n.Decode(&value)
	return value, true, err
}

// duration parses Go duration strings ("15m", "1h30m").
type duration time.Duration

func (d *duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = duration(v)
	return nil
}

type rawConfig struct {
	HealthAddr yaml.Node   `yaml:"health_addr"`
	LogLevel   string      `yaml:"log_level"`
	StateDir   string      `yaml:"state_dir"`
	LockPath   string      `yaml:"lock_path"`
	Mirrors    []rawMirror `yaml:"mirrors"`
}

type rawMirror struct {
	Name         string      `yaml:"name"`
	StatePath    string      `yaml:"state_path"`
	PollInterval duration    `yaml:"poll_interval"`
	IdleReset    duration    `yaml:"idle_reset"`
	UIDBatch     int         `yaml:"uid_batch"`
	Seed         string      `yaml:"seed"`
	DestGuard    *bool       `yaml:"dest_guard"`
	CarrySeen    *bool       `yaml:"carry_seen"`
	Source       rawEndpoint `yaml:"source"`
	Dest         rawEndpoint `yaml:"dest"`
	Archive      rawArchive  `yaml:"archive"`
	Sent         rawSent     `yaml:"sent"`
	Labels       rawLabels   `yaml:"labels"`
}

type rawEndpoint struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	User         string `yaml:"user"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	Folder       string `yaml:"folder"`
	Inbox        string `yaml:"inbox"`
	TLS          *bool  `yaml:"tls"`
}

type rawArchive struct {
	Enabled bool   `yaml:"enabled"`
	Folder  string `yaml:"folder"`
}

type rawSent struct {
	Enabled      bool   `yaml:"enabled"`
	Folder       string `yaml:"folder"`
	SourceFolder string `yaml:"source_folder"`
}

type rawLabels struct {
	Enabled            bool     `yaml:"enabled"`
	Propagate          bool     `yaml:"propagate"`
	Exclude            []string `yaml:"exclude"`
	KeywordPrefix      string   `yaml:"keyword_prefix"`
	KeywordReplacement string   `yaml:"keyword_replacement"`
}

// ---- loading ----

// Path returns the config file path (CONFIG_PATH env or the default).
func Path() string {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		return p
	}
	return DefaultConfigPath
}

var nameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

func isKeywordChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}

// LoadFile reads, defaults and validates the YAML configuration. Unknown
// keys are rejected (typo protection).
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w (set CONFIG_PATH or mount %s)", err, DefaultConfigPath)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return resolve(&raw)
}

func resolve(raw *rawConfig) (*Config, error) {
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	cfg := &Config{
		HealthAddr: ":8080",
		LogLevel:   strings.ToLower(defaultStr(raw.LogLevel, "info")),
		StateDir:   defaultStr(raw.StateDir, "/state"),
	}
	if v, set, err := optString(raw.HealthAddr); err != nil {
		fail("health_addr: %v", err)
	} else if set {
		cfg.HealthAddr = v // explicit null/"" disables
	}
	cfg.LockPath = defaultStr(raw.LockPath, filepath.Join(cfg.StateDir, "umleiter.lock"))

	if len(raw.Mirrors) == 0 {
		fail("at least one mirror is required")
	}
	names := map[string]bool{}
	statePaths := map[string]string{}
	for i := range raw.Mirrors {
		rm := &raw.Mirrors[i]
		m := Mirror{
			Name:         rm.Name,
			PollInterval: defaultDur(rm.PollInterval, 15*time.Minute),
			IdleReset:    defaultDur(rm.IdleReset, 25*time.Minute),
			UIDBatch:     defaultInt(rm.UIDBatch, 2000),
			Seed:         SeedMode(strings.ToLower(defaultStr(rm.Seed, string(SeedEmpty)))),
			DestGuard:    defaultBool(rm.DestGuard, true),
			CarrySeen:    defaultBool(rm.CarrySeen, true),
			Archive: Archive{
				Enabled: rm.Archive.Enabled,
				Folder:  defaultStr(rm.Archive.Folder, "Archive"),
			},
			Sent: Sent{
				Enabled:      rm.Sent.Enabled,
				Folder:       defaultStr(rm.Sent.Folder, "Sent"),
				SourceFolder: defaultStr(rm.Sent.SourceFolder, `\Sent`),
			},
			Labels: Labels{
				Enabled:            rm.Labels.Enabled,
				Propagate:          rm.Labels.Propagate,
				Exclude:            rm.Labels.Exclude,
				KeywordPrefix:      rm.Labels.KeywordPrefix,
				KeywordReplacement: defaultStr(rm.Labels.KeywordReplacement, "_"),
			},
		}

		where := fmt.Sprintf("mirror %q", rm.Name)
		if rm.Name == "" {
			where = fmt.Sprintf("mirror #%d", i+1)
			fail("%s: name is required", where)
		} else if !nameRe.MatchString(rm.Name) {
			fail("%s: name must match %s", where, nameRe)
		} else if names[rm.Name] {
			fail("%s: duplicate name", where)
		}
		names[rm.Name] = true

		m.StatePath = defaultStr(rm.StatePath, filepath.Join(cfg.StateDir, rm.Name+".db"))
		if other, dup := statePaths[m.StatePath]; dup {
			fail("%s: state_path %q already used by mirror %q", where, m.StatePath, other)
		}
		statePaths[m.StatePath] = rm.Name

		var err error
		if m.Source, err = resolveEndpoint(&rm.Source, "INBOX"); err != nil {
			fail("%s source: %v", where, err)
		}
		if m.Dest, err = resolveEndpoint(&rm.Dest, "INBOX"); err != nil {
			fail("%s dest: %v", where, err)
		}

		switch m.Seed {
		case SeedEmpty, SeedAlways, SeedNever:
		default:
			fail("%s: seed must be empty|always|never, got %q", where, m.Seed)
		}
		if m.Labels.Propagate && !m.Labels.Enabled {
			fail("%s: labels.propagate requires labels.enabled", where)
		}
		if r := m.Labels.KeywordReplacement; len([]rune(r)) != 1 || !isKeywordChar(r[0]) {
			fail("%s: labels.keyword_replacement must be a single keyword char [A-Za-z0-9_-], got %q", where, r)
		}
		if m.Archive.Enabled && m.Archive.Folder == m.Dest.Folder {
			fail("%s: archive.folder must differ from dest.folder", where)
		}
		if m.Sent.Enabled {
			if m.Sent.Folder == m.Dest.Folder {
				fail("%s: sent.folder must differ from dest.folder", where)
			}
			if m.Archive.Enabled && m.Sent.Folder == m.Archive.Folder {
				fail("%s: sent.folder must differ from archive.folder", where)
			}
		}
		if m.UIDBatch < 1 {
			fail("%s: uid_batch must be >= 1", where)
		}

		cfg.Mirrors = append(cfg.Mirrors, m)
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

func resolveEndpoint(raw *rawEndpoint, defaultFolder string) (Endpoint, error) {
	ep := Endpoint{
		Host:   raw.Host,
		Port:   defaultInt(raw.Port, 993),
		User:   raw.User,
		Folder: defaultStr(raw.Folder, defaultFolder),
		Inbox:  defaultStr(raw.Inbox, "INBOX"),
		TLS:    defaultBool(raw.TLS, true),
	}
	if ep.Host == "" {
		return ep, fmt.Errorf("host is required")
	}
	if ep.User == "" {
		return ep, fmt.Errorf("user is required")
	}
	switch {
	case raw.Password != "" && raw.PasswordFile != "":
		return ep, fmt.Errorf("set either password or password_file, not both")
	case raw.Password != "":
		ep.Password = raw.Password
	case raw.PasswordFile != "":
		b, err := os.ReadFile(raw.PasswordFile)
		if err != nil {
			return ep, fmt.Errorf("password_file: %w", err)
		}
		ep.Password = strings.TrimSpace(string(b))
	default:
		return ep, fmt.Errorf("password (or password_file) is required")
	}
	return ep, nil
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func defaultDur(v duration, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return time.Duration(v)
}

func defaultBool(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}
