package daemon

// P0-02 -- retry release and environment binding (CA-25).
//
// A retry is a fresh attempt of work that was already admitted. It must execute
// the snapshot and the managed environment its parent was bound to, not the
// release that happens to be active when the retry runs, and not one a crash
// recovery pass happened to find in the live registry.
//
// The four tests here are the evidence for that:
//
//  1. TestRetryExecutesTheParentsReleaseSnapshot -- release A fails once, B is
//     activated while A's retry is pending, and the retry runs A's code.
//  2. TestRetryResolvesTheParentsManagedEnvironment -- the retry resolves the
//     environment digest A recorded, not the one a newer release pins.
//  3. TestPendingBacklogKeepsItsDigestThroughAReleasePrune -- a pending retry
//     survives a retention pass that would otherwise remove its snapshot.
//  4. TestRecoveryPlansRetriesFromTheBoundRelease -- the probe the task list
//     does not mention: crash recovery plans the successor from the run's BOUND
//     release, in both directions. A looser live policy must not extend the
//     bound run's retry budget, and a stricter one must not withhold the retry
//     the bound release promises.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/pyenv"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
)

// ------------------------------------------------------------------ fixtures

// p0RetryJob is the external-mode job every release in these tests carries.
//
// The retry is deliberately longer than the time it takes to stage and
// activate a newer release, so the assertions below observe a genuinely
// *pending* retry rather than one that already ran.
func p0RetryJob(policy string) string {
	return `
version: 1
name: p0-retry
entrypoint: main.py
timeout: 30
retry:
` + policy + `
`
}

// p0ExternalEntrypoint fails attempt 1 and succeeds attempt 2, using a marker
// file in the release snapshot itself. A retry that switched to a newer
// snapshot writes to a different directory, so attempt 2 fails there: the
// marker is what proves which snapshot executed, independently of the digest.
const p0ExternalEntrypoint = `
import os
import sys

marker = "a-marker.txt"
if not os.path.exists(marker):
    with open(marker, "w") as handle:
        handle.write("attempt 1 ran in " + os.getcwd() + "\n")
    print("release-a attempt 1")
    sys.exit(1)
print("release-a attempt 2")
`

// p0StagedJob writes a job tree that only differs from the previous one in the
// given entrypoint, so every stage produces a new release digest without
// disturbing the identity registry or any prepared environment.
func p0StagedJob(t *testing.T, root, name, entrypoint, policy string) string {
	t.Helper()
	for _, stale := range []string{"a-marker.txt"} {
		if err := os.Remove(filepath.Join(root, name, stale)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clear %s: %v", stale, err)
		}
	}
	path := writeJob(t, root, name, p0RetryJob(policy), entrypoint)
	return path
}

// p0StageRelease stages and activates one release, recording the environment
// digest the way `otter release` does. releaseAll stages with an empty
// environment, which is enough for external jobs but leaves a managed
// release's metadata unable to answer "what did this run on?".
func p0StageRelease(t *testing.T, root, dataDir, name, jobID string) release.Metadata {
	t.Helper()
	canonicalRoot, err := identity.Canonical(root)
	if err != nil {
		t.Fatalf("canonical %s: %v", root, err)
	}
	// Identity paths are canonical, so the job directory passed to Plan must be
	// too: mixing the /var and /private/var spellings has no common ancestor.
	jobDir, err := identity.Canonical(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("canonical job %s: %v", name, err)
	}
	manager := release.Manager{DataDir: dataDir}
	environment := ""
	if spec, err := (pyenv.Manager{DataDir: dataDir}).ResolveCurrent(context.Background(), jobDir, jobID); err == nil {
		environment = spec.Digest
	}
	layout, err := release.Plan(canonicalRoot, jobDir, nil)
	if err != nil {
		t.Fatalf("plan release %s: %v", name, err)
	}
	meta, err := manager.StageWithLayout(jobID, jobDir, layout, environment)
	if err != nil {
		t.Fatalf("stage release %s: %v", name, err)
	}
	if err := manager.Activate(jobID, meta.Digest); err != nil {
		t.Fatalf("activate release %s: %v", name, err)
	}
	return meta
}

// p0AwaitFirstAttempt waits for the first attempt alone to reach a terminal
// status. awaitTerminal cannot be used for this: it deliberately waits for the
// whole retry chain to settle, which is exactly the state a test asserting on a
// *pending* retry must not be in.
func p0AwaitFirstAttempt(t *testing.T, dataDir, parentRunID string, timeout time.Duration) *runs.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *runs.Run
	for time.Now().Before(deadline) {
		root, _ := readRunFromDisk(t, dataDir, parentRunID)
		last = root
		if root != nil && root.Status.Terminal() {
			return root
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("attempt 1 of %s did not reach a terminal status within %s (last: %+v)", parentRunID, timeout, last)
	return nil
}

// p0WaitForRetryRow polls the run store until the job has a non-terminal
// successor of parentRunID. It is how a test catches a retry while it is still
// pending, instead of racing the worker that will claim it.
func p0WaitForRetryRow(t *testing.T, d *Daemon, parentRunID string, timeout time.Duration) *runs.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, attempts := readRunFromDisk(t, d.cfg.DataDir, parentRunID)
		for _, attempt := range attempts {
			if attempt.ParentRunID != nil && *attempt.ParentRunID == parentRunID &&
				!attempt.Status.Terminal() {
				return attempt
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no pending retry of %s appeared within %s", parentRunID, timeout)
	return nil
}

// p0WaitForChainLength polls an unstarted daemon's database until the run has n
// attempts, then returns them. Recovery and the worker pool both run while it
// is used, so a fixed sleep would be a race.
func p0WaitForChainLength(t *testing.T, dataDir, runID string, n int, timeout time.Duration) []*runs.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []*runs.Run
	for time.Now().Before(deadline) {
		_, attempts := readRunFromDisk(t, dataDir, runID)
		last = attempts
		if len(attempts) >= n {
			return attempts
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s reached %d attempts, want %d, within %s", runID, len(last), n, timeout)
	return nil
}

// p0AssertSnapshotRan asserts that a run's own attempt actually executed the
// marker file inside its recorded snapshot directory.
//
// The path is compared through EvalSymlinks because the child records
// os.getcwd(), which resolves /var to /private/var on macOS while the release
// metadata keeps the other spelling.
func p0AssertSnapshotRan(t *testing.T, snapshotDir string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(snapshotDir, "a-marker.txt"))
	if err != nil {
		t.Fatalf("the bound snapshot did not execute: %v", err)
	}
	recorded := strings.TrimSpace(string(body))
	resolved, err := filepath.EvalSymlinks(snapshotDir)
	if err != nil {
		t.Fatalf("resolve %s: %v", snapshotDir, err)
	}
	if !strings.Contains(recorded, snapshotDir) && !strings.Contains(recorded, resolved) {
		t.Errorf("marker records %q, want the bound snapshot %s (%s)", recorded, snapshotDir, resolved)
	}
}

// --------------------------------------------------- 1. release-snapshot case

// A retry executes the release its parent was bound to, even after a newer
// release has been activated. The run row's digest and source directory are
// both asserted, and the code that ran is asserted through the snapshot's own
// marker file -- a digest alone would not prove the snapshot executed.
func TestRetryExecutesTheParentsReleaseSnapshot(t *testing.T) {
	requirePython(t)

	root := t.TempDir()
	p0StagedJob(t, root, "p0-retry", p0ExternalEntrypoint, "  attempts: 2\n  backoff: linear\n  initial_delay: 3s")

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "p0-retry", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	// The first attempt fails and the daemon records its successor.
	first := p0AwaitFirstAttempt(t, d.cfg.DataDir, runID, 30*time.Second)
	if first.Status != runs.StatusFailed {
		t.Fatalf("attempt 1 status = %s (%s), want failed", first.Status, first.ErrorString())
	}
	digestA, snapshotA := first.ReleaseDigest, first.ReleaseSourceDir
	if digestA == "" {
		t.Fatal("attempt 1 was not bound to a release digest")
	}

	// The retry is pending: this is the state a release activation must not be
	// allowed to affect.
	retry := p0WaitForRetryRow(t, d, runID, 30*time.Second)
	if retry.ReleaseDigest != digestA || retry.ReleaseSourceDir != snapshotA {
		t.Fatalf("pending retry binding = (%s, %s), want the parent's (%s, %s)",
			retry.ReleaseDigest, retry.ReleaseSourceDir, digestA, snapshotA)
	}
	if retry.EnvironmentDigest != first.EnvironmentDigest {
		t.Errorf("pending retry environment = %q, want the parent's %q",
			retry.EnvironmentDigest, first.EnvironmentDigest)
	}

	// Activate release B while the retry is in the backlog. It is observably
	// different code and fails the retry's second-attempt branch, so a retry
	// that followed the active release would fail rather than succeed.
	writeJob(t, root, "p0-retry", p0RetryJob("  attempts: 1\n"), `print("release-b")`)
	releaseAll(t, root, d.cfg.DataDir)
	if _, err := d.Reload(context.Background()); err != nil {
		t.Fatalf("reload after staging release B: %v", err)
	}
	active, ok, err := release.Manager{DataDir: d.cfg.DataDir}.Active(runtimeID(t, d, "p0-retry"))
	if err != nil || !ok {
		t.Fatalf("release B is not active: ok=%v err=%v", ok, err)
	}
	if active.Digest == digestA {
		t.Fatal("fixture is wrong: release B has the same digest as release A")
	}

	// The retry runs A's snapshot.
	view := awaitTerminal(t, d, runID)
	if view.LatestStatus != runs.StatusSucceeded {
		t.Fatalf("chain latest status = %s (%s), want succeeded on the parent's snapshot",
			view.LatestStatus, view.Run.ErrorString())
	}
	if len(view.Attempts) != 2 {
		t.Fatalf("chain has %d attempts, want 2", len(view.Attempts))
	}

	// Read the rows from disk: ReleaseSourceDir is a real column and json:"-",
	// so the API view cannot prove it.
	dbRoot, attempts := readRunFromDisk(t, d.cfg.DataDir, runID)
	if len(attempts) != 2 {
		t.Fatalf("database has %d attempts, want 2", len(attempts))
	}
	for i, attempt := range attempts {
		if attempt.ReleaseDigest != digestA {
			t.Errorf("attempt %d digest = %s, want release A's %s", i+1, attempt.ReleaseDigest, digestA)
		}
		if attempt.ReleaseSourceDir != snapshotA {
			t.Errorf("attempt %d source dir = %s, want release A's %s", i+1, attempt.ReleaseSourceDir, snapshotA)
		}
	}
	if dbRoot.ReleaseDigest != digestA {
		t.Errorf("root digest = %s, want %s", dbRoot.ReleaseDigest, digestA)
	}

	stdout := strings.Join(logMessages(t, d, attempts[1].ID, runs.StreamStdout), "\n")
	if !strings.Contains(stdout, "release-a attempt 2") {
		t.Errorf("the retry did not execute release A's code:\n%s", stdout)
	}
	p0AssertSnapshotRan(t, snapshotA)
	if _, err := os.Stat(filepath.Join(root, "p0-retry", "a-marker.txt")); !os.IsNotExist(err) {
		t.Errorf("release A's marker leaked into the live tree (err=%v)", err)
	}
}

// -------------------------------------------------------- 2. managed-Python case

// A managed-Python retry resolves the environment digest its parent recorded,
// not the one the newly active release pins. Each environment's interpreter is
// a stub that prints the environment it came from, so the assertion covers the
// environment that actually executed as well as the digest on the row.
func TestRetryResolvesTheParentsManagedEnvironment(t *testing.T) {
	requirePython(t)

	root := t.TempDir()
	dataDir := t.TempDir()

	// Two environment identities: the pyproject lock is the declared input, so
	// changing it moves the digest without needing a second interpreter.
	//
	// The environment is keyed by the durable job id, not the manifest label:
	// production prepares and resolves by identity, so a fixture keyed by label
	// would prepare a different environment than the one a run resolves.
	p0WriteManagedJob(t, root, "p0-retry", "print(\"managed-a\")", "version = 1\n", "  attempts: 2\n  backoff: linear\n  initial_delay: 3s")
	manager := pyenv.Manager{DataDir: dataDir, Probe: func(context.Context, string) error { return nil }}
	pythonVersion := p0LocalPythonVersion(t)
	jobID := identityIDFor(t, root, dataDir, "p0-retry")
	envA := p0PrepareEnvironment(t, manager, root, "p0-retry", jobID, pythonVersion, "version = 1\n")
	releaseA := p0StageRelease(t, root, dataDir, "p0-retry", jobID)

	// Each environment's interpreter is replaced by a stub that announces
	// which one it is and fails the first attempt from inside its own snapshot.
	p0StubManagedInterpreter(t, envA)

	d := newDaemonUnreleased(t, root, dataDir, nil, nil)
	startDaemon(t, d)

	// The run is submitted while only release A exists, so it binds to A's
	// environment. Release B is staged afterwards, while the retry is pending.
	runID, err := d.SubmitRun(context.Background(), "p0-retry", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	first := p0AwaitFirstAttempt(t, d.cfg.DataDir, runID, 30*time.Second)
	if first.Status != runs.StatusFailed {
		t.Fatalf("attempt 1 status = %s (%s), want failed", first.Status, first.ErrorString())
	}
	if first.EnvironmentDigest != envA.Digest {
		t.Fatalf("attempt 1 environment = %s, want release A's %s",
			first.EnvironmentDigest, envA.Digest)
	}
	if first.PythonMode != "managed" {
		t.Fatalf("attempt 1 python mode = %q, want managed", first.PythonMode)
	}

	retry := p0WaitForRetryRow(t, d, runID, 30*time.Second)
	if retry.EnvironmentDigest != envA.Digest {
		t.Fatalf("pending retry environment = %s, want the parent's %s", retry.EnvironmentDigest, envA.Digest)
	}
	if retry.PythonPolicy != first.PythonPolicy || retry.PythonVersion != first.PythonVersion {
		t.Errorf("retry identity = (%s, %s), want the parent's (%s, %s)",
			retry.PythonVersion, retry.PythonPolicy, first.PythonVersion, first.PythonPolicy)
	}

	// Release B pins a different lock, so it resolves a different environment.
	// It is prepared and activated while A's retry is still in the backlog.
	p0WriteManagedJob(t, root, "p0-retry", "print(\"managed-b\")", "version = 2\n", "  attempts: 1\n")
	envB := p0PrepareEnvironment(t, manager, root, "p0-retry", jobID, pythonVersion, "version = 2\n")
	releaseB := p0StageRelease(t, root, dataDir, "p0-retry", jobID)

	if envA.Digest == envB.Digest {
		t.Fatal("fixture is wrong: the two releases resolved the same environment digest")
	}
	if releaseA.Environment != envA.Digest || releaseB.Environment != envB.Digest {
		t.Fatalf("staged environments = (%s, %s), want (%s, %s)",
			releaseA.Environment, releaseB.Environment, envA.Digest, envB.Digest)
	}
	p0StubManagedInterpreter(t, envB)

	active, ok, err := release.Manager{DataDir: dataDir}.Active(jobID)
	if err != nil || !ok {
		t.Fatalf("release B is not active: ok=%v err=%v", ok, err)
	}
	if active.Digest != releaseB.Digest {
		t.Fatalf("active release = %s, want release B's %s", active.Digest, releaseB.Digest)
	}
	if active.Environment != envB.Digest {
		t.Fatalf("release B records environment %s, want %s", active.Environment, envB.Digest)
	}

	view := awaitTerminal(t, d, runID)
	if view.LatestStatus != runs.StatusSucceeded {
		t.Fatalf("chain latest status = %s (%s), want succeeded in the parent's environment",
			view.LatestStatus, view.Run.ErrorString())
	}

	_, attempts := readRunFromDisk(t, dataDir, runID)
	if len(attempts) != 2 {
		t.Fatalf("database has %d attempts, want 2", len(attempts))
	}
	for i, attempt := range attempts {
		if attempt.EnvironmentDigest != envA.Digest {
			t.Errorf("attempt %d environment = %s, want release A's %s", i+1, attempt.EnvironmentDigest, envA.Digest)
		}
		if attempt.PythonPolicy != envA.Policy {
			t.Errorf("attempt %d policy = %q, want %q", i+1, attempt.PythonPolicy, envA.Policy)
		}
	}

	stdout := strings.Join(logMessages(t, d, attempts[1].ID, runs.StreamStdout), "\n")
	if !strings.Contains(stdout, "managed-env="+envA.Digest[:12]) {
		t.Errorf("the retry did not execute release A's environment %s (release B's is %s):\n%s",
			envA.Digest[:12], envB.Digest[:12], stdout)
	}
	if strings.Contains(stdout, "managed-env="+envB.Digest[:12]) {
		t.Errorf("the retry executed release B's environment:\n%s", stdout)
	}
	p0AssertSnapshotRan(t, attempts[1].ReleaseSourceDir)
}

// p0WriteManagedJob writes a managed-Python job tree. python.path and
// python.executable are both unset, which Validate requires for managed mode.
func p0WriteManagedJob(t *testing.T, root, name, entrypoint, lock, policy string) {
	t.Helper()
	path := writeJob(t, root, name, `
version: 1
name: `+name+`
entrypoint: main.py
timeout: 30
python:
  mode: managed
retry:
`+policy, entrypoint)
	for file, body := range map[string]string{
		".python-version": "3.13.5\n",
		"pyproject.toml":  "[project]\nname = 'fixture'\nversion = '0.1.0'\nrequires-python = '>=3.10'\n",
		"uv.lock":         lock,
	} {
		if err := os.WriteFile(filepath.Join(path, file), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
}

// p0PrepareEnvironment prepares an environment offline: a fake uv creates the
// layout and symlinks the local interpreter, exactly as
// internal/pyenv/manager_test.go does. No network and no real uv.
func p0PrepareEnvironment(t *testing.T, manager pyenv.Manager, root, name, jobID, pythonVersion, lock string) pyenv.Ready {
	t.Helper()

	if err := os.WriteFile(filepath.Join(root, name, "uv.lock"), []byte(lock), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	uv := filepath.Join(manager.DataDir, "tools", "uv", "uv")
	if err := os.MkdirAll(filepath.Dir(uv), 0o700); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	real, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed; skipping managed-Python test")
	}
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'uv test'; exit 0; fi\n" +
		"if [ \"$1\" = python ]; then exit 0; fi\n" +
		"mkdir -p \"$UV_PROJECT_ENVIRONMENT/bin\"\nln -sf " + real + " \"$UV_PROJECT_ENVIRONMENT/bin/python\"\n"
	if err := os.WriteFile(uv, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake uv: %v", err)
	}

	// The manifest must exist before Prepare so the version check can pass, and
	// its .python-version pin is authoritative for the environment.
	if err := os.WriteFile(filepath.Join(root, name, ".python-version"), []byte(pythonVersion+"\n"), 0o600); err != nil {
		t.Fatalf("write pin: %v", err)
	}
	ready, err := manager.Prepare(context.Background(), filepath.Join(root, name), jobID, uv)
	if err != nil {
		t.Fatalf("prepare environment: %v", err)
	}
	return ready
}

// p0StubManagedInterpreter replaces an environment's interpreter with a shell
// stub. The stub announces which environment it is -- by the environment's own
// digest, so two environments are distinguishable -- fails the first attempt,
// and on the retry delegates to the real Python launcher so the snapshot's own
// code runs.
func p0StubManagedInterpreter(t *testing.T, env pyenv.Ready) {
	t.Helper()
	real, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed; skipping managed-Python test")
	}
	stub := `#!/bin/sh
echo "managed-env=` + env.Digest[:12] + `"
if [ "$1" = "-m" ] && [ "$2" = "otter._launcher" ]; then
  if [ ! -f a-marker.txt ]; then
    pwd > a-marker.txt
    exit 1
  fi
  shift 2
  exec ` + real + ` -c 'import runpy,sys; sys.argv=sys.argv[1:]; runpy.run_path(sys.argv[0], run_name="__main__")' "$@"
fi
exec ` + real + ` "$@"
`
	// Remove rather than overwrite: the environment's interpreter is a symlink
	// to the real Python, and writing through it would try to replace a
	// system-protected binary.
	if err := os.Remove(env.Interpreter); err != nil {
		t.Fatalf("remove prepared interpreter: %v", err)
	}
	if err := os.WriteFile(env.Interpreter, []byte(stub), 0o700); err != nil {
		t.Fatalf("stub interpreter: %v", err)
	}
}

// p0LocalPythonVersion reads the exact patch version of the local interpreter,
// which is what a managed environment must be pinned to.
func p0LocalPythonVersion(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed; skipping managed-Python test")
	}
	out, err := exec.Command(python, "-c", "import sys; print('.'.join(map(str,sys.version_info[:3])))").Output()
	if err != nil {
		t.Fatalf("read python3 version: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// ------------------------------------------------------------- 3. prune case

// A pending backlog keeps its digest through a release prune. A retry is a
// non-terminal run, so retention must see its digest as pinned and leave the
// snapshot in place; a prune that removed it would strand the retry on a
// snapshot the runtime says is gone.
func TestPendingBacklogKeepsItsDigestThroughAReleasePrune(t *testing.T) {
	requirePython(t)

	root := t.TempDir()
	writeJob(t, root, "p0-retry", p0RetryJob("  attempts: 2\n  backoff: linear\n  initial_delay: 3s"), p0ExternalEntrypoint)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "p0-retry", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	first := p0AwaitFirstAttempt(t, d.cfg.DataDir, runID, 30*time.Second)
	if first.Status != runs.StatusFailed {
		t.Fatalf("attempt 1 status = %s (%s), want failed", first.Status, first.ErrorString())
	}
	digestA, snapshotA := first.ReleaseDigest, first.ReleaseSourceDir
	retry := p0WaitForRetryRow(t, d, runID, 30*time.Second)
	if retry.ReleaseDigest != digestA || retry.ReleaseSourceDir != snapshotA {
		t.Fatalf("pending retry binding = (%s, %s), want (%s, %s)",
			retry.ReleaseDigest, retry.ReleaseSourceDir, digestA, snapshotA)
	}

	// Three more releases, so the pinned one is far outside a one-release
	// window and only the pin can save it.
	for _, stage := range []struct{ entrypoint, policy string }{
		{`print("release-b")`, "  attempts: 1\n"},
		{`print("release-c")`, "  attempts: 1\n"},
		{`print("release-d")`, "  attempts: 1\n"},
	} {
		writeJob(t, root, "p0-retry", p0RetryJob(stage.policy), stage.entrypoint)
		releaseAll(t, root, d.cfg.DataDir)
	}

	// The pin set is exactly what `otter release --keep` computes: every digest
	// a queued, running or retrying run is still bound to.
	referenced := p0PinnedReleases(t, d.cfg.DataDir, runtimeID(t, d, "p0-retry"))
	if !referenced[digestA] {
		t.Fatalf("the pending retry's digest %s is not in the pin set %v", digestA[:12], referenced)
	}
	manager := release.Manager{DataDir: d.cfg.DataDir}
	removed, err := manager.Retain(runtimeID(t, d, "p0-retry"), 1, referenced)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	for _, digest := range removed {
		if digest == digestA {
			t.Fatal("retention removed the release a pending retry is bound to")
		}
	}
	if _, err := os.Stat(filepath.Join(snapshotA, "main.py")); err != nil {
		t.Fatalf("the pending retry's snapshot did not survive the prune: %v", err)
	}

	// And the retry really does execute it.
	view := awaitTerminal(t, d, runID)
	if view.LatestStatus != runs.StatusSucceeded {
		t.Fatalf("chain latest status = %s (%s), want succeeded on the pruned-but-pinned snapshot",
			view.LatestStatus, view.Run.ErrorString())
	}
	_, attempts := readRunFromDisk(t, d.cfg.DataDir, runID)
	if len(attempts) != 2 {
		t.Fatalf("database has %d attempts, want 2", len(attempts))
	}
	if attempts[1].ReleaseDigest != digestA || attempts[1].ReleaseSourceDir != snapshotA {
		t.Errorf("retry binding = (%s, %s), want (%s, %s)",
			attempts[1].ReleaseDigest, attempts[1].ReleaseSourceDir, digestA, snapshotA)
	}
	stdout := strings.Join(logMessages(t, d, attempts[1].ID, runs.StreamStdout), "\n")
	if !strings.Contains(stdout, "release-a attempt 2") {
		t.Errorf("the retry did not execute the pinned snapshot:\n%s", stdout)
	}
	p0AssertSnapshotRan(t, snapshotA)
}

// p0PinnedReleases mirrors internal/cli's pin query: the distinct digests
// non-terminal runs are bound to.
func p0PinnedReleases(t *testing.T, dataDir, jobID string) map[string]bool {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT release_digest FROM runs
		  WHERE job_id = ? AND release_digest <> ''
		    AND status IN ('queued', 'running', 'retrying')`, jobID)
	if err != nil {
		t.Fatalf("pin query: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			t.Fatalf("scan pin: %v", err)
		}
		if digest != "" {
			out[digest] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pin rows: %v", err)
	}
	return out
}

// ---------------------------------------------- 4. recovery retry policy

// Crash recovery plans a successor for an interrupted run from the manifest of
// the release that run was bound to -- the same snapshot executeRun re-reads --
// not from the live registry entry. Each case stages a bound release and a live
// release whose retry policies disagree, then kills a first attempt and lets
// startup recovery decide.
//
// The two directions are the two failure modes of getting this wrong: a live
// policy that is looser must not grant a retry the bound release forbids, and a
// live policy that is stricter must not deny the retry the bound release
// promises. Both cases assert the retry, when it exists, still runs the bound
// snapshot.
func TestRecoveryPlansRetriesFromTheBoundRelease(t *testing.T) {
	requirePython(t)

	cases := []struct {
		name string
		// boundPolicy is release A's retry block: the policy the interrupted run
		// was admitted to.
		boundPolicy string
		// livePolicy is release B's retry block: the policy of the release that
		// is live when recovery runs.
		livePolicy string
		// wantRetry is whether recovery must create a successor.
		wantRetry bool
	}{
		{
			// A live release must not loosen an older run's retry budget.
			name:        "live policy is looser than the bound release",
			boundPolicy: "  attempts: 1\n",
			livePolicy:  "  attempts: 2\n  backoff: none\n",
			wantRetry:   false,
		},
		{
			// A live release must not tighten it either: the retry the bound
			// release promises still happens.
			name:        "live policy is stricter than the bound release",
			boundPolicy: "  attempts: 2\n  backoff: none\n",
			livePolicy:  "  attempts: 1\n",
			wantRetry:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dataDir := t.TempDir()

			// Release A: the release the interrupted run was admitted to.
			p0StagedJob(t, root, "p0-retry", p0ExternalEntrypoint, tc.boundPolicy)
			releaseAll(t, root, dataDir)
			inst := identityIDFor(t, root, dataDir, "p0-retry")
			bound, ok, err := release.Manager{DataDir: dataDir}.Active(inst)
			if err != nil || !ok {
				t.Fatalf("release A is not active: ok=%v err=%v", ok, err)
			}
			boundDir, err := release.Manager{DataDir: dataDir}.SourceDir(bound)
			if err != nil {
				t.Fatalf("resolve release A's snapshot: %v", err)
			}

			// Release B: the release that is live when recovery runs.
			p0StagedJob(t, root, "p0-retry", `print("release-b")`, tc.livePolicy)
			releaseAll(t, root, dataDir)
			live, ok, err := release.Manager{DataDir: dataDir}.Active(inst)
			if err != nil || !ok {
				t.Fatalf("release B is not active: ok=%v err=%v", ok, err)
			}
			if live.Digest == bound.Digest {
				t.Fatal("fixture is wrong: release B did not replace release A")
			}

			// A daemon died while executing release A's first attempt. The
			// killed child had already written its attempt-1 marker into the
			// snapshot, which is the state a successor must observe: attempt 2
			// sees the marker and succeeds.
			if err := os.WriteFile(filepath.Join(boundDir, "a-marker.txt"),
				[]byte("attempt 1 ran in "+boundDir+"\n"), 0o644); err != nil {
				t.Fatalf("plant the killed attempt's marker: %v", err)
			}
			p0SeedRunningRun(t, dataDir, inst, "run-crashed", bound.Digest, boundDir, 1)

			d, err := New(context.Background(), Options{
				Config:  startupConfig(t, root, dataDir),
				Logger:  testLogger(),
				Version: "test",
			})
			if err != nil {
				t.Fatalf("daemon.New (recovery must not fail): %v", err)
			}
			startDaemon(t, d)

			// The interrupted attempt is terminalised whether or not it retries.
			_, attempts := readRunFromDisk(t, dataDir, "run-crashed")
			if len(attempts) < 1 || attempts[0].Status != runs.StatusFailed {
				t.Fatalf("interrupted attempt = %+v, want a failed attempt", attempts)
			}
			if got := attempts[0].ReleaseDigest; got != bound.Digest {
				t.Errorf("interrupted attempt digest = %s, want the bound release %s", got, bound.Digest)
			}

			if !tc.wantRetry {
				// No successor may appear: the bound policy forbids it. Poll for
				// a moment so a wrongly-created retry would be observed rather
				// than missed by a fast read.
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					_, current := readRunFromDisk(t, dataDir, "run-crashed")
					if len(current) != 1 {
						t.Fatalf("recovery created %d attempts; the bound release allows 1, so the live release's policy was applied",
							len(current))
					}
					time.Sleep(25 * time.Millisecond)
				}
				return
			}

			// The bound release promises a second attempt, even though the live
			// release does not.
			// The successor is queued immediately (backoff: none), so a few
			// seconds is ample; a wrongly-absent retry fails fast.
			attempts = p0WaitForChainLength(t, dataDir, "run-crashed", 2, 5*time.Second)
			retry := attempts[1]
			if retry.ReleaseDigest != bound.Digest {
				t.Errorf("recovery-bound retry digest = %s, want the bound release %s", retry.ReleaseDigest, bound.Digest)
			}
			if retry.ReleaseSourceDir != boundDir {
				t.Errorf("recovery-bound retry source dir = %s, want the bound release %s", retry.ReleaseSourceDir, boundDir)
			}
			if live.Digest == retry.ReleaseDigest {
				t.Errorf("the retry was re-bound to the live release %s", live.Digest[:12])
			}

			// It executes the bound snapshot, not the live release's code.
			stderr, otter := p0AwaitAttemptOutput(t, d, "run-crashed", 2, 30*time.Second)
			_, settled := readRunFromDisk(t, dataDir, "run-crashed")
			if len(settled) != 2 {
				t.Fatalf("database has %d attempts, want 2", len(settled))
			}
			if settled[1].Status != runs.StatusSucceeded {
				t.Fatalf("recovery retry status = %s (%s), want succeeded on the bound snapshot",
					settled[1].Status, settled[1].ErrorString())
			}
			stdout := strings.Join(logMessages(t, d, settled[1].ID, runs.StreamStdout), "\n")
			if !strings.Contains(stdout, "release-a attempt 2") {
				t.Errorf("the recovery retry did not execute the bound snapshot:\nstdout=%q\nstderr=%q\notter=%q",
					stdout, stderr, otter)
			}
			p0AssertSnapshotRan(t, boundDir)
		})
	}
}

// p0AwaitAttemptOutput waits until attempt n of a run has reached a terminal
// status, then returns its stderr and otter-stream text. Waiting on status
// rather than on stdout avoids a race with the log sink's flush.
func p0AwaitAttemptOutput(t *testing.T, d *Daemon, runID string, n int, timeout time.Duration) (string, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, attempts := readRunFromDisk(t, d.cfg.DataDir, runID)
		if len(attempts) >= n && attempts[n-1].Status.Terminal() {
			return strings.Join(logMessages(t, d, attempts[n-1].ID, runs.StreamStderr), "\n"),
				strings.Join(logMessages(t, d, attempts[n-1].ID, runs.StreamOtter), "\n")
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("attempt %d of %s did not reach a terminal status within %s", n, runID, timeout)
	return "", ""
}

// p0SeedRunningRun plants the row a killed daemon leaves behind: a run in
// `running` bound to a release digest and source directory.
func p0SeedRunningRun(t *testing.T, dataDir, jobID, runID, digest, sourceDir string, attempt int) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	}()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, job_id, trigger_type, status, attempt, created_at, started_at,
		                   release_digest, release_source_dir, capture_policy)
		 VALUES (?, ?, 'manual', 'running', ?, ?, ?, ?, ?, 'off')`,
		runID, jobID, attempt, database.FormatTime(time.Now().UTC()),
		database.FormatTime(time.Now().UTC()), digest, sourceDir); err != nil {
		t.Fatalf("seed running run: %v", err)
	}
}
