// Package providers holds the configured agent instances: which CLI to
// run, from where, with what environment, and whether it is switched on.
// The built-in "claude" and "codex" instances always exist; more can be
// added, for example a second Claude with its own CLAUDE_CONFIG_DIR and
// account. Instance names are what threads store in their Agent field, so
// they never change once created.
package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"starcode/internal/agent"
	"starcode/internal/agent/claude"
	"starcode/internal/agent/codex"
	"starcode/internal/agent/fake"
)

// Drivers are the adapters an instance can be built on.
var Drivers = []string{"claude", "codex"}

// Colors an instance can be tagged with, as CSS values.
var Colors = []string{"#8497bd", "#d9774f", "#4fd1a5", "#f0b95b", "#f07178", "#b78bf5", "#5ec8d8"}

type Instance struct {
	Name   string `json:"name"`   // registry key, immutable
	Driver string `json:"driver"` // "claude" | "codex" | "fake"
	Label  string `json:"label,omitempty"`
	// Binary overrides the driver's default executable.
	Binary string `json:"binary,omitempty"`
	// ConfigDir becomes CLAUDE_CONFIG_DIR or CODEX_HOME for the child.
	ConfigDir string `json:"config_dir,omitempty"`
	// Env holds extra KEY=VALUE pairs (API keys, base URLs).
	Env     []string `json:"env,omitempty"`
	Color   string   `json:"color,omitempty"`
	Enabled bool     `json:"enabled"`
	// HiddenModels are catalog ids left out of the picker; ExtraModels are
	// ids added to it.
	HiddenModels []string `json:"hidden_models,omitempty"`
	ExtraModels  []string `json:"extra_models,omitempty"`
}

// BuiltIn instances share their driver's name and cannot be deleted.
func (in Instance) BuiltIn() bool { return in.Name == in.Driver }

// DisplayName is the label, the driver's product name for an unlabelled
// built-in, or the instance name.
func (in Instance) DisplayName() string {
	if in.Label != "" {
		return in.Label
	}
	if in.BuiltIn() {
		return DriverName(in.Driver)
	}
	return in.Name
}

// DriverName is the product behind a driver id.
func DriverName(driver string) string {
	switch driver {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "fake":
		return "Fake agent"
	}
	return driver
}

// ConfigDirVar is the environment variable ConfigDir sets for the driver.
func (in Instance) ConfigDirVar() string {
	switch in.Driver {
	case "claude":
		return "CLAUDE_CONFIG_DIR"
	case "codex":
		return "CODEX_HOME"
	}
	return ""
}

// ChildEnv is Env plus the config dir variable.
func (in Instance) ChildEnv() []string {
	env := append([]string(nil), in.Env...)
	if in.ConfigDir != "" && in.ConfigDirVar() != "" {
		env = append(env, in.ConfigDirVar()+"="+in.ConfigDir)
	}
	return env
}

type config struct {
	CheckIntervalSeconds int        `json:"check_interval_seconds"`
	Instances            []Instance `json:"instances"`
}

// Store is the on-disk instance list. Defaults maps a driver to the binary
// its built-in instance runs (the -claude and -codex flags).
type Store struct {
	mu       sync.Mutex
	path     string
	cfg      config
	defaults map[string]string
	log      *slog.Logger
}

const defaultCheckInterval = 3600

// Open reads path (missing is fine) and makes sure the built-ins exist.
func Open(path string, defaults map[string]string, withFake bool, log *slog.Logger) (*Store, error) {
	s := &Store{path: path, defaults: defaults, log: log}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &s.cfg); err != nil {
			return nil, fmt.Errorf("providers: %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.cfg.CheckIntervalSeconds == 0 {
		s.cfg.CheckIntervalSeconds = defaultCheckInterval
	}
	for _, d := range Drivers {
		if _, ok := s.find(d); !ok {
			s.cfg.Instances = append(s.cfg.Instances, Instance{Name: d, Driver: d, Enabled: true})
		}
	}
	if _, ok := s.find("fake"); withFake && !ok {
		s.cfg.Instances = append(s.cfg.Instances, Instance{Name: "fake", Driver: "fake", Enabled: true})
	}
	// Write the defaults out so the file is there to look at and edit.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) find(name string) (int, bool) {
	for i, in := range s.cfg.Instances {
		if in.Name == name {
			return i, true
		}
	}
	return -1, false
}

// Instances returns a copy, built-ins first, then by name.
func (s *Store) Instances() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Instance(nil), s.cfg.Instances...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BuiltIn() != out[j].BuiltIn() {
			return out[i].BuiltIn()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Store) Get(name string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.find(name)
	if !ok {
		return Instance{}, false
	}
	return s.cfg.Instances[i], true
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// Validate checks an instance before it is stored or built.
func Validate(in Instance) error {
	if !nameRe.MatchString(in.Name) {
		return errors.New("name must be lowercase letters, digits, - or _ (max 40)")
	}
	known := in.Driver == "fake"
	for _, d := range Drivers {
		known = known || d == in.Driver
	}
	if !known {
		return fmt.Errorf("unknown driver %q", in.Driver)
	}
	for _, kv := range in.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.ContainsAny(k, " \t") {
			return fmt.Errorf("environment entry %q is not KEY=value", kv)
		}
	}
	if in.ConfigDir != "" && !filepath.IsAbs(in.ConfigDir) {
		return errors.New("config dir must be an absolute path")
	}
	return nil
}

// Put stores in (adding or replacing by name) and writes the file. The
// driver of an existing instance cannot change: its threads' transcripts
// belong to that agent.
func (s *Store) Put(in Instance) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Label = strings.TrimSpace(in.Label)
	in.Binary = strings.TrimSpace(in.Binary)
	in.ConfigDir = strings.TrimSpace(in.ConfigDir)
	if err := Validate(in); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.find(in.Name); ok {
		if s.cfg.Instances[i].Driver != in.Driver {
			return errors.New("the driver of an existing instance cannot change")
		}
		s.cfg.Instances[i] = in
	} else {
		if in.Name == in.Driver {
			return fmt.Errorf("%q is the built-in instance's name", in.Name)
		}
		s.cfg.Instances = append(s.cfg.Instances, in)
	}
	return s.save()
}

// Delete removes an added instance. Built-ins can only be disabled.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.find(name)
	if !ok {
		return fmt.Errorf("no instance %q", name)
	}
	if s.cfg.Instances[i].BuiltIn() {
		return errors.New("built-in instances can be disabled but not deleted")
	}
	s.cfg.Instances = append(s.cfg.Instances[:i], s.cfg.Instances[i+1:]...)
	return s.save()
}

// CheckInterval is how often the version and sign-in checks rerun; zero
// turns the background checks off.
func (s *Store) CheckInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Duration(s.cfg.CheckIntervalSeconds) * time.Second
}

func (s *Store) SetCheckInterval(seconds int) error {
	if seconds < 0 {
		return errors.New("interval cannot be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.CheckIntervalSeconds = seconds
	return s.save()
}

// save writes the file with owner-only permissions: Env may carry keys.
func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Binary is the executable an instance runs: its own, or the driver's
// default.
func (s *Store) Binary(in Instance) string {
	if in.Binary != "" {
		return in.Binary
	}
	if b := s.defaults[in.Driver]; b != "" {
		return b
	}
	return in.Driver
}

// Build makes the adapter for an instance.
func (s *Store) Build(in Instance) (agent.Agent, error) {
	if err := Validate(in); err != nil {
		return nil, err
	}
	switch in.Driver {
	case "claude":
		return claude.New(claude.WithBinary(s.Binary(in)), claude.WithEnv(in.ChildEnv()), claude.WithLogger(s.log)), nil
	case "codex":
		return codex.New(codex.WithBinary(s.Binary(in)), codex.WithEnv(in.ChildEnv()), codex.WithLogger(s.log)), nil
	case "fake":
		return fake.New(), nil
	}
	return nil, fmt.Errorf("unknown driver %q", in.Driver)
}

// Slug turns a label into a candidate instance name.
func Slug(label string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_', r == '.':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
