package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/runs"
)

// requirePython skips the test when no Python interpreter is available.
func requirePython(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed; skipping process execution test")
	}
	return path
}

func testLogger() *logging.Logger {
	return logging.New(os.Stderr, logging.FormatJSON, logging.LevelError)
}

// collector is a LogSink that records every captured line.
type collector struct {
	mu    sync.Mutex
	lines []capturedLine
}

type capturedLine struct {
	Stream  string
	Message string
	At      time.Time
}

func (c *collector) Line(stream string, at time.Time, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, capturedLine{Stream: stream, Message: message, At: at})
}

func (c *collector) all() []capturedLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedLine, len(c.lines))
	copy(out, c.lines)
	return out
}

func (c *collector) messages(stream string) []string {
	var out []string
	for _, l := range c.all() {
		if l.Stream == stream {
			out = append(out, l.Message)
		}
	}
	return out
}

// writeScript creates an integration directory containing main.py.
func writeScript(t *testing.T, body string) (*config.Manifest, string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(body), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}

	m := &config.Manifest{
		Version:    config.SupportedVersion,
		Name:       "fixture",
		Entrypoint: "main.py",
		Dir:        dir,
	}
	m.ApplyDefaults()
	return m, dir
}

func TestRunCapturesStdoutStderrAndExitCode(t *testing.T) {
	requirePython(t)

	m, _ := writeScript(t, `
import sys
print("hello stdout")
print("hello stderr", file=sys.stderr)
sys.exit(0)
`)

	sink := &collector{}
	exec := New(testLogger(), "")

	res := exec.Run(context.Background(), &Request{
		Manifest: m,
		RunID:    "run-1",
		Timeout:  30 * time.Second,
	}, sink)

	if res.StartError != nil {
		t.Fatalf("unexpected start error: %v", res.StartError)
	}
	if !res.Succeeded() {
		t.Fatalf("expected success, got exit code %v (signal %q, timed out %v, cancelled %v)",
			res.ExitCode, res.Signal, res.TimedOut, res.Cancelled)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", res.ExitCode)
	}
	if res.StartedAt.IsZero() || res.FinishedAt.IsZero() {
		t.Fatal("expected start and finish timestamps to be recorded")
	}
	if res.FinishedAt.Before(res.StartedAt) {
		t.Fatal("finished_at must not precede started_at")
	}

	if got := sink.messages(runs.StreamStdout); len(got) != 1 || got[0] != "hello stdout" {
		t.Errorf("stdout capture = %v, want [hello stdout]", got)
	}
	if got := sink.messages(runs.StreamStderr); len(got) != 1 || got[0] != "hello stderr" {
		t.Errorf("stderr capture = %v, want [hello stderr]", got)
	}
}

func TestRunRecordsNonZeroExit(t *testing.T) {
	requirePython(t)

	m, _ := writeScript(t, `
import sys
print("failing")
sys.exit(3)
`)

	res := New(testLogger(), "").Run(context.Background(), &Request{
		Manifest: m,
		RunID:    "run-2",
		Timeout:  30 * time.Second,
	}, &collector{})

	if res.Succeeded() {
		t.Fatal("expected the run to be reported as failed")
	}
	if res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", res.ExitCode)
	}
	if res.TimedOut || res.Cancelled {
		t.Fatalf("a non-zero exit must not be reported as timeout/cancel: %+v", res)
	}
}

func TestRunTimesOutAndKillsProcess(t *testing.T) {
	requirePython(t)

	m, _ := writeScript(t, `
import time
print("starting to hang", flush=True)
time.sleep(300)
print("should never print")
`)

	start := time.Now()
	res := New(testLogger(), "").Run(context.Background(), &Request{
		Manifest:       m,
		RunID:          "run-3",
		Timeout:        1 * time.Second,
		TerminateGrace: 500 * time.Millisecond,
	}, &collector{})
	elapsed := time.Since(start)

	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	if res.Cancelled {
		t.Error("a timeout must not be reported as a cancellation")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("timeout took %s; the process was not killed promptly", elapsed)
	}
	// SIGTERM + grace means we should be well past the 1s timeout but long
	// before the 300s sleep would have ended.
	if elapsed < 900*time.Millisecond {
		t.Fatalf("returned after %s, before the 1s timeout elapsed", elapsed)
	}
}

func TestRunKillsProcessGroupSoChildrenDie(t *testing.T) {
	requirePython(t)

	// The child spawns a grandchild that would outlive a naive kill of only
	// the direct child. Both must be torn down.
	m, dir := writeScript(t, `
import subprocess, sys, time
subprocess.Popen([sys.executable, "-c", "import time; time.sleep(300)"])
print("parent hanging", flush=True)
time.sleep(300)
`)

	res := New(testLogger(), "").Run(context.Background(), &Request{
		Manifest:       m,
		RunID:          "run-group",
		Timeout:        1 * time.Second,
		TerminateGrace: 500 * time.Millisecond,
	}, &collector{})

	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	_ = dir
}

func TestRunCancellation(t *testing.T) {
	requirePython(t)

	m, _ := writeScript(t, `
import time
print("running", flush=True)
time.sleep(300)
`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	res := New(testLogger(), "").Run(ctx, &Request{
		Manifest:       m,
		RunID:          "run-4",
		Timeout:        60 * time.Second,
		TerminateGrace: 500 * time.Millisecond,
	}, &collector{})

	if !res.Cancelled {
		t.Fatalf("expected Cancelled, got %+v", res)
	}
	if res.TimedOut {
		t.Error("a cancellation must not be reported as a timeout")
	}
	if res.Succeeded() {
		t.Error("a cancelled run must not report success")
	}
}

func TestRunSetsOtterEnvironmentAndStripsDaemonSecrets(t *testing.T) {
	requirePython(t)

	// The daemon's own API token must never leak into integration code.
	t.Setenv("OTTER_API_TOKEN", "super-secret-daemon-token")
	t.Setenv("OUTER_VAR", "outer-value")

	m, _ := writeScript(t, `
import json, os
keys = ["OTTER_INTEGRATION_ID", "OTTER_RUN_ID", "OTTER_API_URL",
        "OTTER_TRIGGER_TYPE", "OTTER_INTEGRATION_DIR", "OTTER_STATE_TOKEN",
        "OTTER_API_TOKEN", "PYTHONUNBUFFERED", "PYTHONDONTWRITEBYTECODE",
        "MANIFEST_VAR", "SECRET_TOKEN", "EXPANDED", "OUTER_VAR", "PYTHONPATH"]
print(json.dumps({k: os.environ.get(k) for k in keys}))
`)

	m.Env = map[string]string{
		"MANIFEST_VAR": "manifest-value",
		"EXPANDED":     "prefix-${SECRET_TOKEN}",
	}

	sink := &collector{}
	res := New(testLogger(), "/tmp/fake-sdk").Run(context.Background(), &Request{
		Manifest:    m,
		RunID:       "run-5",
		TriggerType: runs.TriggerWebhook,
		APIURL:      "http://127.0.0.1:7337",
		StateToken:  "scoped-run-token",
		ExtraEnv:    map[string]string{"SECRET_TOKEN": "s3cr3t"},
		Timeout:     30 * time.Second,
	}, sink)
	if res.StartError != nil {
		t.Fatalf("start error: %v", res.StartError)
	}
	if !res.Succeeded() {
		t.Fatalf("run failed: %+v", res)
	}

	stdout := sink.messages(runs.StreamStdout)
	if len(stdout) != 1 {
		t.Fatalf("expected one stdout line, got %v", stdout)
	}

	var got map[string]*string
	if err := json.Unmarshal([]byte(stdout[0]), &got); err != nil {
		t.Fatalf("decode env dump %q: %v", stdout[0], err)
	}

	wantValues := map[string]string{
		"OTTER_INTEGRATION_ID":    "fixture",
		"OTTER_RUN_ID":            "run-5",
		"OTTER_API_URL":           "http://127.0.0.1:7337",
		"OTTER_TRIGGER_TYPE":      "webhook",
		"OTTER_STATE_TOKEN":       "scoped-run-token",
		"PYTHONUNBUFFERED":        "1",
		"PYTHONDONTWRITEBYTECODE": "1",
		"MANIFEST_VAR":            "manifest-value",
		"SECRET_TOKEN":            "s3cr3t",
		"EXPANDED":                "prefix-s3cr3t",
		"OUTER_VAR":               "outer-value",
	}
	for key, want := range wantValues {
		if got[key] == nil {
			t.Errorf("%s is not set in the child environment", key)
			continue
		}
		if *got[key] != want {
			t.Errorf("%s = %q, want %q", key, *got[key], want)
		}
	}

	if got["OTTER_INTEGRATION_DIR"] == nil || *got["OTTER_INTEGRATION_DIR"] != m.Dir {
		t.Errorf("OTTER_INTEGRATION_DIR = %v, want %q", got["OTTER_INTEGRATION_DIR"], m.Dir)
	}

	if got["OTTER_API_TOKEN"] != nil {
		t.Errorf("the daemon API token leaked into the child environment: %q", *got["OTTER_API_TOKEN"])
	}

	if got["PYTHONPATH"] == nil || !strings.HasPrefix(*got["PYTHONPATH"], "/tmp/fake-sdk") {
		t.Errorf("PYTHONPATH = %v, want it to start with the SDK path", got["PYTHONPATH"])
	}
}

func TestRunFailsWhenInterpreterMissing(t *testing.T) {
	m, _ := writeScript(t, "print('never')\n")
	m.Python.Executable = "definitely-not-a-real-interpreter-xyz"

	res := New(testLogger(), "").Run(context.Background(), &Request{
		Manifest: m,
		RunID:    "run-6",
		Timeout:  10 * time.Second,
	}, &collector{})

	if res.StartError == nil {
		t.Fatal("expected a StartError when the interpreter cannot be launched")
	}
	if res.Succeeded() {
		t.Error("a run that never started must not report success")
	}
}

func TestRunTruncatesVeryLongLines(t *testing.T) {
	requirePython(t)

	m, _ := writeScript(t, `
print("x" * (200 * 1024))
print("after")
`)

	sink := &collector{}
	res := New(testLogger(), "").Run(context.Background(), &Request{
		Manifest: m,
		RunID:    "run-7",
		Timeout:  30 * time.Second,
	}, sink)
	if !res.Succeeded() {
		t.Fatalf("run failed: %+v", res)
	}

	lines := sink.messages(runs.StreamStdout)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (the long one truncated), got %d", len(lines))
	}
	if !strings.HasSuffix(lines[0], "…(truncated)") {
		t.Errorf("long line was not marked as truncated: %.80q", lines[0])
	}
	if len(lines[0]) > maxLogLine+32 {
		t.Errorf("truncated line is still %d bytes", len(lines[0]))
	}
	if lines[1] != "after" {
		t.Errorf("output after a long line was lost: got %q", lines[1])
	}
}

func TestRunWithoutSinkDiscardsOutput(t *testing.T) {
	requirePython(t)

	m, _ := writeScript(t, `print("discarded")`)
	res := New(testLogger(), "").Run(context.Background(), &Request{
		Manifest: m,
		RunID:    "run-8",
		Timeout:  30 * time.Second,
	}, nil)

	if !res.Succeeded() {
		t.Fatalf("run failed: %+v", res)
	}
}

func TestRunRejectsNilManifest(t *testing.T) {
	res := New(testLogger(), "").Run(context.Background(), &Request{RunID: "x"}, &collector{})
	if res.StartError == nil {
		t.Fatal("expected an error for a request without a manifest")
	}
}

func TestBuildEnvIgnoresNonStringKeys(t *testing.T) {
	m, _ := writeScript(t, "pass\n")
	exec := New(testLogger(), "")

	env, err := exec.buildEnv(&Request{Manifest: m, RunID: "r", TriggerType: "manual"})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}

	seen := map[string]string{}
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		seen[key] = value
	}
	if seen["OTTER_RUN_ID"] != "r" {
		t.Errorf("OTTER_RUN_ID = %q, want r", seen["OTTER_RUN_ID"])
	}
	if seen["OTTER_TRIGGER_TYPE"] != "manual" {
		t.Errorf("OTTER_TRIGGER_TYPE = %q, want manual", seen["OTTER_TRIGGER_TYPE"])
	}
}
