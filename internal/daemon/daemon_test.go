package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/notify"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/secrets"
)

// ---------------------------------------------------------------- test setup

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; skipping daemon integration test")
	}
}

func testLogger() *logging.Logger {
	return logging.New(io.Discard, logging.FormatJSON, logging.LevelError)
}

// writeIntegration lays a manifest and entrypoint into root/<dir>.
func writeIntegration(t *testing.T, root, dir, manifestYAML, python string) string {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, config.ManifestFileName), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "main.py"), []byte(python), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	return path
}

// freeAddr reserves an ephemeral loopback port for the test API server.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// releaseAll stages and activates a release for every valid integration under
// root. A run executes the active release rather than the source tree, so a
// workspace with no releases refuses every submission -- including the ones a
// test is about to make.
//
// Releases are keyed by the durable identity, which the registry assigns, so
// this helper performs the same observe-and-reconcile pass the daemon will:
// the identities it mints are the ones the daemon then reuses.
func releaseAll(t *testing.T, root, dataDir string) {
	t.Helper()

	ctx := context.Background()
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open %s: %v", dataDir, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close %s: %v", dataDir, err)
		}
	}()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate %s: %v", dataDir, err)
	}

	store := identity.NewStore(db.DB)
	svc := identity.NewService(store, root)
	scan, err := config.Observe(root, nil)
	if err != nil {
		t.Fatalf("observe %s: %v", root, err)
	}
	if _, err := svc.Recover(ctx); err != nil {
		t.Fatalf("identity recover: %v", err)
	}
	if _, err := svc.Reconcile(ctx, scan); err != nil {
		t.Fatalf("identity reconcile: %v", err)
	}

	// Identity paths are canonical, so the release base must be too: on a
	// platform where the temporary directory is reached through a symlink,
	// mixing the two spellings has no common ancestor.
	canonicalRoot, err := identity.Canonical(root)
	if err != nil {
		t.Fatalf("canonical %s: %v", root, err)
	}

	instances, err := store.Instances(ctx)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	manager := release.Manager{DataDir: dataDir}
	for _, inst := range instances {
		if inst.Status != identity.StatusActive {
			continue
		}
		manifestPath := filepath.Join(inst.CanonicalPath, config.ManifestFileName)
		if _, err := config.LoadAndValidate(manifestPath); err != nil {
			continue // the daemon reports invalid manifests itself
		}
		layout, err := release.Plan(canonicalRoot, inst.CanonicalPath, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", inst.ID, err)
		}
		meta, err := manager.StageWithLayout(inst.ID.String(), inst.CanonicalPath, layout, "")
		if err != nil {
			t.Fatalf("stage %s: %v", inst.ID, err)
		}
		if err := manager.Activate(inst.ID.String(), meta.Digest); err != nil {
			t.Fatalf("activate %s: %v", inst.ID, err)
		}
	}
}

// runtimeID resolves the durable identity a test knows by label. Tests assert
// on identity, never on the label, because the label is not a key.
func runtimeID(t *testing.T, d *Daemon, label string) string {
	t.Helper()
	entry, err := d.resolveRef(label)
	if err != nil {
		t.Fatalf("resolve %q: %v", label, err)
	}
	return entry.Integration.ID
}

// identityIDFor runs the same observe-and-reconcile pass the daemon will, so a
// test can learn an integration's durable identity before the daemon exists --
// for example to seed a run record or stage a release the way production keys
// them.
func identityIDFor(t *testing.T, root, dataDir, label string) string {
	t.Helper()

	ctx := context.Background()
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open %s: %v", dataDir, err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close %s: %v", dataDir, err)
		}
	}()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate %s: %v", dataDir, err)
	}

	store := identity.NewStore(db.DB)
	svc := identity.NewService(store, root)
	scan, err := config.Observe(root, nil)
	if err != nil {
		t.Fatalf("observe %s: %v", root, err)
	}
	if _, err := svc.Recover(ctx); err != nil {
		t.Fatalf("identity recover: %v", err)
	}
	if _, err := svc.Reconcile(ctx, scan); err != nil {
		t.Fatalf("identity reconcile: %v", err)
	}
	instances, err := store.Instances(ctx)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, inst := range instances {
		if inst.Status == identity.StatusActive && inst.Name == label {
			return inst.ID.String()
		}
	}
	t.Fatalf("no active identity labeled %q", label)
	return ""
}

// newDaemon builds a daemon over root, releasing every integration the test
// wrote before this point so a submission can bind to a snapshot the way
// production requires. An empty dataDir gets a fresh temp dir.
func newDaemon(t *testing.T, root, dataDir string, provider secrets.Provider, tweak func(*config.DaemonConfig)) *Daemon {
	return newDaemonWith(t, root, dataDir, provider, tweak, true)
}

// newDaemonUnreleased builds the same daemon with nothing released, which is
// the state of a workspace before `otter release` has ever run.
func newDaemonUnreleased(t *testing.T, root, dataDir string, provider secrets.Provider, tweak func(*config.DaemonConfig)) *Daemon {
	return newDaemonWith(t, root, dataDir, provider, tweak, false)
}

func newDaemonWith(t *testing.T, root, dataDir string, provider secrets.Provider, tweak func(*config.DaemonConfig), releaseIntegrations bool) *Daemon {
	t.Helper()
	requirePython(t)

	if dataDir == "" {
		dataDir = t.TempDir()
	}

	cfg := config.DefaultDaemonConfig("test")
	cfg.IntegrationsDir = root
	cfg.DataDir = dataDir
	cfg.Listen = freeAddr(t)
	cfg.Workers = 4
	cfg.ShutdownGrace = 5 * time.Second
	cfg.LogLevel = "error"
	if tweak != nil {
		tweak(&cfg)
	}

	if releaseIntegrations {
		releaseAll(t, root, dataDir)
	}

	d, err := New(context.Background(), Options{
		Config:  cfg,
		Logger:  testLogger(),
		Secrets: provider,
		Version: "test",
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return d
}

// startDaemon runs the daemon and waits until its API accepts requests.
func startDaemon(t *testing.T, d *Daemon) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Error("daemon did not shut down within 60s")
		}
	})

	client := api.NewClient("http://"+d.cfg.Listen, "")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_, err := client.Health(pingCtx)
		pingCancel()
		if err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon API did not become ready")
}

// awaitTerminal waits until the run's retry chain has settled on a terminal
// status. It double-checks after a short delay so it cannot return in the tiny
// window between an attempt failing and its retry being enqueued.
func awaitTerminal(t *testing.T, d *Daemon, runID string) *api.RunView {
	t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	var last *api.RunView

	for time.Now().Before(deadline) {
		view, err := d.GetRunDetail(context.Background(), runID)
		if err != nil {
			t.Fatalf("get run %s: %v", runID, err)
		}
		if view.LatestStatus.Terminal() {
			if last != nil && last.LatestStatus == view.LatestStatus && len(last.Attempts) == len(view.Attempts) {
				return view
			}
			last = view
			time.Sleep(200 * time.Millisecond)
			continue
		}
		last = nil
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatalf("run %s did not reach a terminal status in time (last: %+v)", runID, last)
	return nil
}

func waitForStatus(t *testing.T, d *Daemon, runID string, want runs.Status, timeout time.Duration) *runs.Run {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run, err := d.runs.Get(context.Background(), runID)
		if err != nil {
			t.Fatalf("get run %s: %v", runID, err)
		}
		if run.Status == want {
			return run
		}
		if run.Status.Terminal() && run.Status != want {
			t.Fatalf("run %s reached %s while waiting for %s", runID, run.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", runID, want)
	return nil
}

// awaitRunCount waits until at least n runs exist for an integration.
func awaitRunCount(t *testing.T, d *Daemon, integrationID string, n int, timeout time.Duration) []*runs.Run {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		list, err := d.runs.List(context.Background(), runs.Filter{IntegrationID: integrationID, Limit: 100})
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		if len(list) >= n {
			return list
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("integration %s did not accumulate %d runs in time", integrationID, n)
	return nil
}

// trackPeakConcurrency samples the running count until done() is true.
func trackPeakConcurrency(d *Daemon, integrationID string, done func() bool, timeout time.Duration) int {
	peak := 0
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n := d.cap.running(integrationID); n > peak {
			peak = n
		}
		if done() {
			return peak
		}
		time.Sleep(5 * time.Millisecond)
	}
	return peak
}

// readRunFromDisk opens the database directly, which is how a test inspects
// state after the daemon has shut down and closed its connection.
func readRunFromDisk(t *testing.T, dataDir, runID string) (*runs.Run, []*runs.Run) {
	t.Helper()

	db, err := database.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	store := runs.NewStore(db.DB)
	root, attempts, err := store.Chain(context.Background(), runID)
	if err != nil {
		t.Fatalf("read run %s from disk: %v", runID, err)
	}
	return root, attempts
}

func getState(t *testing.T, d *Daemon, integrationID, key string) json.RawMessage {
	t.Helper()
	value, err := d.GetState(context.Background(), integrationID, key)
	if err != nil {
		t.Fatalf("get state %s/%s: %v", integrationID, key, err)
	}
	return value
}

func logMessages(t *testing.T, d *Daemon, runID, stream string) []string {
	t.Helper()
	entries, err := d.RunLogs(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("get logs for %s: %v", runID, err)
	}
	var out []string
	for _, e := range entries {
		if e.Stream == stream {
			out = append(out, e.Message)
		}
	}
	return out
}

// ------------------------------------------------------------------- tests

func TestIntegrationExecutesPersistsStateAndLogs(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "counter", `
version: 1
name: counter
entrypoint: main.py
timeout: 30
retry:
  attempts: 0
`, `
from otter import Context

ctx = Context.from_environment()
count = (ctx.state.get("count") or 0) + 1
ctx.state.set("count", count)
ctx.state.set("last_run", {"run_id": ctx.run_id, "trigger": ctx.trigger.type})
ctx.log.info("incremented", count=count)
print("count=%d" % count)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "counter", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", view.Run.Status, view.Run.ErrorString())
	}
	if view.Run.ExitCode == nil || *view.Run.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", view.Run.ExitCode)
	}
	if view.Run.TriggerType != runs.TriggerManual {
		t.Errorf("trigger type = %q, want manual", view.Run.TriggerType)
	}
	if view.Run.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", view.Run.Attempt)
	}
	if len(view.Attempts) != 1 {
		t.Errorf("expected exactly one attempt with retries disabled, got %d", len(view.Attempts))
	}

	if got := string(getState(t, d, "counter", "count")); got != "1" {
		t.Errorf("state count = %s, want 1", got)
	}

	stdout := logMessages(t, d, runID, runs.StreamStdout)
	if len(stdout) == 0 || stdout[0] != "count=1" {
		t.Errorf("stdout logs = %v, want [count=1]", stdout)
	}

	otter := strings.Join(logMessages(t, d, runID, runs.StreamOtter), "\n")
	for _, want := range []string{"run queued", "run started", "incremented", "run succeeded"} {
		if !strings.Contains(otter, want) {
			t.Errorf("otter log stream is missing %q; got:\n%s", want, otter)
		}
	}

	// A second run must observe the state the first one persisted.
	secondID, err := d.SubmitRun(context.Background(), "counter", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit second run: %v", err)
	}
	awaitTerminal(t, d, secondID)

	if got := string(getState(t, d, "counter", "count")); got != "2" {
		t.Errorf("state count after second run = %s, want 2", got)
	}
}

func TestRetryPolicyRetriesUntilSuccess(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "flaky", `
version: 1
name: flaky
entrypoint: main.py
timeout: 30
retry:
  attempts: 3
  backoff: exponential
  initial_delay: 100ms
  max_delay: 500ms
`, `
from otter import Context

ctx = Context.from_environment()
attempts = (ctx.state.get("attempts") or 0) + 1
ctx.state.set("attempts", attempts)
ctx.log.info("attempt", n=attempts)
if attempts < 3:
    raise RuntimeError("simulated failure %d" % attempts)
print("recovered on attempt %d" % attempts)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "flaky", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	view := awaitTerminal(t, d, runID)

	if view.Run.Status != runs.StatusFailed {
		t.Errorf("first attempt status = %s, want failed", view.Run.Status)
	}
	if view.LatestStatus != runs.StatusSucceeded {
		t.Fatalf("chain latest status = %s, want succeeded", view.LatestStatus)
	}
	if len(view.Attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(view.Attempts))
	}

	for i, attempt := range view.Attempts {
		wantAttempt := i + 1
		if attempt.Attempt != wantAttempt {
			t.Errorf("attempts[%d].Attempt = %d, want %d", i, attempt.Attempt, wantAttempt)
		}
		if i > 0 {
			if attempt.ParentRunID == nil || *attempt.ParentRunID != view.Attempts[i-1].ID {
				t.Errorf("attempts[%d] does not link back to the previous attempt", i)
			}
		}
	}
	if view.Attempts[0].Status != runs.StatusFailed || view.Attempts[1].Status != runs.StatusFailed {
		t.Errorf("first two attempts should have failed, got %s and %s",
			view.Attempts[0].Status, view.Attempts[1].Status)
	}
	if view.Attempts[2].Status != runs.StatusSucceeded {
		t.Errorf("final attempt status = %s, want succeeded", view.Attempts[2].Status)
	}

	if got := string(getState(t, d, "flaky", "attempts")); got != "3" {
		t.Errorf("state attempts = %s, want 3", got)
	}

	// Backoff must be honoured: the retry may not start before the first
	// attempt finished plus initial_delay (100ms).
	if view.Attempts[0].FinishedAt == nil || view.Attempts[1].StartedAt == nil {
		t.Fatal("expected the first two attempts to have timestamps")
	}
	gap := view.Attempts[1].StartedAt.Sub(*view.Attempts[0].FinishedAt)
	if gap < 90*time.Millisecond {
		t.Errorf("retry started %s after the failure; backoff was not applied", gap)
	}
}

func TestRetryPolicyStopsAtMaxAttempts(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "always-fails", `
version: 1
name: always-fails
entrypoint: main.py
timeout: 30
retry:
  attempts: 3
  backoff: none
`, `
import sys
print("failing")
sys.exit(7)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "always-fails", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	view := awaitTerminal(t, d, runID)
	if view.LatestStatus != runs.StatusFailed {
		t.Fatalf("latest status = %s, want failed", view.LatestStatus)
	}
	if len(view.Attempts) != 3 {
		t.Fatalf("expected exactly 3 attempts (attempts: 3), got %d", len(view.Attempts))
	}
	last := view.Attempts[2]
	if last.ExitCode == nil || *last.ExitCode != 7 {
		t.Errorf("final exit code = %v, want 7", last.ExitCode)
	}
}

func TestTimeoutMarksRunTimedOut(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "hang", `
version: 1
name: hang
entrypoint: main.py
timeout: 1
retry:
  attempts: 0
`, `
import time
print("hanging", flush=True)
time.sleep(300)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "hang", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	start := time.Now()
	view := awaitTerminal(t, d, runID)
	elapsed := time.Since(start)

	if view.Run.Status != runs.StatusTimedOut {
		t.Fatalf("status = %s (%s), want timed_out", view.Run.Status, view.Run.ErrorString())
	}
	if elapsed > 30*time.Second {
		t.Errorf("timeout took %s; the child was not killed promptly", elapsed)
	}
	if !strings.Contains(view.Run.ErrorString(), "timeout") {
		t.Errorf("error = %q, want it to mention the timeout", view.Run.ErrorString())
	}
	if view.Run.ExitCode != nil {
		t.Errorf("a signal-killed run should not record an exit code, got %v", *view.Run.ExitCode)
	}
	if len(view.Attempts) != 1 {
		t.Errorf("expected no retry with attempts: 0, got %d attempts", len(view.Attempts))
	}
}

func TestTimeoutIsRetriedWhenPolicyAllows(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "hang", `
version: 1
name: hang
entrypoint: main.py
timeout: 1
retry:
  attempts: 2
  backoff: none
`, `
import time
time.sleep(300)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "hang", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	view := awaitTerminal(t, d, runID)
	if view.LatestStatus != runs.StatusTimedOut {
		t.Fatalf("latest status = %s, want timed_out", view.LatestStatus)
	}
	if len(view.Attempts) != 2 {
		t.Fatalf("expected the timeout to be retried once (attempts: 2), got %d attempts", len(view.Attempts))
	}
}

func TestMissingSecretFailsWithoutRetrying(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "needs-secret", `
version: 1
name: needs-secret
entrypoint: main.py
timeout: 30
retry:
  attempts: 5
secrets:
  - REQUIRED_TOKEN
`, `
print("should never run")
`)

	// An empty static provider guarantees the secret is absent regardless of
	// the environment the test runs in.
	d := newDaemon(t, root, "", secrets.NewStaticProvider(map[string]string{}), nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "needs-secret", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusFailed {
		t.Fatalf("status = %s, want failed", view.Run.Status)
	}
	if !strings.Contains(view.Run.ErrorString(), "REQUIRED_TOKEN") {
		t.Errorf("error = %q, want it to name the missing secret", view.Run.ErrorString())
	}
	if len(view.Attempts) != 1 {
		t.Fatalf("a configuration failure must not be retried, got %d attempts", len(view.Attempts))
	}
	if stdout := logMessages(t, d, runID, runs.StreamStdout); len(stdout) != 0 {
		t.Errorf("python must not run when a secret is missing, but stdout was %v", stdout)
	}

	// The same manifest must run once the secret exists.
	d2 := newDaemon(t, root, "", secrets.NewStaticProvider(map[string]string{"REQUIRED_TOKEN": "shh"}), nil)
	startDaemon(t, d2)
	secondID, err := d2.SubmitRun(context.Background(), "needs-secret", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	if view := awaitTerminal(t, d2, secondID); view.Run.Status != runs.StatusSucceeded {
		t.Fatalf("status with the secret present = %s (%s), want succeeded",
			view.Run.Status, view.Run.ErrorString())
	}
}

func TestConcurrencyLimitSerializesRuns(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "slow", `
version: 1
name: slow
entrypoint: main.py
timeout: 60
concurrency: 1
retry:
  attempts: 0
`, `
import time
from otter import Context

ctx = Context.from_environment()
ctx.log.info("start", run_id=ctx.run_id)
time.sleep(0.6)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id, err := d.SubmitRun(context.Background(), "slow", api.TriggerPayload{Type: api.TriggerManual})
		if err != nil {
			t.Fatalf("submit run %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	done := func() bool {
		list, err := d.runs.List(context.Background(), runs.Filter{IntegrationID: runtimeID(t, d, "slow"), Limit: 10})
		if err != nil {
			return false
		}
		finished := 0
		for _, run := range list {
			if run.Status == runs.StatusSucceeded {
				finished++
			}
		}
		return finished == 3
	}

	peak := trackPeakConcurrency(d, runtimeID(t, d, "slow"), done, 60*time.Second)
	if peak > 1 {
		t.Fatalf("concurrency: 1 allowed %d simultaneous runs", peak)
	}
	if peak == 0 {
		t.Fatal("never observed the integration running")
	}

	for _, id := range ids {
		if view := awaitTerminal(t, d, id); view.Run.Status != runs.StatusSucceeded {
			t.Errorf("run %s status = %s, want succeeded", id, view.Run.Status)
		}
	}
}

func TestConcurrencyAboveOneRunsInParallel(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "parallel", `
version: 1
name: parallel
entrypoint: main.py
timeout: 60
concurrency: 3
retry:
  attempts: 0
`, `
import time
time.sleep(1.0)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	for i := 0; i < 3; i++ {
		if _, err := d.SubmitRun(context.Background(), "parallel", api.TriggerPayload{Type: api.TriggerManual}); err != nil {
			t.Fatalf("submit run %d: %v", i, err)
		}
	}

	done := func() bool {
		list, err := d.runs.List(context.Background(), runs.Filter{IntegrationID: runtimeID(t, d, "parallel"), Limit: 10})
		if err != nil {
			return false
		}
		finished := 0
		for _, run := range list {
			if run.Status.Terminal() {
				finished++
			}
		}
		return finished == 3
	}

	peak := trackPeakConcurrency(d, runtimeID(t, d, "parallel"), done, 60*time.Second)
	if peak < 2 {
		t.Fatalf("concurrency: 3 only ever ran %d at a time", peak)
	}
}

func TestCancelRunningRunIsNotRetried(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "slow", `
version: 1
name: slow
entrypoint: main.py
timeout: 60
retry:
  attempts: 3
`, `
import time
time.sleep(300)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "slow", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	waitForStatus(t, d, runID, runs.StatusRunning, 30*time.Second)

	if err := d.CancelRun(context.Background(), runID); err != nil {
		t.Fatalf("cancel run: %v", err)
	}

	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", view.Run.Status, view.Run.ErrorString())
	}
	if len(view.Attempts) != 1 {
		t.Fatalf("cancelled runs must not be retried, got %d attempts", len(view.Attempts))
	}
}

func TestCancelQueuedRunRemovesItFromTheQueue(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "slow", `
version: 1
name: slow
entrypoint: main.py
timeout: 60
concurrency: 1
retry:
  attempts: 3
`, `
import time
time.sleep(2)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	blocker, err := d.SubmitRun(context.Background(), "slow", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit blocker: %v", err)
	}
	waitForStatus(t, d, blocker, runs.StatusRunning, 30*time.Second)

	queued, err := d.SubmitRun(context.Background(), "slow", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit queued run: %v", err)
	}
	waitForStatus(t, d, queued, runs.StatusQueued, 30*time.Second)

	if err := d.CancelRun(context.Background(), queued); err != nil {
		t.Fatalf("cancel queued run: %v", err)
	}

	run, err := d.runs.Get(context.Background(), queued)
	if err != nil {
		t.Fatalf("get queued run: %v", err)
	}
	if run.Status != runs.StatusCancelled {
		t.Fatalf("status = %s, want cancelled", run.Status)
	}
	present, err := d.queue.Contains(context.Background(), queued)
	if err != nil {
		t.Fatalf("queue contains: %v", err)
	}
	if present {
		t.Error("the cancelled run is still in the queue")
	}
}

func TestCrashRecoveryMarksRunningRunsFailedAndRetries(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", `
version: 1
name: job
entrypoint: main.py
timeout: 30
retry:
  attempts: 2
  backoff: none
`, `
from otter import Context

ctx = Context.from_environment()
ctx.state.set("ran", True)
print("job ran")
`)

	dataDir := t.TempDir()
	jobID := identityIDFor(t, root, dataDir, "job")

	// Simulate a daemon that was killed while this run was executing.
	seed, err := database.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := database.Migrate(context.Background(), seed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	startedAt := time.Now().UTC().Add(-time.Minute)
	seedRun := &runs.Run{
		ID:            "interrupted-run",
		IntegrationID: jobID,
		TriggerType:   runs.TriggerManual,
		Status:        runs.StatusRunning,
		Attempt:       1,
		CreatedAt:     startedAt,
		StartedAt:     &startedAt,
	}
	if err := runs.NewStore(seed.DB).Create(context.Background(), seedRun); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	// Constructing a daemon on that data directory performs recovery.
	d := newDaemon(t, root, dataDir, nil, nil)

	view, err := d.GetRunDetail(context.Background(), "interrupted-run")
	if err != nil {
		t.Fatalf("get recovered run: %v", err)
	}
	if view.Run.Status != runs.StatusFailed {
		t.Fatalf("recovered status = %s, want failed", view.Run.Status)
	}
	if !strings.Contains(view.Run.ErrorString(), "otter daemon restarted during execution") {
		t.Errorf("recovered error = %q, want the documented restart message", view.Run.ErrorString())
	}
	if view.Run.FinishedAt == nil {
		t.Error("recovered run should have a finished_at timestamp")
	}
	if len(view.Attempts) != 2 {
		t.Fatalf("expected a retry attempt to be queued, got %d attempts", len(view.Attempts))
	}
	if view.Attempts[1].Status != runs.StatusRetrying {
		t.Errorf("retry status = %s, want retrying", view.Attempts[1].Status)
	}

	// Starting the daemon must actually execute the retry.
	startDaemon(t, d)
	retryView := awaitTerminal(t, d, view.Attempts[1].ID)
	if retryView.Run.Status != runs.StatusSucceeded {
		t.Fatalf("retry status = %s (%s), want succeeded",
			retryView.Run.Status, retryView.Run.ErrorString())
	}
	if got := string(getState(t, d, "job", "ran")); got != "true" {
		t.Errorf("state ran = %s, want true", got)
	}
}

func TestCrashRecoveryWithoutRetryPolicyLeavesRunFailed(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", `
version: 1
name: job
entrypoint: main.py
timeout: 30
retry:
  attempts: 0
`, `print("noop")`)

	dataDir := t.TempDir()
	jobID := identityIDFor(t, root, dataDir, "job")
	seed, err := database.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := database.Migrate(context.Background(), seed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := runs.NewStore(seed.DB).Create(context.Background(), &runs.Run{
		ID:            "interrupted",
		IntegrationID: jobID,
		TriggerType:   runs.TriggerManual,
		Status:        runs.StatusRunning,
		Attempt:       1,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	_ = seed.Close()

	d := newDaemon(t, root, dataDir, nil, nil)
	view, err := d.GetRunDetail(context.Background(), "interrupted")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if view.Run.Status != runs.StatusFailed {
		t.Errorf("status = %s, want failed", view.Run.Status)
	}
	if len(view.Attempts) != 1 {
		t.Errorf("attempts: 0 must not enqueue a retry, got %d attempts", len(view.Attempts))
	}
}

func TestGracefulShutdownFailsAndRetriesInFlightRuns(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "slow", `
version: 1
name: slow
entrypoint: main.py
timeout: 120
retry:
  attempts: 2
  backoff: none
`, `
import time
time.sleep(300)
`)

	dataDir := t.TempDir()
	d := newDaemon(t, root, dataDir, nil, func(cfg *config.DaemonConfig) {
		// A short grace period keeps the test fast while still exercising the
		// "terminate what is left" path.
		cfg.ShutdownGrace = 300 * time.Millisecond
	})
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "slow", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	waitForStatus(t, d, runID, runs.StatusRunning, 30*time.Second)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := d.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// The database is closed by now, so inspect the persisted outcome directly.
	root2, attempts := readRunFromDisk(t, dataDir, runID)
	if root2.Status != runs.StatusFailed {
		t.Fatalf("status after shutdown = %s, want failed", root2.Status)
	}
	if !strings.Contains(root2.ErrorString(), "otter daemon shut down during execution") {
		t.Errorf("error = %q, want the documented shutdown message", root2.ErrorString())
	}
	if len(attempts) != 2 {
		t.Fatalf("expected a retry to be enqueued after shutdown, got %d attempts", len(attempts))
	}
}

func TestGracefulShutdownLetsShortRunsFinish(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "quick", `
version: 1
name: quick
entrypoint: main.py
timeout: 30
retry:
  attempts: 0
`, `
import time
time.sleep(1)
print("finished during grace")
`)

	dataDir := t.TempDir()
	d := newDaemon(t, root, dataDir, nil, func(cfg *config.DaemonConfig) {
		cfg.ShutdownGrace = 20 * time.Second
	})
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "quick", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	waitForStatus(t, d, runID, runs.StatusRunning, 30*time.Second)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := d.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	run, _ := readRunFromDisk(t, dataDir, runID)
	if run.Status != runs.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded: the grace period should have been honoured",
			run.Status, run.ErrorString())
	}
}

func TestQueuedRunsSurviveRestart(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", `
version: 1
name: job
entrypoint: main.py
timeout: 30
retry:
  attempts: 0
`, `
print("job ran")
`)

	dataDir := t.TempDir()
	d := newDaemon(t, root, dataDir, nil, nil)
	startDaemon(t, d)

	// A run whose status says "queued" but which has no queue row is the one
	// inconsistency a crash can leave behind; reconciliation must repair it.
	orphan := &runs.Run{
		ID:            "orphan-run",
		IntegrationID: runtimeID(t, d, "job"),
		TriggerType:   runs.TriggerManual,
		Status:        runs.StatusQueued,
		Attempt:       1,
		CreatedAt:     time.Now().UTC(),
	}
	if err := d.runs.Create(context.Background(), orphan); err != nil {
		t.Fatalf("create orphan run: %v", err)
	}

	present, err := d.queue.Contains(context.Background(), orphan.ID)
	if err != nil {
		t.Fatalf("queue contains: %v", err)
	}
	if present {
		t.Fatal("test setup error: the orphan run should not be in the queue")
	}

	if err := d.reconcileQueue(context.Background()); err != nil {
		t.Fatalf("reconcile queue: %v", err)
	}

	present, err = d.queue.Contains(context.Background(), orphan.ID)
	if err != nil {
		t.Fatalf("queue contains: %v", err)
	}
	if !present {
		t.Fatal("reconciliation did not re-enqueue the orphaned run")
	}

	view := awaitTerminal(t, d, orphan.ID)
	if view.Run.Status != runs.StatusSucceeded {
		t.Errorf("re-enqueued run status = %s, want succeeded", view.Run.Status)
	}
}

func TestCronTriggerRegistrationAndFiring(t *testing.T) {
	root := t.TempDir()

	// @every is a robfig/cron descriptor, accepted by the same parser used for
	// standard expressions. It keeps the test fast; production manifests use
	// normal five-field cron.
	writeIntegration(t, root, "ticker", `
version: 1
name: ticker
entrypoint: main.py
timeout: 30
trigger:
  cron: "@every 1s"
retry:
  attempts: 0
`, `
from otter import Context
ctx = Context.from_environment()
ctx.log.info("tick", trigger=ctx.trigger.type)
`)

	writeIntegration(t, root, "five-field", `
version: 1
name: five-field
entrypoint: main.py
timeout: 30
trigger:
  cron: "*/5 * * * *"
`, `print("scheduled")`)

	d := newDaemon(t, root, "", nil, nil)

	spec, ok := d.sched.Spec(runtimeID(t, d, "ticker"))
	if !ok || spec != "@every 1s" {
		t.Fatalf("cron spec = %q (registered=%v), want @every 1s", spec, ok)
	}
	if _, ok := d.sched.Spec(runtimeID(t, d, "five-field")); !ok {
		t.Error("the five-field cron expression was not registered")
	}
	if d.sched.Count() != 2 {
		t.Errorf("registered cron triggers = %d, want 2", d.sched.Count())
	}

	startDaemon(t, d)

	if next, ok := d.sched.Next(runtimeID(t, d, "five-field")); !ok || next.IsZero() {
		t.Error("a registered cron trigger should expose its next fire time")
	}

	list := awaitRunCount(t, d, runtimeID(t, d, "ticker"), 1, 20*time.Second)
	if list[0].TriggerType != runs.TriggerCron {
		t.Errorf("cron run trigger type = %q, want cron", list[0].TriggerType)
	}

	view := awaitTerminal(t, d, list[0].ID)
	if view.Run.Status != runs.StatusSucceeded {
		t.Fatalf("cron run status = %s (%s), want succeeded",
			view.Run.Status, view.Run.ErrorString())
	}

	// The run metadata must record that it came from cron.
	var meta map[string]any
	if err := json.Unmarshal(view.Run.Metadata, &meta); err != nil {
		t.Fatalf("decode run metadata %s: %v", view.Run.Metadata, err)
	}
	if meta["type"] != "cron" {
		t.Errorf("metadata type = %v, want cron", meta["type"])
	}
	if _, ok := meta["scheduled_at"]; !ok {
		t.Error("cron run metadata should include scheduled_at")
	}
}

func TestWebhookTriggerAuthenticatesAndPassesBody(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "hook", `
version: 1
name: hook
entrypoint: main.py
timeout: 30
trigger:
  webhook:
    enabled: true
retry:
  attempts: 0
`, `
from otter import Context

ctx = Context.from_environment()
ctx.state.set("body", ctx.trigger.body)
ctx.state.set("header", ctx.trigger.header("X-Test"))
ctx.state.set("type", ctx.trigger.type)
print("hook %s" % ctx.trigger.body)
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	token, ok := d.WebhookTokenFor("hook")
	if !ok || token == "" {
		t.Fatal("a webhook-enabled integration should have a token")
	}
	if _, ok := d.WebhookTokenFor("missing"); ok {
		t.Error("an unknown integration must not report a webhook token")
	}

	base := "http://" + d.cfg.Listen + "/v1/hooks/hook"
	post := func(tokenValue string, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base, bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if tokenValue != "" {
			req.Header.Set("X-Otter-Token", tokenValue)
		}
		req.Header.Set("X-Test", "hello")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post webhook: %v", err)
		}
		return resp
	}

	unauthorized := post("", `{}`)
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Errorf("webhook without a token = %d, want 401", unauthorized.StatusCode)
	}

	wrong := post("not-the-token", `{}`)
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("webhook with a wrong token = %d, want 401", wrong.StatusCode)
	}

	okResp := post(token, `{"customer_id":"c-42"}`)
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook with a valid token = %d, want 202", okResp.StatusCode)
	}

	var accepted api.SubmitRunResponse
	if err := json.NewDecoder(okResp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode webhook response: %v", err)
	}
	if accepted.RunID == "" {
		t.Fatal("webhook response did not include a run id")
	}

	view := awaitTerminal(t, d, accepted.RunID)
	if view.Run.Status != runs.StatusSucceeded {
		t.Fatalf("webhook run status = %s (%s), want succeeded",
			view.Run.Status, view.Run.ErrorString())
	}
	if view.Run.TriggerType != runs.TriggerWebhook {
		t.Errorf("trigger type = %q, want webhook", view.Run.TriggerType)
	}

	var body map[string]any
	if err := json.Unmarshal(getState(t, d, "hook", "body"), &body); err != nil {
		t.Fatalf("decode stored body: %v", err)
	}
	if body["customer_id"] != "c-42" {
		t.Errorf("trigger body = %v, want customer_id c-42", body)
	}
	if got := string(getState(t, d, "hook", "header")); got != `"hello"` {
		t.Errorf("trigger header = %s, want \"hello\"", got)
	}
	if got := string(getState(t, d, "hook", "type")); got != `"webhook"` {
		t.Errorf("trigger type in python = %s, want \"webhook\"", got)
	}
}

func TestSubmitRunRejectsUnknownAndInvalidIntegrations(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "good", `
version: 1
name: good
entrypoint: main.py
`, `print("ok")`)

	// A manifest that parses but does not validate: the entrypoint is missing.
	if err := os.MkdirAll(filepath.Join(root, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	brokenManifest := "version: 1\nname: broken\nentrypoint: does-not-exist.py\n"
	if err := os.WriteFile(filepath.Join(root, "broken", config.ManifestFileName), []byte(brokenManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	d := newDaemon(t, root, "", nil, nil)

	if _, err := d.SubmitRun(context.Background(), "nope", api.TriggerPayload{}); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("unknown integration error = %v, want api.ErrNotFound", err)
	}
	if _, err := d.SubmitRun(context.Background(), "broken", api.TriggerPayload{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("invalid integration error = %v, want api.ErrInvalid", err)
	}

	// An invalid integration must not stop the valid one from running.
	startDaemon(t, d)
	runID, err := d.SubmitRun(context.Background(), "good", api.TriggerPayload{})
	if err != nil {
		t.Fatalf("submit run for the valid integration: %v", err)
	}
	if view := awaitTerminal(t, d, runID); view.Run.Status != runs.StatusSucceeded {
		t.Errorf("valid integration status = %s, want succeeded", view.Run.Status)
	}

	// The invalid integration should still be visible in the API, flagged.
	view, ok := d.GetIntegration("broken")
	if !ok {
		t.Fatal("the invalid integration is missing from the registry")
	}
	if view.Valid {
		t.Error("the broken integration was reported as valid")
	}
	if view.Error == "" {
		t.Error("the invalid integration should carry a validation error")
	}
}

// A submission binds to the released manifest, so editing the live manifest's
// python.mode cannot move a released run onto different execution settings. The
// live manifest here claims managed Python (with the files such a manifest
// requires) while the release was staged external; the run must record external.
func TestSubmissionBindsToTheReleasedPythonMode(t *testing.T) {
	root := t.TempDir()
	dir := writeIntegration(t, root, "demo", `
version: 1
name: demo
entrypoint: main.py
python:
  mode: external
`, `print("ok")`)
	dataDir := t.TempDir()

	demoID := identityIDFor(t, root, dataDir, "demo")
	manager := release.Manager{DataDir: dataDir}
	layout, err := release.Plan(root, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := manager.StageWithLayout(demoID, dir, layout, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(demoID, meta.Digest); err != nil {
		t.Fatal(err)
	}

	// The live manifest now claims managed Python. Only the bound release can
	// still say the code runs external.
	writeIntegration(t, root, "demo", `
version: 1
name: demo
entrypoint: main.py
python:
  mode: managed
`, `print("ok")`)
	for _, name := range []string{".python-version", "pyproject.toml", "uv.lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("3.13.5\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d := newDaemonWith(t, root, dataDir, nil, nil, false)
	runID, err := d.SubmitRun(context.Background(), "demo", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	run, err := d.runs.Get(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PythonMode != "external" {
		t.Errorf("bound run python mode = %q, want external from the released manifest", run.PythonMode)
	}
}

func TestStateAPIRoundTripAndNamespacing(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "a", "version: 1\nname: a\nentrypoint: main.py\n", `print("a")`)
	writeIntegration(t, root, "b", "version: 1\nname: b\nentrypoint: main.py\n", `print("b")`)
	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	if _, err := d.SetState(ctx, "a", "cursor", json.RawMessage(`{"id":7}`)); err != nil {
		t.Fatalf("set state: %v", err)
	}
	if _, err := d.SetState(ctx, "b", "cursor", json.RawMessage(`"other"`)); err != nil {
		t.Fatalf("set state: %v", err)
	}

	if got := string(getState(t, d, "a", "cursor")); got != `{"id":7}` {
		t.Errorf("state a/cursor = %s, want {\"id\":7}", got)
	}
	if got := string(getState(t, d, "b", "cursor")); got != `"other"` {
		t.Errorf("state b/cursor = %s, want \"other\"", got)
	}

	all, err := d.AllState(ctx, "a")
	if err != nil {
		t.Fatalf("all state: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("integration a has %d state keys, want 1", len(all))
	}

	if _, err := d.GetState(ctx, "a", "missing"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("missing key error = %v, want not found", err)
	}
	if _, err := d.GetState(ctx, "nope", "cursor"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("unknown integration error = %v, want not found", err)
	}
	if _, err := d.SetState(ctx, "a", "bad key", json.RawMessage(`1`)); err == nil {
		t.Error("an invalid state key should be rejected")
	}
	if _, err := d.SetState(ctx, "a", "k", json.RawMessage(`{`)); err == nil {
		t.Error("an invalid state value should be rejected")
	}

	deleted, err := d.DeleteState(ctx, "a", "cursor")
	if err != nil || !deleted {
		t.Fatalf("delete state = %v, %v; want true, nil", deleted, err)
	}
	deleted, err = d.DeleteState(ctx, "a", "cursor")
	if err != nil || deleted {
		t.Fatalf("second delete = %v, %v; want false, nil", deleted, err)
	}
}

func TestRunTokensAreScopedAndRevoked(t *testing.T) {
	registry := newRunTokenRegistry()

	token, err := registry.Issue("run-1", "integration-a", 0)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	if registry.Len() != 1 {
		t.Fatalf("registry size = %d, want 1", registry.Len())
	}

	scope, ok := registry.Lookup(token)
	if !ok {
		t.Fatal("a freshly issued token should resolve")
	}
	if scope.RunID != "run-1" || scope.IntegrationID != "integration-a" {
		t.Errorf("scope = %+v, want run-1/integration-a", scope)
	}

	registry.Revoke(token)
	if _, ok := registry.Lookup(token); ok {
		t.Error("a revoked token must not resolve")
	}
	if registry.Len() != 0 {
		t.Errorf("registry size after revoke = %d, want 0", registry.Len())
	}

	// Issuing a second token for the same run replaces the first.
	first, _ := registry.Issue("run-2", "integration-b", 0)
	second, _ := registry.Issue("run-2", "integration-b", 0)
	if _, ok := registry.Lookup(first); ok {
		t.Error("re-issuing should invalidate the previous token for that run")
	}
	if _, ok := registry.Lookup(second); !ok {
		t.Error("the newest token should resolve")
	}

	// Tokens must survive concurrent use.
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			tok, err := registry.Issue("concurrent", "integration-c", 0)
			if err == nil {
				registry.Lookup(tok)
				registry.Revoke(tok)
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

func TestCapacityReserveRelease(t *testing.T) {
	cap := newCapacity(4)
	cap.setLimits(map[string]int{"one": 1, "two": 2})

	if !cap.Reserve("one") {
		t.Fatal("first reservation for a concurrency-1 integration should succeed")
	}
	if cap.Reserve("one") {
		t.Fatal("second concurrent reservation must be refused")
	}
	if !cap.Reserve("two") || !cap.Reserve("two") {
		t.Fatal("a concurrency-2 integration should allow two reservations")
	}
	if cap.Reserve("two") {
		t.Fatal("a third reservation for a concurrency-2 integration must be refused")
	}
	if cap.totalRunning() != 3 {
		t.Fatalf("total running = %d, want 3", cap.totalRunning())
	}

	cap.Release("one")
	if !cap.Reserve("one") {
		t.Fatal("releasing a slot should allow a new reservation")
	}

	// An integration with no manifest limit defaults to one.
	if !cap.Reserve("unlisted") {
		t.Fatal("an unlisted integration should default to concurrency 1")
	}
	if cap.Reserve("unlisted") {
		t.Fatal("an unlisted integration should not allow a second reservation")
	}

	cap.close()
	for _, id := range []string{"one", "two", "unlisted", "fresh"} {
		for cap.running(id) > 0 {
			cap.Release(id)
		}
		if cap.Reserve(id) {
			t.Fatalf("a draining runtime must not grant new slots (%s)", id)
		}
	}
}

func TestWorkerPoolLimitCapsGlobalConcurrency(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "parallel", `
version: 1
name: parallel
entrypoint: main.py
timeout: 60
concurrency: 10
retry:
  attempts: 0
`, `
import time
time.sleep(1.0)
`)

	d := newDaemon(t, root, "", nil, func(cfg *config.DaemonConfig) {
		cfg.Workers = 2
	})
	startDaemon(t, d)

	for i := 0; i < 4; i++ {
		if _, err := d.SubmitRun(context.Background(), "parallel", api.TriggerPayload{}); err != nil {
			t.Fatalf("submit run %d: %v", i, err)
		}
	}

	done := func() bool {
		list, err := d.runs.List(context.Background(), runs.Filter{IntegrationID: runtimeID(t, d, "parallel"), Limit: 10})
		if err != nil {
			return false
		}
		finished := 0
		for _, run := range list {
			if run.Status.Terminal() {
				finished++
			}
		}
		return finished == 4
	}

	peak := trackPeakConcurrency(d, runtimeID(t, d, "parallel"), done, 90*time.Second)
	if peak > 2 {
		t.Fatalf("--workers 2 allowed %d simultaneous runs", peak)
	}
	if peak < 2 {
		t.Logf("observed peak concurrency %d; the worker pool may have serialized by chance", peak)
	}
}

func TestDrainingRuntimeRejectsNewRuns(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", "version: 1\nname: job\nentrypoint: main.py\n", `print("ok")`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	d.draining.Store(true)
	if _, err := d.SubmitRun(context.Background(), "job", api.TriggerPayload{}); !errors.Is(err, api.ErrConflict) {
		t.Errorf("submitting while draining = %v, want api.ErrConflict", err)
	}
}

func TestRunMetadataRecordsTriggerType(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", "version: 1\nname: job\nentrypoint: main.py\n", `print("ok")`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "job", api.TriggerPayload{
		Type: api.TriggerManual,
		Body: json.RawMessage(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	run, err := d.runs.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	var meta map[string]any
	if err := json.Unmarshal(run.Metadata, &meta); err != nil {
		t.Fatalf("decode metadata %s: %v", run.Metadata, err)
	}
	if meta["type"] != "manual" {
		t.Errorf("metadata type = %v, want manual", meta["type"])
	}
	body, ok := meta["body"].(map[string]any)
	if !ok || body["a"] != float64(1) {
		t.Errorf("metadata body = %v, want {a:1}", meta["body"])
	}
}

// notifyFailure is the daemon's only notification path. It must send for a
// reported status, respect the allow-list, and never report success.
func TestNotifyFailureSendsOnlyReportedStatuses(t *testing.T) {
	var received []notify.Payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p notify.Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode: %v", err)
		}
		received = append(received, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := &Daemon{
		log:      testLogger(),
		notifier: notify.New(config.NotifyConfig{URL: server.URL + "/hook", On: []string{"failed"}}, testLogger()),
	}
	run := &runs.Run{
		ID: "run-1", IntegrationID: "flaky", Attempt: 2, ReleaseDigest: "abc123",
	}
	exit := 1

	d.notifyFailure(run, runs.Finish{Status: runs.StatusFailed, Error: "boom", ExitCode: &exit},
		"last line", 1234)
	d.notifyFailure(run, runs.Finish{Status: runs.StatusTimedOut}, "ignored", 1)
	d.notifyFailure(run, runs.Finish{Status: runs.StatusSucceeded}, "ignored", 1)

	if len(received) != 1 {
		t.Fatalf("received %d notifications, want exactly the failed one: %+v", len(received), received)
	}
	got := received[0]
	if got.RunID != "run-1" || got.Status != "failed" || got.Detail != "last line" {
		t.Errorf("payload = %+v", got)
	}
	if got.Release != "abc123" {
		t.Errorf("release = %q, want the run's release so a failure ties to a build", got.Release)
	}
	if got.ExitCode == nil || *got.ExitCode != 1 {
		t.Errorf("exit code missing from the payload")
	}
}

// A missing endpoint must not affect the run it is reporting on, and the URL
// is never echoed into the log.
func TestNotifyFailureSurvivesABrokenEndpoint(t *testing.T) {
	d := &Daemon{
		log: testLogger(),
		// Nothing is listening here. The retry backoff is compressed so the
		// point of the test -- that the daemon carries on -- is not hidden
		// behind a production-length delay.
		notifier: notify.New(config.NotifyConfig{URL: "http://127.0.0.1:1/hook"}, testLogger(),
			notify.WithRetryPolicy(2, time.Millisecond)),
	}
	d.notifyFailure(&runs.Run{ID: "run-2", IntegrationID: "flaky", Attempt: 1},
		runs.Finish{Status: runs.StatusFailed, Error: "boom"}, "", 10)
	// Reaching this point without a panic or a blocked call is the assertion:
	// notification is best-effort and must never fail a run.
}

// With more than one daemon syncing the same destination, an alert has to say
// which machine failed.
func TestNotifyFailureIdentifiesTheHost(t *testing.T) {
	var got notify.Payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := &Daemon{
		log:      testLogger(),
		hostname: "droplet-1",
		notifier: notify.New(config.NotifyConfig{URL: server.URL}, testLogger()),
	}
	d.notifyFailure(&runs.Run{ID: "run-1", IntegrationID: "flaky", Attempt: 1},
		runs.Finish{Status: runs.StatusFailed, Error: "boom"}, "", 10)

	if got.Host != "droplet-1" {
		t.Errorf("host = %q, want the daemon's hostname", got.Host)
	}
}

// An integration with no active release cannot run, whatever its Python mode.
// This is the strict half of the release model: nothing executes until a
// snapshot has been activated, so "what ran" is always something that was
// deliberately made live.
func TestUnreleasedIntegrationIsRefused(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", "version: 1\nname: job\nentrypoint: main.py\n", `print("ok")`)

	d := newDaemonUnreleased(t, root, "", nil, nil)
	startDaemon(t, d)

	_, err := d.SubmitRun(context.Background(), "job", api.TriggerPayload{Type: api.TriggerManual})
	if err == nil {
		t.Fatal("a run of an unreleased integration was accepted")
	}
	// A conflict with the integration's state, not a server fault: the API
	// must answer 409 rather than 500.
	if !errors.Is(err, api.ErrConflict) {
		t.Errorf("refusal is not classified as a conflict: %v", err)
	}
	for _, want := range []string{"job", "no active release", "otter release job"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// A run executes the release that was active when it was submitted, not the
// source tree as it stands at execution time. For an external integration that
// is the whole value of a release: it pins no interpreter, but it does pin the
// code.
func TestRunExecutesTheActiveReleaseNotTheLiveTree(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", "version: 1\nname: job\nentrypoint: main.py\n", `print("released")`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	// Edit the source after the release: the next run must not see this.
	if err := os.WriteFile(filepath.Join(root, "job", "main.py"), []byte(`print("edited")`), 0o644); err != nil {
		t.Fatal(err)
	}

	runID, err := d.SubmitRun(context.Background(), "job", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", view.Run.Status, view.Run.ErrorString())
	}
	if view.Run.ReleaseDigest == "" {
		t.Error("the run did not record a release digest")
	}
	stdout := strings.Join(logMessages(t, d, runID, runs.StreamStdout), "\n")
	if !strings.Contains(stdout, "released") {
		t.Errorf("the run did not execute the released snapshot:\n%s", stdout)
	}
	if strings.Contains(stdout, "edited") {
		t.Errorf("the run executed the live source tree:\n%s", stdout)
	}

	// Releasing again is what makes the edit live.
	releaseAll(t, root, d.cfg.DataDir)
	nextID, err := d.SubmitRun(context.Background(), "job", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run after re-release: %v", err)
	}
	next := awaitTerminal(t, d, nextID)
	if next.Run.Status != runs.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", next.Run.Status, next.Run.ErrorString())
	}
	if stdout := strings.Join(logMessages(t, d, nextID, runs.StreamStdout), "\n"); !strings.Contains(stdout, "edited") {
		t.Errorf("the re-released snapshot was not executed:\n%s", stdout)
	}
	if next.Run.ReleaseDigest == view.Run.ReleaseDigest {
		t.Error("re-releasing produced the same digest, so the edit was not captured")
	}
}

// -------------------------------------------------------------------- reload

// minimalManifest is a runnable integration with no triggers, used as the
// base of the reload tests. A per-test name keeps duplicate-name validation
// from interfering.
func minimalManifest(name string) string {
	return `
version: 1
name: ` + name + `
entrypoint: main.py
timeout: 30
`
}

const noopPython = `print("ok")`

func TestReloadAddsAnIntegrationWithoutStoppingTheDaemon(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "existing", `
version: 1
name: existing
entrypoint: main.py
timeout: 30
trigger:
  cron: "@every 6h"
`, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	ctx := context.Background()
	nextBefore, ok := d.sched.Next(runtimeID(t, d, "existing"))
	if !ok {
		t.Fatal("the existing cron trigger has no next fire time")
	}

	// A second integration appears while the daemon is serving.
	writeIntegration(t, root, "fresh", minimalManifest("fresh"), noopPython)

	if _, ok := d.GetIntegration("fresh"); ok {
		t.Fatal("an integration added after startup should not be visible before a reload")
	}

	result, err := d.Reload(ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(result.Added) != 1 || result.Added[0] != "fresh" {
		t.Errorf("added = %v, want [fresh]", result.Added)
	}
	if len(result.Removed) != 0 || len(result.Changed) != 0 {
		t.Errorf("removed = %v, changed = %v, want none", result.Removed, result.Changed)
	}
	if result.Total != 2 || result.Valid != 2 {
		t.Errorf("total = %d valid = %d, want 2 and 2", result.Total, result.Valid)
	}

	view, ok := d.GetIntegration("fresh")
	if !ok || !view.Valid {
		t.Fatalf("fresh is not visible and valid after a reload: ok=%v view=%+v", ok, view)
	}

	// The daemon never restarted: the same API listener is still answering.
	if _, err := api.NewClient("http://"+d.cfg.Listen, "").Health(ctx); err != nil {
		t.Fatalf("the API stopped answering across a reload: %v", err)
	}

	// The integration that did not change kept its schedule, which is the
	// property a restart cannot offer.
	nextAfter, ok := d.sched.Next(runtimeID(t, d, "existing"))
	if !ok {
		t.Fatal("the existing cron trigger disappeared across a reload")
	}
	if !nextAfter.Equal(nextBefore) {
		t.Errorf("next fire time moved from %s to %s: a reload must not disturb an unchanged schedule",
			nextBefore, nextAfter)
	}
	if d.sched.Count() != 1 {
		t.Errorf("registered cron triggers = %d, want 1", d.sched.Count())
	}
}

func TestReloadMakesANewIntegrationRunnableWithoutRestart(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "existing", minimalManifest("existing"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)
	ctx := context.Background()

	writeIntegration(t, root, "fresh", minimalManifest("fresh"), noopPython)

	// Undiscovered: the daemon cannot address it at all.
	if _, err := d.SubmitRun(ctx, "fresh", api.TriggerPayload{Type: api.TriggerManual}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("submitting an undiscovered integration = %v, want not found", err)
	}

	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Discovered, but a run executes the active release, so reload makes it
	// visible and release makes it live. The two steps stay separate.
	if _, err := d.SubmitRun(ctx, "fresh", api.TriggerPayload{Type: api.TriggerManual}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("submitting an unreleased integration = %v, want conflict", err)
	}

	releaseAll(t, root, d.cfg.DataDir)

	runID, err := d.SubmitRun(ctx, "fresh", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit after release: %v", err)
	}
	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusSucceeded {
		t.Fatalf("run status = %s (%s), want succeeded", view.Run.Status, view.Run.ErrorString())
	}
}

func TestReloadReportsChangedAndInvalidManifests(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "kept", minimalManifest("kept"), noopPython)

	d := newDaemon(t, root, "", nil, nil)

	// Edit the manifest of an integration the daemon already knows, and add a
	// directory whose manifest cannot be used.
	writeIntegration(t, root, "kept", `
version: 1
name: kept
entrypoint: main.py
timeout: 45
trigger:
  cron: "@every 3h"
`, noopPython)

	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatalf("mkdir broken: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, config.ManifestFileName),
		[]byte("version: 1\nname: broken\nentrypoint: main.py\ntrigger:\n  cron: \"not a cron\"\n"), 0o644); err != nil {
		t.Fatalf("write broken manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "main.py"), []byte(noopPython), 0o644); err != nil {
		t.Fatalf("write broken entrypoint: %v", err)
	}

	result, err := d.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(result.Changed) != 1 || result.Changed[0] != "kept" {
		t.Errorf("changed = %v, want [kept]", result.Changed)
	}
	if len(result.Added) != 1 || result.Added[0] != "broken" {
		t.Errorf("added = %v, want [broken]", result.Added)
	}
	if len(result.Invalid) != 1 || result.Invalid[0] != "broken" {
		t.Errorf("invalid = %v, want [broken]", result.Invalid)
	}
	if result.Total != 2 || result.Valid != 1 {
		t.Errorf("total = %d valid = %d, want 2 and 1", result.Total, result.Valid)
	}

	// The broken integration is present but not runnable, which is the
	// difference an operator needs: "not there" and "there but broken" are
	// fixed by different actions.
	if _, err := d.SubmitRun(context.Background(), "broken", api.TriggerPayload{Type: api.TriggerManual}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("submitting an invalid integration = %v, want invalid", err)
	}

	// An invalid manifest must not keep its cron trigger.
	if _, ok := d.sched.Spec(runtimeID(t, d, "broken")); ok {
		t.Error("an invalid integration should not have a cron trigger")
	}
}

func TestReloadCancelsQueuedRunsOfARemovedIntegration(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "doomed", minimalManifest("doomed"), noopPython)
	writeIntegration(t, root, "kept", minimalManifest("kept"), noopPython)

	// Not started: no worker claims the run, so it stays queued and is
	// available to be cancelled by the reload.
	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	runID, err := d.SubmitRun(ctx, "doomed", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if run, err := d.runs.Get(ctx, runID); err != nil || run.Status != runs.StatusQueued {
		t.Fatalf("run status = %v (err %v), want queued", run.Status, err)
	}

	if err := os.RemoveAll(filepath.Join(root, "doomed")); err != nil {
		t.Fatalf("remove integration: %v", err)
	}

	result, err := d.Reload(ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "doomed" {
		t.Errorf("removed = %v, want [doomed]", result.Removed)
	}
	if result.RunsCancelled != 1 {
		t.Errorf("runs cancelled = %d, want 1", result.RunsCancelled)
	}

	// The queued run can never execute now, so it is ended with a reason that
	// names the cause rather than failing later without explanation.
	run, err := d.runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != runs.StatusCancelled {
		t.Errorf("run status = %s, want cancelled", run.Status)
	}
	if !strings.Contains(run.ErrorString(), "removed") {
		t.Errorf("run error = %q, want it to name the removal", run.ErrorString())
	}
	if present, err := d.queue.Contains(ctx, runID); err != nil || present {
		t.Errorf("the cancelled run is still queued (present=%v err=%v)", present, err)
	}

	// The integration that stayed was not touched.
	if _, ok := d.GetIntegration("kept"); !ok {
		t.Error("reload removed an integration that is still on disk")
	}
}

func TestReloadOfAnUnchangedDirectoryReportsNothing(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "a", minimalManifest("a"), noopPython)

	d := newDaemon(t, root, "", nil, nil)

	result, err := d.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(result.Added)+len(result.Removed)+len(result.Changed)+len(result.Invalid) != 0 {
		t.Errorf("an unchanged reload reported changes: %+v", result)
	}
	if result.Total != 1 || result.Valid != 1 {
		t.Errorf("total = %d valid = %d, want 1 and 1", result.Total, result.Valid)
	}
	if result.RunsCancelled != 0 {
		t.Errorf("runs cancelled = %d, want 0", result.RunsCancelled)
	}
}

// A manifest that failed to load is fixed by editing the file and reloading.
// This is the case that used to require a restart.
func TestReloadPicksUpAFixedManifest(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "fixed", `
version: 1
name: fixed
entrypoint: main.py
timeout: 30
trigger:
  cron: "not a cron"
`, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	if view, ok := d.GetIntegration("fixed"); !ok || view.Valid {
		t.Fatalf("the broken integration should be present and invalid: ok=%v valid=%v", ok, view.Valid)
	}

	writeIntegration(t, root, "fixed", `
version: 1
name: fixed
entrypoint: main.py
timeout: 30
trigger:
  cron: "@every 4h"
`, noopPython)

	result, err := d.Reload(ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(result.Invalid) != 0 {
		t.Errorf("invalid = %v after the fix, want none", result.Invalid)
	}
	// A manifest that never registered had no identity to change: fixing it
	// registers one, so the fix is reported as an addition rather than a
	// change. Its unregistered placeholder is reported as removed.
	if len(result.Added) != 1 || result.Added[0] != "fixed" {
		t.Errorf("added = %v, want [fixed]", result.Added)
	}

	view, ok := d.GetIntegration("fixed")
	if !ok || !view.Valid {
		t.Fatalf("the fixed integration is still not valid: ok=%v view=%+v", ok, view)
	}
	if spec, ok := d.sched.Spec(runtimeID(t, d, "fixed")); !ok || spec != "@every 4h" {
		t.Errorf("cron spec = %q (registered=%v), want @every 4h", spec, ok)
	}
}

func TestConcurrentReloadIsRejected(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "a", minimalManifest("a"), noopPython)
	d := newDaemon(t, root, "", nil, nil)

	// Hold the reload gate, which is what a reload in flight does.
	d.reloadMu.Lock()
	_, err := d.Reload(context.Background())
	d.reloadMu.Unlock()

	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a concurrent reload = %v, want a conflict", err)
	}
}
