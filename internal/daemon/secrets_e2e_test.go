package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/runs"
)

// These tests are the executable half of P0-12: the manifest reference claims
// that a manifest declares secret *names*, that values come from the daemon's
// environment, and that a missing secret fails a run before Python starts and
// is not retried. The daemon resolves through secrets.EnvProvider by default,
// so a nil provider here exercises the production path -- the same one a
// systemd `EnvironmentFile=` feeds -- not a test double.
//
// The entrypoint is a tripwire: it writes a sentinel file when it runs. The
// final case sets the secret and proves the sentinel *is* written, so an
// absence in the earlier cases cannot be a test that passes because nothing
// works.

// p012SecretName cannot collide with anything a developer's shell exports, and
// it is unset for the duration of the test so "missing" is manufactured rather
// than assumed.
const p012SecretName = "OTTER_P0_12_CASTOR_SHOPIFY_TOKEN"

func requireEnvAbsent(t *testing.T, name string) {
	t.Helper()

	previous, present := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(name, previous)
			return
		}
		_ = os.Unsetenv(name)
	})
}

// TestMissingSecretFailsBeforePythonStartsAndRepeats asserts all three claims
// of the P0-12 evidence bar, each so that it can fail on its own:
//
//  1. the run fails cleanly, naming the secret and the job;
//  2. Python never starts -- asserted by a sentinel file the entrypoint
//     writes, with a positive control proving the sentinel mechanism works;
//  3. the failure is not retried, and it repeats identically on the next run --
//     and again after the daemon is restarted -- rather than succeeding or
//     degrading into a different error.
func TestMissingSecretFailsBeforePythonStartsAndRepeats(t *testing.T) {
	requireEnvAbsent(t, p012SecretName)

	root := t.TempDir()
	dataDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "python-started")

	// The entrypoint's whole job is to prove it ran. It is valid Python that
	// needs nothing from the SDK, so a failure to start cannot be confused
	// with an import error.
	writeJob(t, root, "castor-sync", fmt.Sprintf(`version: 1
name: castor-sync
entrypoint: main.py
timeout: 30
retry:
  attempts: 5
secrets:
  - %s
`, p012SecretName), fmt.Sprintf(`
import pathlib
pathlib.Path(%q).write_text("the job process ran\n")
print("python started")
`, sentinel))

	// A nil provider is the daemon's documented default: the daemon's own
	// environment, which is where the deployed `0600` env file lands.
	d := newDaemon(t, root, dataDir, nil, nil)
	startDaemon(t, d)

	submit := func(d *Daemon) string {
		t.Helper()
		id, err := d.SubmitRun(context.Background(), "castor-sync", api.TriggerPayload{Type: api.TriggerManual})
		if err != nil {
			t.Fatalf("submit run: %v", err)
		}
		return id
	}

	// assertCleanFailure checks one run's shape and returns its message so the
	// caller can compare runs.
	assertCleanFailure := func(d *Daemon, runID string) string {
		t.Helper()

		view := awaitTerminal(t, d, runID)
		if view.Run.Status != runs.StatusFailed {
			t.Fatalf("status = %s, want failed", view.Run.Status)
		}

		message := view.Run.ErrorString()
		if !strings.Contains(message, p012SecretName) {
			t.Errorf("error = %q, want it to name the missing secret %s", message, p012SecretName)
		}
		if !strings.Contains(message, "castor-sync") || !strings.Contains(message, "not available") {
			t.Errorf("error = %q, want it to name the job and say the secret is not available", message)
		}
		if strings.Contains(strings.ToLower(message), "traceback") {
			t.Errorf("error = %q, want a configuration message, not a Python traceback", message)
		}

		// Not retried: the manifest allows five attempts, and a configuration
		// failure must consume exactly one.
		if len(view.Attempts) != 1 {
			t.Fatalf("attempts = %d, want 1: a missing secret must not be retried", len(view.Attempts))
		}
		if view.Attempts[0].Attempt != 1 {
			t.Errorf("attempt number = %d, want 1", view.Attempts[0].Attempt)
		}
		// No process ever exited, which is what a failed launch would have
		// recorded; the failure is the resolver's, before the executor.
		if view.Attempts[0].ExitCode != nil {
			t.Errorf("exit code = %d, want none: no Python process was started", *view.Attempts[0].ExitCode)
		}

		if stdout := logMessages(t, d, runID, runs.StreamStdout); len(stdout) != 0 {
			t.Errorf("python must not run when a secret is missing, but stdout was %v", stdout)
		}
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatal("the entrypoint ran: a missing secret must fail before Python starts")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat sentinel: %v", err)
		}
		return message
	}

	first := assertCleanFailure(d, submit(d))

	// "Repeatedly": the same absent secret must produce the same clean failure
	// on a fresh run, not a one-off, and not a retry of the first.
	second := assertCleanFailure(d, submit(d))
	if first != second {
		t.Errorf("the failure message changed between runs:\nfirst:  %q\nsecond: %q", first, second)
	}

	// The failure must not be a property of this process instance: a daemon
	// that comes back up must reach the same verdict from the same data
	// directory, which is what makes the failure reliable to operate against.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := d.Shutdown(shutdownCtx); err != nil {
		cancel()
		t.Fatalf("shutdown: %v", err)
	}
	cancel()

	restarted := newDaemon(t, root, dataDir, nil, nil)
	startDaemon(t, restarted)

	third := assertCleanFailure(restarted, submit(restarted))
	if third != first {
		t.Errorf("the failure message changed across a daemon restart:\nbefore: %q\nafter:  %q", first, third)
	}

	// Positive control. If the sentinel, the entrypoint or the release were
	// broken, every assertion above would pass for the wrong reason; setting
	// the secret and watching the run succeed rules that out.
	t.Setenv(p012SecretName, "shpat_test_value_not_a_real_credential")

	fourth := awaitTerminal(t, restarted, submit(restarted))
	if fourth.Run.Status != runs.StatusSucceeded {
		t.Fatalf("status with the secret present = %s (%s), want succeeded",
			fourth.Run.Status, fourth.Run.ErrorString())
	}
	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("the entrypoint did not run even with the secret set: %v", err)
	}
	if !strings.Contains(string(body), "the job process ran") {
		t.Errorf("sentinel = %q, want the entrypoint's own output", body)
	}
}
