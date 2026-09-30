package daemon

// P0-01 -- the real crash/kill harness.
//
// Every other crash test in this package is *seeded*: it inserts `running`
// rows into a migrated database and constructs a Daemon, because recovery runs
// inside daemon.New. That proves the recovery logic but cannot observe the
// failures only an abrupt process death exposes -- a lost run, a run whose
// child outlives the daemon, or an outcome that is written twice.
//
// This harness runs the real daemon as a separate OS process, submits more
// than one listing page of runs, waits until they are genuinely executing,
// SIGKILLs that process, restarts it, and then asserts against the database and
// against an external side-effect file that the job wrote:
//
//  1. the daemon is booted against a temporary data directory, as a separate
//     process, through the test binary's own re-exec dispatch (the same pattern
//     internal/executor/proc_linux_test.go uses);
//  2. more than 50 runs are accepted, so recovery must cross the
//     defaultListLimit = 50 page boundary;
//  3. the harness waits until more than 50 children have actually started
//     (the job appends its run id and pid to a file before it holds), not for a
//     timer;
//  4. the daemon is killed with SIGKILL;
//  5. a second daemon process is started over the same data directory;
//  6. every accepted run is asserted to be terminal, and every run that was
//     running at kill time is asserted to have a retry successor -- none lost,
//     and no attempt executed twice;
//  7. fail-closed startup on incomplete recovery is covered by
//     TestStartupFailClosedOnIncompleteRecovery (daemon_test.go); this harness
//     does not duplicate it, and asserts instead that startup recovery did
//     complete, by reading the restarted daemon's own recovery log line.
//
// Platform split. The runtime has no parent-death signal outside Linux
// (internal/executor/proc_darwin.go): on macOS an in-flight child is reparented
// after a SIGKILL and keeps running. The loss/re-enqueue assertions therefore
// run on every platform, and the child-death and "no orphan completed the work"
// assertions live in crash_harness_linux_test.go behind a linux build tag. See
// crash_harness_nonlinux_test.go for the exclusion this records.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/runs"
)

// The harness job. The numbers are chosen so both halves of "every accepted
// run" are exercised rather than only the queued path:
//
//   - crashHarnessRuns (80) is more than one listing page and more than the
//     worker pool, so 64 runs are executing at kill time and 16 are still
//     queued; recovery reads `running` rows with runs.ListByStatusAll, whose
//     predecessor List defaulted to 50.
//   - crashHarnessWorkers and the manifest's concurrency are both 64, so every
//     claimable run is executing rather than waiting.
//   - crashHarnessHold is long enough that no child can finish before the kill,
//     so every interrupted run is genuinely mid-execution.
const (
	crashHarnessRuns    = 80
	crashHarnessWorkers = 64
	crashHarnessHold    = 90 // seconds
	crashHarnessMinLive = 51 // strictly more than one listing page
)

// Environment variables that dispatch the re-executed test binary into the
// helper that runs the real daemon. They are prefixed like the other test
// seams in this repository.
const (
	crashHelperEnv  = "OTTER_TEST_CRASH_HELPER"
	crashJobsEnv    = "OTTER_TEST_CRASH_JOBS"
	crashDataEnv    = "OTTER_TEST_CRASH_DATA"
	crashListenEnv  = "OTTER_TEST_CRASH_LISTEN"
	crashWorkersEnv = "OTTER_TEST_CRASH_WORKERS"
	crashLogEnv     = "OTTER_TEST_CRASH_LOG"
	crashReadyEnv   = "OTTER_TEST_CRASH_READY"
)

// TestMain adds the re-exec dispatch the crash harness needs. The repository's
// established pattern for killing a real process is to re-run the test binary
// as the process under test (internal/executor/proc_linux_test.go:32); package
// daemon had no TestMain before this file.
func TestMain(m *testing.M) {
	if os.Getenv(crashHelperEnv) == "1" {
		crashHelperMain()
		return
	}
	os.Exit(m.Run())
}

// crashHelperMain runs inside the re-executed test binary. It is the real
// daemon: daemon.New performs discovery, migration and crash recovery exactly
// as `otterd` does, and daemon.Run serves the API and drives the workers.
//
// It must not return on its own. The harness SIGKILLs it, so a normal exit
// would mean the daemon stopped for a reason the test did not cause.
func crashHelperMain() {
	logPath := os.Getenv(crashLogEnv)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crash helper: open log %s: %v\n", logPath, err)
		os.Exit(2)
	}

	workers, err := strconv.Atoi(os.Getenv(crashWorkersEnv))
	if err != nil || workers < 1 {
		fmt.Fprintf(os.Stderr, "crash helper: invalid workers %q: %v\n", os.Getenv(crashWorkersEnv), err)
		os.Exit(2)
	}

	cfg := config.DefaultDaemonConfig("crash-harness")
	cfg.JobsDir = os.Getenv(crashJobsEnv)
	cfg.DataDir = os.Getenv(crashDataEnv)
	cfg.Listen = os.Getenv(crashListenEnv)
	cfg.Workers = workers
	cfg.LogLevel = "debug"
	// The harness kills this process; a shutdown grace period would only make
	// the graceful path reachable, which is not what is under test.
	cfg.ShutdownGrace = 0

	readyPath := os.Getenv(crashReadyEnv)
	d, err := New(context.Background(), Options{
		Config:  cfg,
		Logger:  logging.New(logFile, logging.FormatJSON, logging.LevelDebug),
		Version: "crash-harness",
		OnReady: func(addr string) {
			if err := os.WriteFile(readyPath, []byte(addr+"\n"), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "crash helper: write ready file %s: %v\n", readyPath, err)
			}
		},
	})
	if err != nil {
		// Startup refused. The harness reports this as the daemon dying before
		// it was ready, with this line in the log file.
		fmt.Fprintf(os.Stderr, "crash helper: daemon.New: %v\n", err)
		os.Exit(3)
	}

	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "crash helper: daemon.Run: %v\n", err)
		os.Exit(4)
	}
	os.Exit(5)
}

// crashHarness owns the temporary directories and paths one harness run uses.
type crashHarness struct {
	root     string // jobs root, scanned by the daemon
	dataDir  string // the daemon's data directory, shared across the restart
	stateDir string // side effects and logs, deliberately outside both

	jobID string

	holdFile   string
	startFile  string
	effectFile string

	log1   string
	ready1 string
	log2   string
	ready2 string

	addr string
}

// crashDaemon is a helper process plus the goroutine that reaps it. Wait is
// idempotent so the test can kill, wait and still let cleanup run.
type crashDaemon struct {
	cmd    *exec.Cmd
	exited chan struct{}

	mu  sync.Mutex
	err error
}

func startCrashDaemon(cmd *exec.Cmd) *crashDaemon {
	d := &crashDaemon{cmd: cmd, exited: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		d.mu.Lock()
		d.err = err
		d.mu.Unlock()
		close(d.exited)
	}()
	return d
}

// wait blocks until the process has exited and returns its Wait error.
func (d *crashDaemon) wait() error {
	<-d.exited
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// kill SIGKILLs the process and reaps it. This is the abrupt death the harness
// is about: no SIGTERM handler, no graceful shutdown, no chance to terminalise
// anything.
func (d *crashDaemon) kill() {
	_ = d.cmd.Process.Kill()
	_ = d.wait()
}

// newCrashHarness lays out a job whose child reports that it started, holds
// for crashHarnessHold seconds, and only then records a completion side effect.
// Writing the completion last is what makes a duplicated orphan observable:
// a killed attempt writes nothing, so exactly one completion per accepted run
// means no orphan finished the work behind its retry's back.
func newCrashHarness(t *testing.T) *crashHarness {
	t.Helper()

	h := &crashHarness{
		root:     t.TempDir(),
		dataDir:  t.TempDir(),
		stateDir: t.TempDir(),
		addr:     crashFreeAddr(t),
	}
	h.holdFile = filepath.Join(h.stateDir, "hold_seconds")
	h.startFile = filepath.Join(h.stateDir, "started.log")
	h.effectFile = filepath.Join(h.stateDir, "effects.log")
	h.log1 = filepath.Join(h.stateDir, "daemon-1.log")
	h.ready1 = filepath.Join(h.stateDir, "daemon-1.ready")
	h.log2 = filepath.Join(h.stateDir, "daemon-2.log")
	h.ready2 = filepath.Join(h.stateDir, "daemon-2.ready")

	if err := os.WriteFile(h.holdFile, []byte(strconv.Itoa(crashHarnessHold)), 0o644); err != nil {
		t.Fatalf("write hold file: %v", err)
	}

	writeJob(t, h.root, "crashjob", crashManifest(crashHarnessWorkers),
		crashJobScript(h.startFile, h.holdFile, h.effectFile))

	// A run executes an active release, not the source tree, so every job must
	// be released before the daemon can accept a submission for it.
	releaseAll(t, h.root, h.dataDir)
	h.jobID = identityIDFor(t, h.root, h.dataDir, "crashjob")

	// Whatever the harness leaves behind, it must not leave a sleeping Python
	// child on a developer's machine. The runtime guarantees the child's death
	// only on Linux; this cleanup is the harness's own hygiene, not evidence
	// about the runtime, and it runs after the assertions.
	t.Cleanup(func() {
		for _, pid := range crashReadPIDs(h.startFile) {
			_ = crashKillProcess(pid)
		}
	})
	return h
}

func crashManifest(concurrency int) string {
	return fmt.Sprintf(`version: 1
name: crashjob
entrypoint: main.py
timeout: 300
concurrency: %d
retry:
  attempts: 3
  backoff: none
`, concurrency)
}

// crashJobScript is the payload whose external effect the harness checks. It
// reads the run id from the environment the executor sets, not through the
// Python SDK, so a failure of the job is unambiguously about the runtime.
func crashJobScript(startFile, holdFile, effectFile string) string {
	const script = `import os
import time

START = __START__
HOLD = __HOLD__
EFFECT = __EFFECT__

run_id = os.environ.get("OTTER_RUN_ID", "unknown")
pid = os.getpid()

with open(START, "a", encoding="utf-8") as handle:
    handle.write(run_id + " " + str(pid) + "\n")
    handle.flush()
    os.fsync(handle.fileno())

seconds = 0.0
try:
    with open(HOLD, "r", encoding="utf-8") as handle:
        seconds = float(handle.read().strip() or "0")
except (OSError, ValueError):
    seconds = 0.0
if seconds > 0:
    time.sleep(seconds)

with open(EFFECT, "a", encoding="utf-8") as handle:
    handle.write(run_id + "\n")
    handle.flush()
    os.fsync(handle.fileno())
`
	return strings.NewReplacer(
		"__START__", strconv.Quote(startFile),
		"__HOLD__", strconv.Quote(holdFile),
		"__EFFECT__", strconv.Quote(effectFile),
	).Replace(script)
}

// start launches the helper as a separate process and waits until its API
// listener is bound. Readiness is the daemon's own OnReady callback, so it
// proves daemon.New -- including recovery -- returned and the server started.
func (h *crashHarness) start(t *testing.T, n int) *crashDaemon {
	t.Helper()

	logPath, readyPath := h.log1, h.ready1
	if n == 2 {
		logPath, readyPath = h.log2, h.ready2
	}
	_ = os.Remove(readyPath)

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		crashHelperEnv+"=1",
		crashJobsEnv+"="+h.root,
		crashDataEnv+"="+h.dataDir,
		crashListenEnv+"="+h.addr,
		crashWorkersEnv+"="+strconv.Itoa(crashHarnessWorkers),
		crashLogEnv+"="+logPath,
		crashReadyEnv+"="+readyPath,
	)
	// The helper's stderr carries New/Run failures; keep it visible in the
	// test's own output rather than discarding it.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start crash helper %d: %v", n, err)
	}

	d := startCrashDaemon(cmd)
	t.Cleanup(d.kill)

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return d
		}
		select {
		case <-d.exited:
			t.Fatalf("crash helper %d exited before it was ready (wait: %v); log:\n%s",
				n, d.wait(), crashReadFile(t, logPath))
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("crash helper %d was not ready within 90s; log:\n%s", n, crashReadFile(t, logPath))
	return nil
}

// TestCrashHarnessSIGKILLAndRecovery is P0-01's deliverable 1-6 and the real
// evidence behind runtime-contract.md Appendix A FM-01.
func TestCrashHarnessSIGKILLAndRecovery(t *testing.T) {
	requirePython(t)

	h := newCrashHarness(t)
	store, closeStore := crashOpenStore(t, h.dataDir)
	defer closeStore()

	// (1) Boot the real daemon against the temporary data directory.
	first := h.start(t, 1)
	client := api.NewClient("http://"+h.addr, "")

	// (2) Submit more than 50 runs, to cross the listing-page boundary the
	// simulated test uses.
	accepted := make([]string, 0, crashHarnessRuns)
	for i := 0; i < crashHarnessRuns; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		runID, err := client.SubmitRun(ctx, "crashjob", nil)
		cancel()
		if err != nil {
			t.Fatalf("submit run %d/%d: %v", i+1, crashHarnessRuns, err)
		}
		accepted = append(accepted, runID)
	}
	t.Logf("accepted %d runs against the real daemon (pid %d)", len(accepted), first.cmd.Process.Pid)

	// (3) Wait until they are genuinely running: every child writes its run id
	// and pid before it holds. A count read from the database alone could be a
	// status set a moment before the process existed; this proves the processes
	// are alive and executing the job.
	crashWaitForStartLines(t, h.startFile, crashHarnessMinLive, 3*time.Minute)
	started := crashReadPIDs(h.startFile)
	if len(started) <= 50 {
		t.Fatalf("only %d children reported running, want more than one listing page (50)", len(started))
	}

	runningBefore := crashRunsInStatus(t, store, h.jobID, runs.StatusRunning)
	if len(runningBefore) <= 50 {
		t.Fatalf("only %d runs have status running, want >50 so recovery must cross a page boundary", len(runningBefore))
	}
	t.Logf("%d children genuinely running; %d rows in status running", len(started), len(runningBefore))

	// (4) kill -9 the daemon process.
	first.kill()
	t.Logf("SIGKILLed daemon pid %d; log:\n%s", first.cmd.Process.Pid, crashTail(t, h.log1, 8))

	// The interrupted set is the state the restarted daemon must repair. With
	// every child holding, nothing can have finished between the submission
	// burst and the kill, so this is every run that was executing.
	interrupted := crashRunsInStatus(t, store, h.jobID, runs.StatusRunning)
	if len(interrupted) <= 50 {
		t.Fatalf("only %d rows were running at kill time, want >50 to cross the recovery page boundary", len(interrupted))
	}
	interruptedIDs := make([]string, 0, len(interrupted))
	for _, run := range interrupted {
		interruptedIDs = append(interruptedIDs, run.ID)
	}

	// Let the retries finish quickly. The already-started children read the old
	// value and keep holding; only attempts that start after the restart see 0.
	crashWriteFile(t, h.holdFile, "0")

	// (5) Restart the daemon over the same data directory.
	second := h.start(t, 2)

	// Recovery ran inside daemon.New, before the listener was bound, so the
	// restarted daemon's own log line is the authoritative report of how many
	// interrupted runs it repaired. Comparing it with the database snapshot
	// proves the pass was complete and not truncated at one page.
	recovered := recoveryCount(t, crashReadFile(t, h.log2))
	if recovered != len(interruptedIDs) {
		// Fatal, not Error: if the restarted daemon did not even report
		// repairing every interrupted run, the rest of the harness is testing
		// a premise that is false, and the drain wait below would block on rows
		// nothing is going to repair.
		t.Fatalf("restarted daemon recovered %d interrupted runs, want %d (the >50 page boundary)",
			recovered, len(interruptedIDs))
	}
	t.Logf("restarted daemon (pid %d) reported recovering %d interrupted runs", second.cmd.Process.Pid, recovered)

	// (6a) Wait until every run has settled. Nothing is allowed to remain
	// queued, running or retrying once the restarted workers have drained.
	crashWaitForDrain(t, store, h.jobID, second, 5*time.Minute)
	second.kill()

	all := crashListRuns(t, store, h.jobID)
	rowsByID := make(map[string]*runs.Run, len(all))
	for _, run := range all {
		rowsByID[run.ID] = run
	}

	// One row per accepted run, plus exactly one successor per interrupted run.
	// A recovery that enqueued an attempt twice, or resurrected a terminal row,
	// would show up here as extra rows.
	if want := len(accepted) + len(interruptedIDs); len(all) != want {
		t.Errorf("database holds %d run rows, want %d (%d accepted + %d interrupted successors)",
			len(all), want, len(accepted), len(interruptedIDs))
	}

	// (6b) None lost: every accepted run still exists and its chain settled.
	chains := make(map[string][]*runs.Run, len(accepted))
	succeeded := map[string]bool{}
	for _, rootID := range accepted {
		root, attempts, err := store.Chain(context.Background(), rootID)
		if err != nil {
			t.Fatalf("chain for accepted run %s: %v", rootID, err)
		}
		if root.ID != rootID {
			t.Errorf("chain for accepted run %s starts at %s", rootID, root.ID)
		}
		chains[rootID] = attempts
		for _, attempt := range attempts {
			if attempt.Status.Terminal() {
				continue
			}
			t.Errorf("accepted run %s left attempt %s non-terminal (%s)", rootID, attempt.ID, attempt.Status)
		}
		last := attempts[len(attempts)-1]
		if last.Status != runs.StatusSucceeded {
			t.Errorf("accepted run %s ended %s (%s), want succeeded", rootID, last.Status, last.ErrorString())
			continue
		}
		succeeded[last.ID] = true
	}
	if got := len(succeeded); got != len(accepted) {
		t.Errorf("%d accepted runs reached succeeded, want %d", got, len(accepted))
	}
	for _, run := range all {
		if !run.Status.Terminal() {
			t.Errorf("run %s is %s after the restarted daemon drained, want terminal", run.ID, run.Status)
		}
	}

	// (6c) None executed twice -- first at the attempt level. Every run id that
	// appears in the effect file must be a run that exists, and no run id may
	// appear twice: a terminal attempt must never be resurrected.
	effects := crashCountLines(t, h.effectFile)
	for id, count := range effects {
		if _, ok := rowsByID[id]; !ok {
			t.Errorf("effect file names run %s, which does not exist in the database", id)
		}
		if count != 1 {
			t.Errorf("run %s executed %d times (effect file has %d lines), want exactly one execution per attempt", id, count, count)
		}
	}
	for id := range succeeded {
		if effects[id] == 0 {
			t.Errorf("run %s succeeded but wrote no completion side effect", id)
		}
	}

	// (6d) Every run that was running at kill time was re-enqueued: recovery
	// terminalised it and created a successor attempt.
	interruptedSet := map[string]bool{}
	for _, id := range interruptedIDs {
		interruptedSet[id] = true
	}
	for _, id := range interruptedIDs {
		run := rowsByID[id]
		if run == nil {
			t.Errorf("interrupted run %s disappeared across the restart", id)
			continue
		}
		if run.Status != runs.StatusFailed {
			t.Errorf("interrupted run %s is %s, want failed", id, run.Status)
		}
		if !strings.Contains(run.ErrorString(), crashMessage) {
			t.Errorf("interrupted run %s error = %q, want it to name the restart", id, run.ErrorString())
		}
		if len(chains[id]) < 2 {
			t.Errorf("interrupted run %s has no retry successor (%d attempts); the work was lost", id, len(chains[id]))
		}
	}
	// Runs that were merely queued at kill time are not interrupted, so they
	// must have executed exactly once with no successor.
	for _, id := range accepted {
		if interruptedSet[id] {
			continue
		}
		if len(chains[id]) != 1 {
			t.Errorf("run %s was queued at kill time but has %d attempts, want 1", id, len(chains[id]))
		}
	}
	t.Logf("all %d accepted runs terminal across %d rows; %d interrupted runs re-enqueued; %d distinct completions recorded",
		len(accepted), len(all), len(interruptedIDs), len(effects))

	// (7) Fail-closed startup on incomplete recovery is TestStartupFailClosedOnIncompleteRecovery
	// (daemon_test.go:1223). This harness asserts the complementary positive
	// case: recovery completed, so New returned and the API bound.

	// Platform-specific half. On Linux this proves the killed children died and
	// no orphan completed work behind its retry; elsewhere it records why that
	// cannot be asserted.
	crashAssertPlatform(t, &crashPlatformEvidence{
		accepted:    accepted,
		interrupted: interruptedIDs,
		chains:      chains,
		effects:     effects,
		pids:        started,
	})
}

// crashPlatformEvidence is what the platform-gated checks need after the main
// assertions have run.
type crashPlatformEvidence struct {
	accepted    []string
	interrupted []string
	chains      map[string][]*runs.Run
	effects     map[string]int
	pids        map[string]int
}

// ---------------------------------------------------------------- utilities

func crashFreeAddr(t *testing.T) string {
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

func crashOpenStore(t *testing.T, dataDir string) (*runs.Store, func()) {
	t.Helper()
	db, err := database.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("open data directory %s: %v", dataDir, err)
	}
	return runs.NewStore(db.DB), func() { _ = db.Close() }
}

func crashRunsInStatus(t *testing.T, store *runs.Store, jobID string, status runs.Status) []*runs.Run {
	t.Helper()
	list, err := store.ListByStatusAll(context.Background(), status)
	if err != nil {
		t.Fatalf("list %s runs: %v", status, err)
	}
	out := make([]*runs.Run, 0, len(list))
	for _, run := range list {
		if run.JobID == jobID {
			out = append(out, run)
		}
	}
	return out
}

func crashListRuns(t *testing.T, store *runs.Store, jobID string) []*runs.Run {
	t.Helper()
	// 1000 is maxListLimit. The harness creates a few hundred rows at most, so
	// this is exhaustive; anything larger would be silently truncated by List.
	list, err := store.List(context.Background(), runs.Filter{JobID: jobID, Limit: 1000})
	if err != nil {
		t.Fatalf("list all runs: %v", err)
	}
	return list
}

// crashWaitForStartLines polls the start file until it holds at least want
// lines. It reads file state, never a sleep, so a slow machine only makes it
// wait longer rather than making it flake.
func crashWaitForStartLines(t *testing.T, path string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if lines := crashReadLines(t, path); len(lines) >= want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("only %d of %d children started within %s; start file:\n%s",
		len(crashReadLines(t, path)), want, timeout, crashReadFile(t, path))
}

// crashWaitForDrain polls until no run of the job is queued, running or
// retrying. It watches the restarted helper as well, so a daemon that dies
// during recovery fails the test with its log instead of hanging.
func crashWaitForDrain(t *testing.T, store *runs.Store, jobID string, daemon *crashDaemon, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-daemon.exited:
			t.Fatalf("restarted daemon exited before the backlog drained (wait: %v)", daemon.wait())
		default:
		}
		pending := 0
		for _, status := range []runs.Status{runs.StatusQueued, runs.StatusRunning, runs.StatusRetrying} {
			pending += len(crashRunsInStatus(t, store, jobID, status))
		}
		if pending == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("runs were still queued/running/retrying after %s", timeout)
}

// crashReadPIDs parses the start file into run id -> pid.
func crashReadPIDs(path string) map[string]int {
	out := map[string]int{}
	for _, line := range crashLines(path) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || pid <= 0 {
			continue
		}
		out[fields[0]] = pid
	}
	return out
}

// crashCountLines counts how many times each value appears in a file. It is how
// the harness reads the job's external side effect.
func crashCountLines(t *testing.T, path string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, line := range crashLines(path) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out[line]++
	}
	return out
}

func crashReadLines(t *testing.T, path string) []string {
	t.Helper()
	return crashLines(path)
}

func crashLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func crashReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(cannot read %s: %v)", path, err)
	}
	return string(data)
}

// crashTail returns the last n lines of a file, for the compact evidence lines
// the harness logs.
func crashTail(t *testing.T, path string, n int) string {
	t.Helper()
	lines := crashLines(path)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func crashWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// crashKillProcess SIGKILLs a pid, best effort. It is used only for the
// harness's own cleanup of children the runtime is not obliged to kill on this
// platform.
func crashKillProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
