package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// writeManifest writes body to dir/otter.yaml and returns the manifest path.
func writeManifest(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, ManifestFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest %s: %v", path, err)
	}
	return path
}

// touch creates a regular file, creating parent directories as needed.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "main.py"))
	path := writeManifest(t, dir, "version: 1\nname: example\nentrypoint: main.py\n")

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if m.Timeout.Duration() != DefaultTimeout {
		t.Errorf("Timeout = %s, want %s", m.Timeout, DefaultTimeout)
	}
	if m.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", m.Concurrency, DefaultConcurrency)
	}
	if m.Retry.Attempts != DefaultRetryAttempts {
		t.Errorf("Retry.Attempts = %d, want %d", m.Retry.Attempts, DefaultRetryAttempts)
	}
	if m.Retry.Backoff != DefaultBackoff {
		t.Errorf("Retry.Backoff = %q, want %q", m.Retry.Backoff, DefaultBackoff)
	}
	if m.Retry.InitialDelay.Duration() != DefaultRetryInitialDelay {
		t.Errorf("Retry.InitialDelay = %s, want %s", m.Retry.InitialDelay, DefaultRetryInitialDelay)
	}
	if m.Retry.MaxDelay.Duration() != DefaultRetryMaxDelay {
		t.Errorf("Retry.MaxDelay = %s, want %s", m.Retry.MaxDelay, DefaultRetryMaxDelay)
	}
	if m.Python.Executable != DefaultPythonExecutable {
		t.Errorf("Python.Executable = %q, want %q", m.Python.Executable, DefaultPythonExecutable)
	}
	if got := m.MaxAttempts(); got != 1 {
		t.Errorf("MaxAttempts() = %d, want 1", got)
	}
	if m.RetriesEnabled() {
		t.Errorf("RetriesEnabled() = true, want false")
	}

	// A minimal manifest is valid once its entrypoint exists.
	if err := m.Validate(); err != nil {
		t.Errorf("Validate() on minimal manifest = %v, want nil", err)
	}
}

func TestManagedPythonRequiresExplicitModeAndInputs(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "main.py"))
	for name, value := range map[string]string{
		".python-version": "3.13.5\n",
		"pyproject.toml":  "[project]\nname='fixture'\nversion='0.1.0'\n",
		"uv.lock":         "version = 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := writeManifest(t, dir, "version: 1\nname: fixture\nentrypoint: main.py\npython:\n  mode: managed\n")
	m, err := LoadAndValidate(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Python.Executable != "" {
		t.Fatalf("managed executable = %q", m.Python.Executable)
	}
	path = writeManifest(t, dir, "version: 1\nname: fixture\nentrypoint: main.py\npython:\n  mode: managed\n  executable: python3\n")
	if _, err := LoadAndValidate(path); err == nil || !strings.Contains(err.Error(), "cannot be set") {
		t.Fatalf("conflicting executable: %v", err)
	}
	path = writeManifest(t, dir, "version: 1\nname: fixture\nentrypoint: main.py\n")
	m, err = LoadAndValidate(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Python.Mode != "external" || m.Python.Executable != "python3" {
		t.Fatalf("legacy mode changed: %+v", m.Python)
	}
}

// assertAcceptedFormsMentioned checks the documented hint is present in an
// invalid-duration error.
func assertAcceptedFormsMentioned(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	for _, want := range []string{"number of seconds", "duration string"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDurationUnmarshal(t *testing.T) {
	type wrapper struct {
		D Duration `yaml:"d"`
	}

	valid := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"bare integer seconds", "300", 300 * time.Second},
		{"float seconds", "1.5", 1500 * time.Millisecond},
		{"minutes", "5m", 5 * time.Minute},
		{"milliseconds", "500ms", 500 * time.Millisecond},
		{"hours", "1h", time.Hour},
		{"compound", "1h30m", 90 * time.Minute},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			var w wrapper
			if err := yaml.Unmarshal([]byte("d: "+tc.in+"\n"), &w); err != nil {
				t.Fatalf("yaml.Unmarshal(%q) error = %v", tc.in, err)
			}
			if got := w.D.Duration(); got != tc.want {
				t.Errorf("Duration = %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("garbage rejected via yaml.Unmarshal", func(t *testing.T) {
		var w wrapper
		err := yaml.Unmarshal([]byte("d: soon\n"), &w)
		assertAcceptedFormsMentioned(t, err)
	})
}

func TestDurationManifestField(t *testing.T) {
	valid := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"bare integer", "300", 300 * time.Second},
		{"float", "1.5", 1500 * time.Millisecond},
		{"minutes", "5m", 5 * time.Minute},
		{"milliseconds", "500ms", 500 * time.Millisecond},
		{"hours", "1h", time.Hour},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			touch(t, filepath.Join(dir, "main.py"))
			path := writeManifest(t, dir, fmt.Sprintf(
				"version: 1\nname: example\nentrypoint: main.py\ntimeout: %s\n", tc.in))

			m, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got := m.Timeout.Duration(); got != tc.want {
				t.Errorf("Timeout = %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("garbage rejected via manifest", func(t *testing.T) {
		dir := t.TempDir()
		path := writeManifest(t, dir, "version: 1\nname: example\nentrypoint: main.py\ntimeout: soon\n")
		_, err := Load(path)
		assertAcceptedFormsMentioned(t, err)
	})
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{
			name:      "triggers instead of trigger",
			body:      "version: 1\nname: example\nentrypoint: main.py\ntriggers:\n  cron: \"*/5 * * * *\"\n",
			wantField: "triggers",
		},
		{
			name:      "retires instead of retry",
			body:      "version: 1\nname: example\nentrypoint: main.py\nretires:\n  attempts: 3\n",
			wantField: "retires",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeManifest(t, dir, tc.body)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load() = nil error, want unknown-field failure")
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not mention %q", err, tc.wantField)
			}
		})
	}
}

// validManifest builds an in-memory manifest that passes Validate when dir
// contains an entrypoint named main.py.
func validManifest(dir string) *Manifest {
	return &Manifest{
		Version:     SupportedVersion,
		Name:        "example",
		Entrypoint:  "main.py",
		Timeout:     Duration(300 * time.Second),
		Concurrency: 1,
		Retry: RetryConfig{
			Backoff:      BackoffExponential,
			InitialDelay: Duration(2 * time.Second),
			MaxDelay:     Duration(60 * time.Second),
		},
		Dir:  dir,
		Path: filepath.Join(dir, ManifestFileName),
	}
}

// TestValidateRejects exercises Validate directly. Several of these values
// (negative timeout, zero concurrency, negative retry.attempts) are coerced to
// defaults by ApplyDefaults inside Load, so they can only be observed by
// calling Validate on a manifest that has not been defaulted.
func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		want   []string
		setup  func(t *testing.T, dir string)
		mutate func(m *Manifest)
	}{
		{
			name:   "wrong version",
			want:   []string{"version"},
			mutate: func(m *Manifest) { m.Version = 2 },
		},
		{
			name:   "missing name",
			want:   []string{"name"},
			mutate: func(m *Manifest) { m.Name = "" },
		},
		{
			name:   "blank name",
			want:   []string{"name"},
			mutate: func(m *Manifest) { m.Name = "   " },
		},
		{
			name:   "uppercase name",
			want:   []string{"name"},
			mutate: func(m *Manifest) { m.Name = "Bad Name" },
		},
		{
			name:   "leading dash name",
			want:   []string{"name"},
			mutate: func(m *Manifest) { m.Name = "-leading" },
		},
		{
			name:   "missing entrypoint",
			want:   []string{"entrypoint"},
			mutate: func(m *Manifest) { m.Entrypoint = "" },
		},
		{
			name:   "absolute entrypoint",
			want:   []string{"entrypoint", "relative"},
			mutate: func(m *Manifest) { m.Entrypoint = filepath.Join(m.Dir, "main.py") },
		},
		{
			name:   "escaping entrypoint",
			want:   []string{"entrypoint", "escape"},
			mutate: func(m *Manifest) { m.Entrypoint = "../outside.py" },
		},
		{
			name:   "missing entrypoint file",
			want:   []string{"entrypoint", "not found"},
			mutate: func(m *Manifest) { m.Entrypoint = "missing.py" },
		},
		{
			name: "entrypoint is a directory",
			want: []string{"entrypoint", "directory"},
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
					t.Fatalf("mkdir sub: %v", err)
				}
			},
			mutate: func(m *Manifest) { m.Entrypoint = "sub" },
		},
		{
			name:   "negative timeout",
			want:   []string{"timeout"},
			mutate: func(m *Manifest) { m.Timeout = Duration(-1 * time.Second) },
		},
		{
			name:   "zero concurrency",
			want:   []string{"concurrency"},
			mutate: func(m *Manifest) { m.Concurrency = 0 },
		},
		{
			name:   "negative retry attempts",
			want:   []string{"retry.attempts"},
			mutate: func(m *Manifest) { m.Retry.Attempts = -1 },
		},
		{
			name:   "too many retry attempts",
			want:   []string{"retry.attempts"},
			mutate: func(m *Manifest) { m.Retry.Attempts = MaxRetryAttempts + 1 },
		},
		{
			name:   "unknown backoff",
			want:   []string{"retry.backoff"},
			mutate: func(m *Manifest) { m.Retry.Backoff = "random" },
		},
		{
			name: "max delay below initial delay",
			want: []string{"retry.max_delay"},
			mutate: func(m *Manifest) {
				m.Retry.InitialDelay = Duration(10 * time.Second)
				m.Retry.MaxDelay = Duration(1 * time.Second)
			},
		},
		{
			name:   "invalid env key",
			want:   []string{"env", "1BAD"},
			mutate: func(m *Manifest) { m.Env = map[string]string{"1BAD": "x"} },
		},
		{
			name:   "invalid secret name",
			want:   []string{"secrets", "1BAD"},
			mutate: func(m *Manifest) { m.Secrets = []string{"1BAD"} },
		},
		{
			name:   "duplicate secret",
			want:   []string{"more than once", "API_KEY"},
			mutate: func(m *Manifest) { m.Secrets = []string{"API_KEY", "API_KEY"} },
		},
		{
			name:   "cron with too few fields",
			want:   []string{"cron"},
			mutate: func(m *Manifest) { m.Trigger.Cron = "* * * *" },
		},
		{
			name:   "cron garbage",
			want:   []string{"cron"},
			mutate: func(m *Manifest) { m.Trigger.Cron = "nope" },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			touch(t, filepath.Join(dir, "main.py"))
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			m := validManifest(dir)
			tc.mutate(m)

			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate() error is %T, want *ValidationError", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestValidationErrorAggregatesProblems(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "main.py"))
	m := validManifest(dir)
	m.Version = 7
	m.Name = ""
	m.Concurrency = 0

	err := m.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate() error is %T, want *ValidationError", err)
	}
	if len(ve.Errors) != 3 {
		t.Fatalf("ValidationError.Errors = %d entries, want 3: %v", len(ve.Errors), ve.Errors)
	}
	msg := err.Error()
	if !strings.Contains(msg, "3 problems") {
		t.Errorf("error %q does not announce 3 problems", msg)
	}
	for _, want := range []string{"version", "name", "concurrency"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not list the %q problem", msg, want)
		}
	}
}

func TestValidManifestFields(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "main.py"))
	path := writeManifest(t, dir, `version: 1
name: salesforce-to-netsuite
description: sync orders
entrypoint: main.py
timeout: 42
concurrency: 3
python:
  executable: python3.11
trigger:
  cron: "*/5 * * * *"
  webhook:
    enabled: true
env:
  FOO: bar
secrets:
  - API_KEY
  - ERP_TOKEN
`)

	m, err := LoadAndValidate(path)
	if err != nil {
		t.Fatalf("LoadAndValidate() error = %v", err)
	}

	if !m.WebhookEnabled() {
		t.Errorf("WebhookEnabled() = false, want true")
	}
	if got := m.Cron(); got != "*/5 * * * *" {
		t.Errorf("Cron() = %q, want %q", got, "*/5 * * * *")
	}
	wantEntrypoint := filepath.Join(dir, "main.py")
	if got := m.EntrypointPath(); got != wantEntrypoint {
		t.Errorf("EntrypointPath() = %q, want %q", got, wantEntrypoint)
	}
	if got := m.TimeoutDuration(); got != 42*time.Second {
		t.Errorf("TimeoutDuration() = %s, want 42s", got)
	}
	if m.Concurrency != 3 {
		t.Errorf("Concurrency = %d, want 3", m.Concurrency)
	}
	if m.Python.Executable != "python3.11" {
		t.Errorf("Python.Executable = %q, want python3.11", m.Python.Executable)
	}
	if len(m.Secrets) != 2 || m.Secrets[0] != "API_KEY" || m.Secrets[1] != "ERP_TOKEN" {
		t.Errorf("Secrets = %v, want [API_KEY ERP_TOKEN]", m.Secrets)
	}
}

func TestValidateAcceptsCronDescriptors(t *testing.T) {
	for _, expr := range []string{"@daily", "@hourly", "@weekly", "@monthly", "@yearly", "@every 5m"} {
		t.Run(expr, func(t *testing.T) {
			dir := t.TempDir()
			touch(t, filepath.Join(dir, "main.py"))
			m := validManifest(dir)
			m.Trigger.Cron = expr
			if err := m.Validate(); err != nil {
				t.Errorf("Validate() with cron %q = %v, want nil", expr, err)
			}
			if got := m.Cron(); got != expr {
				t.Errorf("Cron() = %q, want %q", got, expr)
			}
		})
	}
}

func TestLoadMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", ManifestFileName)
	if _, err := Load(path); err == nil {
		t.Fatalf("Load(%q) = nil error, want failure", path)
	}
}

func TestLoadSetsPathAndDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "integrations", "my-int")
	touch(t, filepath.Join(dir, "main.py"))
	path := writeManifest(t, dir, "version: 1\nname: my-int\nentrypoint: main.py\n")

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantPath, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", path, err)
	}
	if m.Path != wantPath {
		t.Errorf("Path = %q, want %q", m.Path, wantPath)
	}
	if want := filepath.Dir(wantPath); m.Dir != want {
		t.Errorf("Dir = %q, want %q", m.Dir, want)
	}
	if want := filepath.Join(dir, "main.py"); m.EntrypointPath() != want {
		t.Errorf("EntrypointPath() = %q, want %q", m.EntrypointPath(), want)
	}
}

func TestMaxAttemptsSemantics(t *testing.T) {
	tests := []struct {
		attempts    int
		wantMax     int
		wantRetries bool
	}{
		{0, 1, false},
		{1, 1, false},
		{2, 2, true},
		{5, 5, true},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("attempts_%d", tc.attempts), func(t *testing.T) {
			m := &Manifest{Retry: RetryConfig{Attempts: tc.attempts}}
			if got := m.MaxAttempts(); got != tc.wantMax {
				t.Errorf("MaxAttempts() = %d, want %d", got, tc.wantMax)
			}
			if got := m.RetriesEnabled(); got != tc.wantRetries {
				t.Errorf("RetriesEnabled() = %v, want %v", got, tc.wantRetries)
			}
		})
	}
}
