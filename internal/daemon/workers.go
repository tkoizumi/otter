package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/otter-runtime/otter/internal/config"
	"github.com/otter-runtime/otter/internal/executor"
	"github.com/otter-runtime/otter/internal/logging"
	"github.com/otter-runtime/otter/internal/pyenv"
	"github.com/otter-runtime/otter/internal/queue"
	"github.com/otter-runtime/otter/internal/retry"
	"github.com/otter-runtime/otter/internal/runs"
	"github.com/otter-runtime/otter/internal/secrets"
)

// workerIdlePoll is how often an idle worker re-checks the queue. Retries sit
// in the queue with a future available_at, so a periodic sweep is what makes
// them fire on time.
const workerIdlePoll = 500 * time.Millisecond

// startWorkers launches the worker pool. The pool size is the global
// concurrency limit.
func (d *Daemon) startWorkers() {
	for i := 0; i < d.cfg.Workers; i++ {
		d.wg.Add(1)
		go d.worker(i)
	}
}

// worker repeatedly claims and executes runs until the daemon drains.
func (d *Daemon) worker(id int) {
	defer d.wg.Done()

	ticker := time.NewTicker(workerIdlePoll)
	defer ticker.Stop()

	for {
		// Drain everything currently claimable before sleeping.
		for {
			if d.draining.Load() {
				return
			}

			// Claim reserves a concurrency slot; the executor releases it.
			item, err := d.queue.Claim(context.Background(), time.Now().UTC(), d.cap)
			if err != nil {
				if !errors.Is(err, queue.ErrEmpty) {
					d.log.Error("queue_claim_failed", err, "worker", id)
				}
				break
			}
			d.executeRun(item)
		}

		select {
		case <-d.stopCh:
			return
		case <-d.wakeCh:
		case <-ticker.C:
		}
	}
}

// executeRun performs one attempt and records its outcome. The concurrency
// slot reserved by Claim is released exactly once, on return.
func (d *Daemon) executeRun(item *queue.Item) {
	defer d.cap.Release(item.IntegrationID)

	loadCtx, cancelLoad := context.WithTimeout(context.Background(), 30*time.Second)
	run, err := d.runs.Get(loadCtx, item.RunID)
	cancelLoad()
	if err != nil {
		d.log.Error("run_load_failed", err, "run_id", item.RunID, "integration", item.IntegrationID)
		return
	}

	entry, ok := d.reg.get(run.IntegrationID)
	if !ok || entry.Manifest == nil {
		d.finishRun(run, nil, runs.Finish{
			Status: runs.StatusFailed,
			Error:  "integration is no longer available; the manifest may have been removed",
		}, false)
		return
	}
	m := entry.Manifest

	// A bound run executes its recorded snapshot, not the live source tree.
	//
	// The manifest is re-read from the snapshot rather than re-pointed at it:
	// relative declarations in it -- python.path above all -- are resolved
	// against the integration directory, and the snapshot mirrors the checkout
	// layout precisely so those declarations keep resolving inside the release.
	// Reusing the live manifest would import the live shared code instead of
	// the code the release captured.
	if run.ReleaseSourceDir != "" {
		manifestPath := filepath.Join(run.ReleaseSourceDir, config.ManifestFileName)
		if _, err := os.Stat(manifestPath); err != nil {
			d.finishRun(run, m, runs.Finish{
				Status: runs.StatusFailed,
				Error: fmt.Sprintf("release %s is no longer available at %s; it may have been removed by release retention",
					shortDigest(run.ReleaseDigest), run.ReleaseSourceDir),
			}, false)
			return
		}
		bound, err := config.LoadAndValidate(manifestPath)
		if err != nil {
			d.finishRun(run, m, runs.Finish{
				Status: runs.StatusFailed,
				Error:  fmt.Sprintf("release %s is invalid: %v", shortDigest(run.ReleaseDigest), err),
			}, false)
			return
		}
		m = bound
	}

	interpreter := ""
	if m.Python.Mode == "managed" {
		manager := pyenv.Manager{DataDir: d.cfg.DataDir}
		// Rebuild the identity this run recorded. The policy is deliberately
		// not re-derived: a retry must resolve the environment its parent
		// selected, even if the toolchain that prepared it has changed since.
		spec := pyenv.RecordedIdentity(run.IntegrationID, run.PythonVersion, run.EnvironmentDigest, run.PythonPolicy)
		ready, err := manager.GetReady(spec)
		if err != nil {
			d.finishRun(run, m, runs.Finish{Status: runs.StatusFailed, Error: err.Error()}, false)
			return
		}
		interpreter = ready.Interpreter
	}

	startedAt := time.Now().UTC()
	claimed, err := d.runs.MarkRunning(context.Background(), run.ID, startedAt)
	if err != nil {
		d.log.Error("run_mark_running_failed", err, "run_id", run.ID)
		return
	}
	if !claimed {
		// Cancelled while it sat in the queue, or already claimed elsewhere.
		d.log.Debug("run_not_claimable", "run_id", run.ID, "status", string(run.Status))
		return
	}
	run.Status = runs.StatusRunning
	run.StartedAt = &startedAt

	token, err := d.runTokens.Issue(run.ID, run.IntegrationID)
	if err != nil {
		d.finishRun(run, m, runs.Finish{Status: runs.StatusFailed, Error: err.Error()}, false)
		return
	}
	defer d.runTokens.Revoke(token)

	// The run context is derived from the daemon's base context, not from the
	// signal context: a SIGTERM must trigger the graceful path below instead
	// of killing children instantly.
	runCtx, cancelRun := context.WithCancel(d.baseCtx)
	ctl := &runControl{cancel: cancelRun, done: make(chan struct{})}

	d.runCtlMu.Lock()
	d.runCtl[run.ID] = ctl
	d.runCtlMu.Unlock()

	defer func() {
		d.runCtlMu.Lock()
		delete(d.runCtl, run.ID)
		d.runCtlMu.Unlock()
		cancelRun()
		close(ctl.done)
	}()

	d.log.Info("run_started",
		"integration", m.Name,
		"run_id", run.ID,
		"attempt", run.Attempt,
		"trigger", run.TriggerType)

	d.appendOtterLog(run.ID, fmt.Sprintf("run started (attempt %d of %d, trigger %s)",
		run.Attempt, m.MaxAttempts(), run.TriggerType))

	// Resolve secrets before launching Python so a missing secret is a clear
	// configuration failure rather than a crash inside the integration.
	env, err := secrets.Resolve(context.Background(), d.secrets, m.Name, m.Secrets)
	if err != nil {
		message := err.Error()
		d.appendOtterLog(run.ID, "not started: "+message)
		d.finishRun(run, m, runs.Finish{
			Status:     runs.StatusFailed,
			Error:      message,
			FinishedAt: time.Now().UTC(),
		}, false)
		return
	}

	sink := newRunLogSink(d.logs, run.ID, d.log)
	result := d.exec.Run(runCtx, &executor.Request{
		Manifest:       m,
		Executable:     interpreter,
		Managed:        m.Python.Mode == "managed",
		RunID:          run.ID,
		TriggerType:    run.TriggerType,
		APIURL:         d.cfg.ChildAPIURL(),
		StateToken:     token,
		ExtraEnv:       env,
		Timeout:        m.TimeoutDuration(),
		TerminateGrace: 5 * time.Second,
	}, sink)
	sink.Flush()

	status, message, retryable := classifyOutcome(result, ctl.getReason(), m)
	message = withFailureHint(message, status, sink.FailureHint())
	d.finishRun(run, m, runs.Finish{
		Status:     status,
		ExitCode:   result.ExitCode,
		Error:      message,
		FinishedAt: result.FinishedAt,
	}, retryable)
}

// shortDigest abbreviates a digest for an error message, tolerating an empty
// or already-short value.
func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	if digest == "" {
		return "unknown"
	}
	return digest
}

// withFailureHint folds the integration's own error into the message Otter
// records, so `otter run-status`, the daemon log and the run log all name the
// real cause instead of only the exit code.
func withFailureHint(message string, status runs.Status, hint string) string {
	if status == runs.StatusSucceeded || hint == "" {
		return message
	}
	switch {
	case message == "":
		return hint
	case strings.Contains(message, hint):
		return message
	default:
		return message + ": " + hint
	}
}

// classifyOutcome maps an execution result onto a run status, an error
// message and whether the retry policy applies.
func classifyOutcome(res *executor.Result, reason cancelReason, m *config.Manifest) (runs.Status, string, bool) {
	switch {
	case res.StartError != nil:
		// Configuration failure: the same thing would fail again.
		return runs.StatusFailed, res.StartError.Error(), false
	case reason == reasonUser:
		return runs.StatusCancelled, "cancelled by operator", false
	case res.TimedOut:
		return runs.StatusTimedOut,
			fmt.Sprintf("execution exceeded the %s timeout", m.TimeoutDuration()), true
	case reason == reasonShutdown:
		return runs.StatusFailed, "otter daemon shut down during execution", true
	case res.Cancelled:
		return runs.StatusCancelled, "cancelled", false
	case res.Succeeded():
		return runs.StatusSucceeded, "", false
	default:
		return runs.StatusFailed, describeExit(res), true
	}
}

func describeExit(res *executor.Result) string {
	switch {
	case res.ExitCode != nil:
		return fmt.Sprintf("process exited with code %d", *res.ExitCode)
	case res.Signal != "":
		return fmt.Sprintf("process terminated by signal %s", res.Signal)
	case res.WaitError != nil:
		return res.WaitError.Error()
	default:
		return "process failed"
	}
}

// finishRun records the terminal state of an attempt and schedules a retry
// when the policy allows one.
func (d *Daemon) finishRun(run *runs.Run, m *config.Manifest, f runs.Finish, retryable bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if f.FinishedAt.IsZero() {
		f.FinishedAt = time.Now().UTC()
	}
	if err := d.runs.Finish(ctx, run.ID, f); err != nil {
		d.log.Error("run_finish_failed", err, "run_id", run.ID, "status", string(f.Status))
		return
	}
	run.Status = f.Status
	run.FinishedAt = &f.FinishedAt

	// The integration's own final log line. Read before this attempt's
	// terminal lifecycle line is written, so it is the integration's summary
	// (pages/fetched/written) rather than Otter's own narration.
	detail := d.detailLine(ctx, run.ID)

	fields := []any{
		"integration", run.IntegrationID,
		"run_id", run.ID,
		"attempt", run.Attempt,
		"status", string(f.Status),
		"duration_ms", run.Duration().Milliseconds(),
	}
	if f.Error != "" {
		fields = append(fields, "error", f.Error)
	}
	if f.ExitCode != nil {
		fields = append(fields, "exit_code", *f.ExitCode)
	}
	if detail != "" {
		// The integration's own last log line: for a successful sync that is
		// its summary (pages/fetched/written), which is exactly what you need
		// to tell "nothing changed" apart from "nothing was written".
		fields = append(fields, "detail", detail)
	}

	if f.Status == runs.StatusSucceeded {
		d.log.Info("run_succeeded", fields...)
	} else {
		d.log.Warn("run_finished", fields...)
	}

	summary := fmt.Sprintf("run %s (attempt %d, %s)", f.Status, run.Attempt, run.Duration().Round(time.Millisecond))
	if f.ExitCode != nil {
		summary += fmt.Sprintf(", exit code %d", *f.ExitCode)
	}
	if f.Error != "" {
		summary += ": " + f.Error
	}
	d.appendOtterLog(run.ID, summary)

	if !retryable || m == nil {
		return
	}
	if !retry.ShouldRetry(m.MaxAttempts(), run.Attempt) {
		d.log.Info("run_retries_exhausted",
			"integration", run.IntegrationID, "run_id", run.ID, "attempts", run.Attempt)
		return
	}
	if err := d.scheduleRetry(ctx, run, m); err != nil {
		d.log.Error("run_retry_failed", err, "run_id", run.ID)
	}
}

// detailLine returns the integration's last own log line for an attempt, read
// straight from the log store so it covers both stdout and ctx.log output.
func (d *Daemon) detailLine(ctx context.Context, runID string) string {
	line, ok, err := d.logs.Last(ctx, runID, runs.StreamOtter)
	if err != nil || !ok {
		return ""
	}
	line = strings.TrimSpace(line)
	const limit = 300
	if len(line) > limit {
		line = line[:limit] + "…"
	}
	return line
}

// scheduleRetry creates the next attempt as a new run record linked to the
// attempt it retries, and enqueues it with the backoff delay applied.
func (d *Daemon) scheduleRetry(ctx context.Context, previous *runs.Run, m *config.Manifest) error {
	delay := retry.Delay(m.Retry, previous.Attempt)
	now := time.Now().UTC()
	parentID := previous.ID

	next := &runs.Run{
		ID:                uuid.NewString(),
		IntegrationID:     previous.IntegrationID,
		TriggerType:       previous.TriggerType,
		Status:            runs.StatusRetrying,
		Attempt:           previous.Attempt + 1,
		ParentRunID:       &parentID,
		CreatedAt:         now,
		Metadata:          previous.Metadata,
		PythonMode:        previous.PythonMode,
		PythonVersion:     previous.PythonVersion,
		EnvironmentDigest: previous.EnvironmentDigest,
		PythonPolicy:      previous.PythonPolicy,
		ReleaseDigest:     previous.ReleaseDigest,
		ReleaseSourceDir:  previous.ReleaseSourceDir,
		SDKVersion:        previous.SDKVersion,
	}
	availableAt := now.Add(delay)

	err := d.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := d.runs.CreateTx(ctx, tx, next); err != nil {
			return err
		}
		return d.queue.EnqueueTx(ctx, tx, next.ID, next.IntegrationID, availableAt)
	})
	if err != nil {
		return fmt.Errorf("schedule retry for %s: %w", previous.ID, err)
	}

	d.log.Info("run_retry_scheduled",
		"integration", next.IntegrationID,
		"run_id", next.ID,
		"retry_of", previous.ID,
		"attempt", next.Attempt,
		"max_attempts", m.MaxAttempts(),
		"delay", delay.String())

	d.appendOtterLog(previous.ID,
		fmt.Sprintf("retry %d of %d scheduled in %s as run %s",
			next.Attempt, m.MaxAttempts(), delay, next.ID))

	d.notifyWorkers()
	return nil
}

// appendOtterLog writes a runtime lifecycle line into the same log stream as
// the integration's own output, so `otter logs <run-id>` tells the whole story.
func (d *Daemon) appendOtterLog(runID, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := d.logs.Append(ctx, runs.LogEntry{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Stream:    runs.StreamOtter,
		Message:   message,
	})
	if err != nil {
		d.log.Error("run_log_write_failed", err, "run_id", runID)
	}
}

// runLogSink batches captured lines so a chatty integration does not cause one
// SQLite transaction per line.
//
// It also remembers the last thing the integration wrote to stderr, plus the
// SDK's own failure line, so a failed run can be reported with the integration's
// actual error instead of only "process exited with code 1". Without that,
// diagnosing a failure means digging the reason out of run_logs by hand.
type runLogSink struct {
	logs   *runs.LogStore
	logger *logging.Logger
	runID  string

	mu            sync.Mutex
	buf           []runs.LogEntry
	lastFlush     time.Time
	lastStderr    string
	lastSdkFailed string
}

// Line implements executor.LogSink.
func (s *runLogSink) Line(stream string, at time.Time, message string) {
	s.mu.Lock()
	s.buf = append(s.buf, runs.LogEntry{
		RunID:     s.runID,
		Timestamp: at,
		Stream:    stream,
		Message:   message,
	})

	if stream == runs.StreamStderr && strings.TrimSpace(message) != "" {
		// The last stderr line of a Python traceback is the exception itself.
		s.lastStderr = strings.TrimSpace(message)
	}
	if stream == runs.StreamOtter && strings.Contains(message, "integration failed:") {
		s.lastSdkFailed = strings.TrimSpace(message)
	}

	var batch []runs.LogEntry
	if len(s.buf) >= 50 || time.Since(s.lastFlush) >= 250*time.Millisecond {
		batch = s.buf
		s.buf = nil
		s.lastFlush = time.Now()
	}
	s.mu.Unlock()

	if len(batch) > 0 {
		s.write(batch)
	}
}

// FailureHint returns the integration's own error, if it produced one.
func (s *runLogSink) FailureHint() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	hint := s.lastStderr
	if hint == "" {
		hint = strings.TrimPrefix(s.lastSdkFailed, "integration failed: ")
	}
	hint = strings.TrimSpace(hint)
	if index := strings.Index(hint, " {"); index > 0 && strings.HasSuffix(hint, "}") {
		// Trim the SDK's structured-log JSON tail from its failure line.
		hint = hint[:index]
	}
	const limit = 300
	if len(hint) > limit {
		hint = hint[:limit] + "…"
	}
	return hint
}

// Flush writes any buffered lines. It must be called before the run finishes.
func (s *runLogSink) Flush() {
	s.mu.Lock()
	batch := s.buf
	s.buf = nil
	s.lastFlush = time.Now()
	s.mu.Unlock()

	if len(batch) > 0 {
		s.write(batch)
	}
}

func (s *runLogSink) write(batch []runs.LogEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.logs.AppendBatch(ctx, batch); err != nil {
		s.logger.Error("run_log_write_failed", err, "run_id", s.runID, "lines", len(batch))
	}
}

func newRunLogSink(logs *runs.LogStore, runID string, logger *logging.Logger) *runLogSink {
	return &runLogSink{logs: logs, logger: logger, runID: runID, lastFlush: time.Now()}
}
