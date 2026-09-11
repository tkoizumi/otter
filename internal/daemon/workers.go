package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/otter-runtime/otter/internal/config"
	"github.com/otter-runtime/otter/internal/executor"
	"github.com/otter-runtime/otter/internal/logging"
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
	d.finishRun(run, m, runs.Finish{
		Status:     status,
		ExitCode:   result.ExitCode,
		Error:      message,
		FinishedAt: result.FinishedAt,
	}, retryable)
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

// scheduleRetry creates the next attempt as a new run record linked to the
// attempt it retries, and enqueues it with the backoff delay applied.
func (d *Daemon) scheduleRetry(ctx context.Context, previous *runs.Run, m *config.Manifest) error {
	delay := retry.Delay(m.Retry, previous.Attempt)
	now := time.Now().UTC()
	parentID := previous.ID

	next := &runs.Run{
		ID:            uuid.NewString(),
		IntegrationID: previous.IntegrationID,
		TriggerType:   previous.TriggerType,
		Status:        runs.StatusRetrying,
		Attempt:       previous.Attempt + 1,
		ParentRunID:   &parentID,
		CreatedAt:     now,
		Metadata:      previous.Metadata,
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
type runLogSink struct {
	logs   *runs.LogStore
	logger *logging.Logger
	runID  string

	mu        sync.Mutex
	buf       []runs.LogEntry
	lastFlush time.Time
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
