// Package executor runs an integration as a separate child process.
//
// Arbitrary Python never runs inside the daemon: each run gets its own
// process so a crash, a memory leak or a timeout can be contained and
// reported. The executor captures stdout/stderr line by line, records the
// exit code, enforces the manifest timeout and supports cancellation.
package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/runs"
)

// defaultTerminateGrace is how long a process gets to exit after SIGTERM
// before it is killed outright.
const defaultTerminateGrace = 5 * time.Second

// maxLogLine bounds a single captured line so a runaway process cannot write
// unbounded rows into SQLite.
const maxLogLine = 64 * 1024

// LogSink receives captured output.
type LogSink interface {
	Line(stream string, at time.Time, message string)
}

// LogSinkFunc adapts a function to LogSink.
type LogSinkFunc func(stream string, at time.Time, message string)

// Line implements LogSink.
func (f LogSinkFunc) Line(stream string, at time.Time, message string) { f(stream, at, message) }

// Request describes one execution.
type Request struct {
	Manifest *config.Manifest

	// IntegrationID is the durable identity the child addresses its state and
	// logs with. IntegrationName is the manifest label. They are separate
	// values on purpose: the label may change or be shared, the identity may
	// not, and neither is read back out of the manifest here.
	IntegrationID   string
	IntegrationName string

	Executable     string
	Managed        bool
	RunID          string
	TriggerType    string
	APIURL         string
	StateToken     string
	ExtraEnv       map[string]string
	Timeout        time.Duration
	TerminateGrace time.Duration

	// CapturePolicy is the resolved HTTP capture policy for the run: off,
	// metadata or full. It is resolved once at submission and passed through
	// unchanged, so a child cannot widen its own capture.
	CapturePolicy string
}

// Result is the outcome of an execution.
type Result struct {
	StartedAt  time.Time
	FinishedAt time.Time

	// ExitCode is the process exit status, or nil when the process was
	// terminated by a signal.
	ExitCode *int

	// Signal names the signal that killed the process, when applicable.
	Signal string

	TimedOut  bool
	Cancelled bool

	// StartError is set when the process could not be launched at all. These
	// are configuration failures and are never retried.
	StartError error

	// WaitError is the raw error from cmd.Wait, kept for diagnostics.
	WaitError error
}

// Succeeded reports whether the process ran and exited zero.
func (r *Result) Succeeded() bool {
	return r.StartError == nil && !r.TimedOut && !r.Cancelled &&
		r.ExitCode != nil && *r.ExitCode == 0
}

// Executor launches integration processes.
type Executor struct {
	logger *logging.Logger

	// SDKPath is prepended to the child PYTHONPATH so that `import otter`
	// works without any installation step.
	SDKPath string
}

// New creates an executor.
func New(logger *logging.Logger, sdkPath string) *Executor {
	return &Executor{logger: logger, SDKPath: sdkPath}
}

// Run executes the integration and blocks until it finishes, times out or is
// cancelled. It always returns a Result; failures are described by the
// Result rather than by an error return.
func (e *Executor) Run(ctx context.Context, req *Request, sink LogSink) *Result {
	res := &Result{}
	if req == nil || req.Manifest == nil {
		res.StartError = errors.New("executor: request has no manifest")
		return res
	}
	m := req.Manifest

	env, err := e.buildEnv(req)
	if err != nil {
		res.StartError = err
		return res
	}

	interpreter := m.Python.Executable
	if req.Executable != "" {
		interpreter = req.Executable
	}
	argv := []string{m.Entrypoint}
	if _, err := os.Stat(filepath.Join(e.SDKPath, "otter", "_launcher.py")); err == nil {
		argv = []string{"-m", "otter._launcher", m.Entrypoint}
	}
	cmd := exec.Command(interpreter, argv...)
	cmd.Dir = m.Dir
	cmd.Env = env
	setProcessGroup(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		res.StartError = fmt.Errorf("executor: open stdout pipe: %w", err)
		return res
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		res.StartError = fmt.Errorf("executor: open stderr pipe: %w", err)
		return res
	}

	if err := cmd.Start(); err != nil {
		res.StartError = startError(interpreter, m, err)
		return res
	}
	res.StartedAt = time.Now().UTC()

	grace := req.TerminateGrace
	if grace <= 0 {
		grace = defaultTerminateGrace
	}
	// If the process exits but a descendant keeps the pipes open, Wait would
	// block forever without this.
	cmd.WaitDelay = grace + 5*time.Second

	var wg sync.WaitGroup
	wg.Add(2)
	go streamLines(stdoutPipe, runs.StreamStdout, sink, &wg)
	go streamLines(stderrPipe, runs.StreamStderr, sink, &wg)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var (
		werr      error
		timedOut  bool
		cancelled bool
	)

	var timeoutCh <-chan time.Time
	if req.Timeout > 0 {
		timer := time.NewTimer(req.Timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case werr = <-done:
	case <-timeoutCh:
		timedOut = true
		e.logger.Warn("run_timeout",
			"integration", m.Name, "run_id", req.RunID, "timeout", req.Timeout.String())
		werr = e.terminate(cmd, done, grace)
	case <-ctx.Done():
		cancelled = true
		e.logger.Info("run_cancelled",
			"integration", m.Name, "run_id", req.RunID)
		werr = e.terminate(cmd, done, grace)
	}

	// Give the readers a moment to drain the pipes so no output is lost.
	streamsDone := make(chan struct{})
	go func() { wg.Wait(); close(streamsDone) }()
	select {
	case <-streamsDone:
	case <-time.After(5 * time.Second):
		e.logger.Warn("run_log_drain_timeout", "integration", m.Name, "run_id", req.RunID)
	}

	res.FinishedAt = time.Now().UTC()
	res.TimedOut = timedOut
	res.Cancelled = cancelled
	res.WaitError = werr
	res.ExitCode, res.Signal = processExit(werr)

	return res
}

// terminate sends SIGTERM to the process group, waits for the grace period and
// escalates to SIGKILL. It returns the result of cmd.Wait.
func (e *Executor) terminate(cmd *exec.Cmd, done <-chan error, grace time.Duration) error {
	if err := terminateGroup(cmd); err != nil {
		// The process is already gone; fall through and collect it.
		e.logger.Debug("run_terminate_noop", "error", err.Error())
	}

	select {
	case err := <-done:
		return err
	case <-time.After(grace):
	}

	e.logger.Warn("run_kill", "pid", cmd.Process.Pid, "grace", grace.String())
	if err := killGroup(cmd); err != nil {
		e.logger.Debug("run_kill_noop", "error", err.Error())
	}
	return <-done
}

// startError explains a failure to launch, adding the likely cause when the
// interpreter is simply not there.
//
// The runtime never requires Python on the host for a managed integration, so
// "python3: executable file not found" is the expected first failure on a fresh
// server -- and the fix is to opt into managed mode, not to install Python.
func startError(interpreter string, m *config.Manifest, err error) error {
	if !errors.Is(err, exec.ErrNotFound) && !strings.Contains(err.Error(), "executable file not found") {
		return fmt.Errorf("executor: start %s %s: %w", interpreter, m.Entrypoint, err)
	}
	return fmt.Errorf("executor: start %s %s: %w\n"+
		"hint: %s is not available on this host. Set `python.mode: managed` in otter.yaml to "+
		"run on an interpreter Otter prepares, or install %s",
		interpreter, m.Entrypoint, err, interpreter, interpreter)
}

// buildEnv assembles the child environment.
//
// Inherited OTTER_* variables are removed first: the daemon's own API token
// (OTTER_API_TOKEN) must never leak into integration code. The child receives
// only the scoped, per-run values set below.
func (e *Executor) buildEnv(req *Request) ([]string, error) {
	m := req.Manifest

	inherited := make([]string, 0, len(os.Environ())+16)
	pythonPath := ""
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "OTTER_") {
			continue
		}
		if req.Managed && (key == "NO_PROXY" || key == "no_proxy") {
			continue
		}
		if req.Managed && !managedHostVariable(key) {
			continue
		}
		if key == "PYTHONPATH" {
			if !req.Managed {
				pythonPath = value
			}
			continue
		}
		if req.Managed && (key == "PYTHONHOME" || key == "PYTHONUSERBASE" || key == "VIRTUAL_ENV") {
			continue
		}
		inherited = append(inherited, kv)
	}
	if req.Managed {
		// Tools launched by the integration should discover the matching venv.
		inherited = append(inherited, "PATH="+filepath.Dir(req.Executable)+string(os.PathListSeparator)+os.Getenv("PATH"))
		noProxy := strings.Trim(strings.Join([]string{os.Getenv("NO_PROXY"), os.Getenv("no_proxy"), "127.0.0.1", "localhost", "::1"}, ","), ",")
		inherited = append(inherited, "NO_PROXY="+noProxy, "no_proxy="+noProxy)
	}

	// PYTHONPATH order matters: the runtime SDK first so `import otter` always
	// resolves to the daemon's own version, then any shared code the manifest
	// declares, then whatever the operator already had.
	searchPath := make([]string, 0, len(m.Python.Path)+2)
	if e.SDKPath != "" {
		searchPath = append(searchPath, e.SDKPath)
	}
	searchPath = append(searchPath, m.PythonPaths()...)
	if pythonPath != "" {
		searchPath = append(searchPath, pythonPath)
	}
	if len(searchPath) > 0 {
		inherited = append(inherited, "PYTHONPATH="+strings.Join(searchPath, string(os.PathListSeparator)))
	}

	inherited = append(inherited,
		"OTTER_INTEGRATION_ID="+req.IntegrationID,
		"OTTER_INTEGRATION_NAME="+req.IntegrationName,
		"OTTER_RUN_ID="+req.RunID,
		"OTTER_API_URL="+req.APIURL,
		"OTTER_TRIGGER_TYPE="+req.TriggerType,
		"OTTER_INTEGRATION_DIR="+m.Dir,
		// Unbuffered output means log lines arrive as they are written
		// instead of being held in Python's stdout buffer until exit.
		"PYTHONUNBUFFERED=1",
		// Keep integration directories free of __pycache__, which matters
		// when they are mounted read-only.
		"PYTHONDONTWRITEBYTECODE=1",
	)
	// Capture is off unless a policy is set, so a run that did not ask for it
	// installs no instrumentation at all.
	if req.CapturePolicy != "" {
		inherited = append(inherited, "OTTER_CAPTURE_POLICY="+req.CapturePolicy)
	}
	if req.Managed {
		inherited = append(inherited, "PYTHONNOUSERSITE=1")
	}
	if req.StateToken != "" {
		inherited = append(inherited, "OTTER_STATE_TOKEN="+req.StateToken)
	}

	// Manifest env may reference either the daemon environment or a resolved
	// secret, so expansion sees both.
	lookup := func(key string) string {
		if req.ExtraEnv != nil {
			if v, ok := req.ExtraEnv[key]; ok {
				return v
			}
		}
		return os.Getenv(key)
	}
	for _, key := range sortedKeys(m.Env) {
		inherited = append(inherited, key+"="+expand(m.Env[key], lookup))
	}

	for _, key := range sortedKeys(req.ExtraEnv) {
		inherited = append(inherited, key+"="+req.ExtraEnv[key])
	}

	return inherited, nil
}

func managedHostVariable(key string) bool {
	switch key {
	case "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE":
		return true

	// Operational knobs an integration reads from its own environment.
	//
	// The managed environment is deliberately narrow: the daemon's world does
	// not become the child's, which is what keeps credentials from leaking
	// sideways. But the same narrowness silently swallowed the tunables an
	// operator sets per deployment -- a dry run quietly performed real writes,
	// because DRY_RUN never reached the integration.
	//
	// These are listed explicitly rather than by prefix so the set stays
	// auditable. `env:` in the manifest is still the first choice; this only
	// makes an operator's environment able to reach an integration that reads
	// a documented knob.
	case "DRY_RUN", "PAGE_SIZE", "MAX_PAGES_PER_RUN", "RUN_BUDGET_SECONDS",
		"OVERLAP_SECONDS", "SALESFORCE_BATCH_SIZE", "SYNC_ADDRESS",
		"SHOPIFY_SORT_KEY":
		return true

	default:
		return false
	}
}

// expand resolves ${VAR} and $VAR references using lookup.
func expand(value string, lookup func(string) string) string {
	if !strings.Contains(value, "$") {
		return value
	}
	return os.Expand(value, lookup)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Insertion sort keeps this dependency-free and the maps are tiny.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// streamLines forwards a pipe to the sink one line at a time, bounding memory
// even if a process emits a line far larger than the buffer.
func streamLines(r io.Reader, stream string, sink LogSink, wg *sync.WaitGroup) {
	defer wg.Done()
	if sink == nil {
		_, _ = io.Copy(io.Discard, r)
		return
	}

	reader := bufio.NewReaderSize(r, 32*1024)
	var (
		buf       []byte
		truncated bool
	)

	flush := func() {
		if len(buf) == 0 && !truncated {
			return
		}
		message := strings.TrimRight(string(buf), "\r\n")
		if truncated {
			message += " …(truncated)"
		}
		if strings.TrimSpace(message) != "" {
			sink.Line(stream, time.Now().UTC(), message)
		}
		buf = buf[:0]
		truncated = false
	}

	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 && !truncated {
			if len(buf)+len(chunk) > maxLogLine {
				remaining := maxLogLine - len(buf)
				if remaining > 0 {
					buf = append(buf, chunk[:remaining]...)
				}
				truncated = true
			} else {
				buf = append(buf, chunk...)
			}
		}

		switch {
		case err == nil:
			flush()
		case errors.Is(err, bufio.ErrBufferFull):
			// Keep consuming; the line simply continues.
		default:
			flush()
			return
		}
	}
}
