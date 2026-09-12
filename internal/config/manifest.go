// Package config loads, validates and discovers Otter integration manifests.
//
// A manifest is a plain YAML file named otter.yaml. Otter deliberately keeps
// the manifest small: it describes how to run an integration process and when
// to trigger it. Integration logic belongs in Python, never in YAML.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// ManifestFileName is the file Otter looks for when discovering integrations.
const ManifestFileName = "otter.yaml"

// SupportedVersion is the only manifest schema version understood today.
const SupportedVersion = 1

// Default values applied when a manifest omits a field.
const (
	DefaultTimeout                  = 300 * time.Second
	DefaultConcurrency              = 1
	DefaultRetryAttempts            = 0
	DefaultRetryInitialDelay        = 2 * time.Second
	DefaultRetryMaxDelay            = 60 * time.Second
	DefaultBackoff                  = BackoffExponential
	DefaultPythonExecutable         = "python3"
	MaxRetryAttempts                = 100
	MaxNameLength                   = 64
	maxTimeout                      = 30 * 24 * time.Hour
	BackoffNone              string = "none"
	BackoffLinear            string = "linear"
	BackoffExponential       string = "exponential"
)

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	envNameRegexp = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Manifest is a parsed otter.yaml.
type Manifest struct {
	Version     int               `yaml:"version"`
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Entrypoint  string            `yaml:"entrypoint"`
	Python      PythonConfig      `yaml:"python"`
	Trigger     TriggerConfig     `yaml:"trigger"`
	Timeout     Duration          `yaml:"timeout"`
	Concurrency int               `yaml:"concurrency"`
	Retry       RetryConfig       `yaml:"retry"`
	Env         map[string]string `yaml:"env"`
	Secrets     []string          `yaml:"secrets"`

	// Dir is the integration directory (the directory containing otter.yaml)
	// and Path is the manifest path. Both are derived, not authored.
	Dir  string `yaml:"-"`
	Path string `yaml:"-"`
}

// PythonConfig describes how to launch the integration process.
type PythonConfig struct {
	Executable string `yaml:"executable"`

	// Path lists directories prepended to the child's PYTHONPATH, so
	// integrations can share client code instead of copying it. Entries are
	// resolved relative to the integration directory, and ".." is allowed
	// because shared code normally lives outside it.
	Path []string `yaml:"path"`
}

// PythonPaths resolves python.path entries to absolute directories.
func (m *Manifest) PythonPaths() []string {
	out := make([]string, 0, len(m.Python.Path))
	for _, entry := range m.Python.Path {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if filepath.IsAbs(entry) {
			out = append(out, filepath.Clean(entry))
			continue
		}
		out = append(out, filepath.Join(m.Dir, filepath.FromSlash(entry)))
	}
	return out
}

// TriggerConfig describes what starts a run. Triggers are optional: manual
// runs through the CLI or API always work.
type TriggerConfig struct {
	Cron    string         `yaml:"cron"`
	Webhook *WebhookConfig `yaml:"webhook"`
}

// WebhookConfig enables the POST /v1/hooks/{integration} endpoint.
type WebhookConfig struct {
	Enabled bool `yaml:"enabled"`
}

// RetryConfig describes the retry policy for a failed run.
type RetryConfig struct {
	Attempts     int      `yaml:"attempts"`
	Backoff      string   `yaml:"backoff"`
	InitialDelay Duration `yaml:"initial_delay"`
	MaxDelay     Duration `yaml:"max_delay"`
}

// WebhookEnabled reports whether the webhook trigger is turned on.
func (m *Manifest) WebhookEnabled() bool {
	return m.Trigger.Webhook != nil && m.Trigger.Webhook.Enabled
}

// Cron returns the cron expression, if any.
func (m *Manifest) Cron() string { return strings.TrimSpace(m.Trigger.Cron) }

// EntrypointPath is the absolute path of the Python entrypoint.
func (m *Manifest) EntrypointPath() string {
	return filepath.Join(m.Dir, filepath.FromSlash(m.Entrypoint))
}

// MaxAttempts is the total number of attempts allowed, including the first.
// `retry.attempts: 0` therefore means "run once, never retry".
func (m *Manifest) MaxAttempts() int {
	if m.Retry.Attempts < 1 {
		return 1
	}
	return m.Retry.Attempts
}

// RetriesEnabled reports whether a failed attempt should be retried.
func (m *Manifest) RetriesEnabled() bool { return m.MaxAttempts() > 1 }

// TimeoutDuration is the enforced execution timeout.
func (m *Manifest) TimeoutDuration() time.Duration { return m.Timeout.Duration() }

// rawManifest mirrors Manifest but makes every optional scalar a pointer, so
// that a field which is absent can be told apart from one that is present with
// an invalid value such as `concurrency: 0`. Without that distinction,
// defaults would silently paper over manifest mistakes and the validator would
// never see them.
type rawManifest struct {
	Version     int               `yaml:"version"`
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Entrypoint  string            `yaml:"entrypoint"`
	Python      PythonConfig      `yaml:"python"`
	Trigger     TriggerConfig     `yaml:"trigger"`
	Timeout     *Duration         `yaml:"timeout"`
	Concurrency *int              `yaml:"concurrency"`
	Retry       rawRetryConfig    `yaml:"retry"`
	Env         map[string]string `yaml:"env"`
	Secrets     []string          `yaml:"secrets"`
}

// rawRetryConfig is the presence-aware form of RetryConfig.
type rawRetryConfig struct {
	Attempts     *int      `yaml:"attempts"`
	Backoff      string    `yaml:"backoff"`
	InitialDelay *Duration `yaml:"initial_delay"`
	MaxDelay     *Duration `yaml:"max_delay"`
}

// Load reads and decodes a manifest without validating it. Unknown fields are
// rejected so typos such as `triggers:` or `retires:` fail loudly instead of
// being silently ignored.
//
// Defaults are applied only to fields the manifest omits: an explicitly
// invalid value is left in place so Validate reports it.
func Load(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var raw rawManifest
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read manifest %s: %w", path, err)
		}
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}

	m := Manifest{
		Version:     raw.Version,
		Name:        raw.Name,
		Description: raw.Description,
		Entrypoint:  raw.Entrypoint,
		Python:      raw.Python,
		Trigger:     raw.Trigger,
		Env:         raw.Env,
		Secrets:     raw.Secrets,
		Retry: RetryConfig{
			Backoff: raw.Retry.Backoff,
		},
	}
	if raw.Timeout != nil {
		m.Timeout = *raw.Timeout
	}
	if raw.Concurrency != nil {
		m.Concurrency = *raw.Concurrency
	}
	if raw.Retry.Attempts != nil {
		m.Retry.Attempts = *raw.Retry.Attempts
	}
	if raw.Retry.InitialDelay != nil {
		m.Retry.InitialDelay = *raw.Retry.InitialDelay
	}
	if raw.Retry.MaxDelay != nil {
		m.Retry.MaxDelay = *raw.Retry.MaxDelay
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	m.Path = abs
	m.Dir = filepath.Dir(abs)

	m.applyDefaultsFor(raw)
	return &m, nil
}

// LoadAndValidate reads a manifest and validates it.
func LoadAndValidate(path string) (*Manifest, error) {
	m, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// ApplyDefaults fills in every optional field with its documented default.
//
// It is intended for manifests built in code, where there is no way to tell an
// omitted field from an explicit zero. Manifests loaded from disk go through
// applyDefaultsFor instead, which respects field presence.
func (m *Manifest) ApplyDefaults() {
	if strings.TrimSpace(m.Python.Executable) == "" {
		m.Python.Executable = DefaultPythonExecutable
	}
	if m.Timeout <= 0 {
		m.Timeout = Duration(DefaultTimeout)
	}
	if m.Concurrency <= 0 {
		m.Concurrency = DefaultConcurrency
	}
	if m.Retry.Attempts < 0 {
		m.Retry.Attempts = DefaultRetryAttempts
	}
	if strings.TrimSpace(m.Retry.Backoff) == "" {
		m.Retry.Backoff = DefaultBackoff
	}
	m.Retry.Backoff = strings.ToLower(strings.TrimSpace(m.Retry.Backoff))
	if m.Retry.InitialDelay <= 0 {
		m.Retry.InitialDelay = Duration(DefaultRetryInitialDelay)
	}
	if m.Retry.MaxDelay <= 0 {
		m.Retry.MaxDelay = Duration(DefaultRetryMaxDelay)
	}
}

// applyDefaultsFor fills in only the fields the manifest omitted, leaving
// explicitly supplied values (including invalid ones) untouched so that
// Validate can reject them.
func (m *Manifest) applyDefaultsFor(raw rawManifest) {
	// Free-form fields have no meaningful "explicitly empty" state.
	if strings.TrimSpace(m.Python.Executable) == "" {
		m.Python.Executable = DefaultPythonExecutable
	}
	if strings.TrimSpace(m.Retry.Backoff) == "" {
		m.Retry.Backoff = DefaultBackoff
	}
	m.Retry.Backoff = strings.ToLower(strings.TrimSpace(m.Retry.Backoff))

	if raw.Timeout == nil {
		m.Timeout = Duration(DefaultTimeout)
	}
	if raw.Concurrency == nil {
		m.Concurrency = DefaultConcurrency
	}
	if raw.Retry.Attempts == nil {
		m.Retry.Attempts = DefaultRetryAttempts
	}
	if raw.Retry.InitialDelay == nil {
		m.Retry.InitialDelay = Duration(DefaultRetryInitialDelay)
	}
	if raw.Retry.MaxDelay == nil {
		m.Retry.MaxDelay = Duration(DefaultRetryMaxDelay)
	}
}

// ValidationError aggregates every problem found in a manifest so that a user
// sees all of them at once instead of fixing them one at a time.
type ValidationError struct {
	Path   string
	Errors []string
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 1 {
		return fmt.Sprintf("%s: %s", e.Path, e.Errors[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d problems:", e.Path, len(e.Errors))
	for _, msg := range e.Errors {
		b.WriteString("\n  - ")
		b.WriteString(msg)
	}
	return b.String()
}

// Validate checks the manifest against the documented rules. It also verifies
// that the entrypoint exists on disk.
func (m *Manifest) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if m.Version != SupportedVersion {
		add("version must be %d, got %v", SupportedVersion, m.Version)
	}

	if strings.TrimSpace(m.Name) == "" {
		add("name is required")
	} else if len(m.Name) > MaxNameLength {
		add("name must be at most %d characters, got %d", MaxNameLength, len(m.Name))
	} else if !namePattern.MatchString(m.Name) {
		add("name %q is invalid: use lowercase letters, digits, dot, dash or underscore (for example salesforce-to-netsuite)", m.Name)
	}

	if strings.TrimSpace(m.Entrypoint) == "" {
		add("entrypoint is required")
	} else if filepath.IsAbs(m.Entrypoint) {
		add("entrypoint %q must be a relative path inside the integration directory", m.Entrypoint)
	} else {
		cleaned := filepath.Clean(filepath.FromSlash(m.Entrypoint))
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			add("entrypoint %q must not escape the integration directory", m.Entrypoint)
		} else if m.Dir != "" {
			info, err := os.Stat(filepath.Join(m.Dir, cleaned))
			switch {
			case err != nil:
				add("entrypoint %q not found in %s", m.Entrypoint, m.Dir)
			case info.IsDir():
				add("entrypoint %q is a directory, expected a file", m.Entrypoint)
			}
		}
	}

	if m.Timeout <= 0 {
		add("timeout must be greater than zero")
	} else if m.Timeout.Duration() > maxTimeout {
		add("timeout must be at most %s", maxTimeout)
	}

	if m.Concurrency < 1 {
		add("concurrency must be at least 1, got %d", m.Concurrency)
	}

	if m.Retry.Attempts < 0 {
		add("retry.attempts must not be negative, got %d", m.Retry.Attempts)
	} else if m.Retry.Attempts > MaxRetryAttempts {
		add("retry.attempts must be at most %d, got %d", MaxRetryAttempts, m.Retry.Attempts)
	}

	switch m.Retry.Backoff {
	case BackoffNone, BackoffLinear, BackoffExponential:
	default:
		add("retry.backoff must be one of %q, %q or %q, got %q",
			BackoffNone, BackoffLinear, BackoffExponential, m.Retry.Backoff)
	}

	if m.Retry.InitialDelay < 0 {
		add("retry.initial_delay must not be negative")
	}
	if m.Retry.MaxDelay < 0 {
		add("retry.max_delay must not be negative")
	}
	if m.Retry.InitialDelay > 0 && m.Retry.MaxDelay > 0 && m.Retry.MaxDelay < m.Retry.InitialDelay {
		add("retry.max_delay (%s) must be greater than or equal to retry.initial_delay (%s)",
			m.Retry.MaxDelay, m.Retry.InitialDelay)
	}

	if m.Cron() != "" {
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
		if _, err := parser.Parse(m.Cron()); err != nil {
			add("trigger.cron %q is not a valid cron expression: %v", m.Cron(), err)
		}
	}

	envKeys := make([]string, 0, len(m.Env))
	for k := range m.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		if !envNameRegexp.MatchString(k) {
			add("env key %q is not a valid environment variable name", k)
		}
	}

	for i, entry := range m.Python.Path {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			add("python.path[%d] is empty", i)
			continue
		}
		if m.Dir == "" {
			continue
		}
		resolved := trimmed
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(m.Dir, filepath.FromSlash(trimmed))
		}
		info, err := os.Stat(resolved)
		switch {
		case err != nil:
			add("python.path[%d] %q does not exist (%s)", i, trimmed, resolved)
		case !info.IsDir():
			add("python.path[%d] %q is not a directory", i, trimmed)
		}
	}

	seenSecrets := map[string]bool{}
	for i, s := range m.Secrets {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			add("secrets[%d] is empty", i)
			continue
		}
		if !envNameRegexp.MatchString(trimmed) {
			add("secrets[%d] %q is not a valid environment variable name", i, trimmed)
		}
		if seenSecrets[trimmed] {
			add("secrets[%d] %q is listed more than once", i, trimmed)
		}
		seenSecrets[trimmed] = true
	}

	if len(problems) > 0 {
		return &ValidationError{Path: m.Path, Errors: problems}
	}
	return nil
}
