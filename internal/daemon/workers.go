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

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/executor"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/notify"
	"github.com/tkoizumi/otter/internal/pyenv"
	"github.com/tkoizumi/otter/internal/queue"
	"github.com/tkoizumi/otter/internal/retry"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/secrets"
)

// workerIdlePoll is how often an idle worker re-checks the queue. Retries sit
// in the queue with a future available_at, so a periodic sweep is what makes
// them fire on time.
const workerIdlePoll = 500 * time.Millisecond

// claimFailureBackoff is how long a claimed run waits before a worker may pick
// it up again after its attempt could not start. Claim deletes the run_queue
// row, so returning the run to the queue is what keeps it from being stranded
// between restarts; the delay keeps a persistent database fault from spinning
// the workers on the same row.
const claimFailureBackoff = 5 * time.Second

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
			// Maintenance stops claiming without stopping the worker: a run
			// already executing has to finish, keep its state and log APIs,
			// and be visible while it does. Returning here rather than
			// exiting the goroutine is what lets a drain complete and the
			// runtime be activated again without a restart.
			if d.maint.Gated() {
				break
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
	// The released slot is what decides whether a drain has finished, so the
	// transition is recorded after it, not before: asking "is anything still
	// running?" while this run still holds its slot would always say yes.
	defer func() {
		d.cap.Release(item.JobID)
		d.noteDrainProgress()
	}()

	loadCtx, cancelLoad := context.WithTimeout(context.Background(), 30*time.Second)
	run, err := d.runs.Get(loadCtx, item.RunID)
	cancelLoad()
	if err != nil {
		// Classify before acting. The store distinguishes only "missing" from
		// "failed to read", and the two need opposite treatment.
		if errors.Is(err, runs.ErrNotFound) {
			// The record is gone: there is nothing to execute, nothing to
			// terminal (Finish would match no row) and nothing worth
			// re-enqueueing, because every later claim could only fail to load
			// it again. The run is already terminal by deletion; say so once
			// rather than manufacture a poison queue row.
			d.log.Warn("run_load_failed",
				"error", err.Error(),
				"run_id", item.RunID,
				"job", item.JobID,
				"action", "dropped")
			return
		}
		// Anything else is a read fault with the record still waiting. Re-enqueue
		// it with a backoff: preserving the attempt is the safer default, and a
		// fault that turns out to be permanent is repaired by startup
		// reconciliation rather than by dropping accepted work.
		d.log.Error("run_load_failed", err, "run_id", item.RunID, "job", item.JobID)
		d.requeueClaimed(item, "run_load_failed")
		return
	}

	entry, ok := d.reg.get(run.JobID)
	if !ok || entry.Manifest == nil {
		d.finishRun(run, nil, runs.Finish{
			Status: runs.StatusFailed,
			Error:  "job is no longer available; the manifest may have been removed",
		}, false)
		return
	}
	m := entry.Manifest

	// Identity fencing. A run was authorized against the identity generation
	// current at submission; a reset, move, retirement or deletion bumps that
	// generation precisely so this claim cannot execute stale code against
	// state that now belongs to a different instance.
	if !entry.Instance.Status.AcceptsWork() {
		d.finishRun(run, m, runs.Finish{
			Status:     runs.StatusCancelled,
			Error:      fmt.Sprintf("job identity is %s", entry.Instance.Status),
			FinishedAt: time.Now().UTC(),
		}, false)
		return
	}
	if run.JobGeneration != 0 && entry.Instance.Generation != run.JobGeneration {
		d.finishRun(run, m, runs.Finish{
			Status: runs.StatusCancelled,
			Error: fmt.Sprintf("job identity changed (generation %d, run authorized for %d)",
				entry.Instance.Generation, run.JobGeneration),
			FinishedAt: time.Now().UTC(),
		}, false)
		return
	}

	// A bound run executes its recorded snapshot, not the live source tree.
	//
	// The manifest is re-read from the snapshot rather than re-pointed at it:
	// relative declarations in it -- python.path above all -- are resolved
	// against the job directory, and the snapshot mirrors the checkout
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
		spec := pyenv.RecordedIdentity(run.JobID, run.PythonVersion, run.EnvironmentDigest, run.PythonPolicy)
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
		// The record was readable and is still waiting, so this is a transient
		// database fault rather than a bad run. Return it to the queue instead
		// of stranding it until the next restart.
		d.log.Error("run_mark_running_failed", err, "run_id", run.ID)
		d.requeueClaimed(item, "run_mark_running_failed")
		return
	}
	if !claimed {
		// Cancelled while it sat in the queue, or already claimed elsewhere.
		d.log.Debug("run_not_claimable", "run_id", run.ID, "status", string(run.Status))
		return
	}
	run.Status = runs.StatusRunning
	run.StartedAt = &startedAt

	token, err := d.runTokens.Issue(run.ID, run.JobID, run.JobGeneration)
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
		"job", m.Name,
		"run_id", run.ID,
		"attempt", run.Attempt,
		"trigger", run.TriggerType)

	d.appendOtterLog(run.ID, fmt.Sprintf("run started (attempt %d of %d, trigger %s)",
		run.Attempt, m.MaxAttempts(), run.TriggerType))

	// Resolve secrets before launching Python so a missing secret is a clear
	// configuration failure rather than a crash inside the job.
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
		JobID:          run.JobID,
		JobName:        run.JobName,
		Executable:     interpreter,
		Managed:        m.Python.Mode == "managed",
		RunID:          run.ID,
		TriggerType:    run.TriggerType,
		APIURL:         d.cfg.ChildAPIURL(),
		StateToken:     token,
		ExtraEnv:       env,
		Timeout:        m.TimeoutDuration(),
		TerminateGrace: 5 * time.Second,
		CapturePolicy:  run.CapturePolicy,
		Config:         d.pinnedConfig(run),
	}, sink)
	sink.Flush()

	// Capture is finalized from the child's actual fate, not from anything it
	// claimed: a process killed by a signal never reports, and its recording must
	// still be visible as incomplete rather than silently pending.
	d.finalizeCapture(run.ID, result, ctl.getReason())

	status, message, retryable := classifyOutcome(result, ctl.getReason(), m)
	message = withFailureHint(message, status, sink.FailureHint())
	d.finishRun(run, m, runs.Finish{
		Status:     status,
		ExitCode:   result.ExitCode,
		Error:      message,
		FinishedAt: result.FinishedAt,
	}, retryable)
}

// requeueClaimed returns a claimed run to the queue after its attempt could not
// be started.
//
// Claim deletes the run_queue row in the same transaction that reserves the
// worker slot, so an attempt that returns without either executing or recording
// a terminal state would sit as queued with no queue row until the next daemon
// restart: accepted work, silently lost. Re-enqueueing with a backoff keeps the
// run durable instead.
//
// It cannot cause a double execution. MarkRunning is still the gate that moves a
// run from queued or retrying to running, and a run that already reached running
// is refused by that transition rather than run a second time.
//
// A re-enqueue that itself fails is logged, not fatal: it leaves exactly the
// inconsistency -- a waiting status with no queue row -- that reconcileQueue
// repairs at the next startup.
func (d *Daemon) requeueClaimed(item *queue.Item, cause string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	availableAt := time.Now().UTC().Add(claimFailureBackoff)
	if err := d.queue.Enqueue(ctx, item.RunID, item.JobID, availableAt); err != nil {
		d.log.Error("run_requeue_failed", err,
			"run_id", item.RunID,
			"job", item.JobID,
			"cause", cause)
		return
	}

	d.log.Warn("run_requeued",
		"run_id", item.RunID,
		"job", item.JobID,
		"cause", cause,
		"retry_in", claimFailureBackoff.String())
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

// withFailureHint folds the job's own error into the message Otter
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

// finishWriteAttempts bounds how many times one attempt's outcome is written
// before the durable fallback takes over. A healthy SQLite write needs no
// retry; the bound absorbs a transient fault without letting a permanent one
// spin the worker.
const (
	finishWriteAttempts = 3
	finishWriteBackoff  = 200 * time.Millisecond
)

// retryPlan is the successor attempt a terminal outcome creates. It is built
// before any write, from the policy alone, so the retry decision is taken once
// and cannot depend on a side effect.
type retryPlan struct {
	run         *runs.Run
	availableAt time.Time
	delay       time.Duration
	maxAttempts int
}

// finishCommit is the commit step of the atomic finish transaction. It is a
// variable so a test can inject a commit failure -- impossible to trigger
// against a healthy SQLite file -- and prove that a terminal write and its
// successor are all-or-nothing.
var finishCommit = func(tx *sql.Tx) error { return tx.Commit() }

// finishRun records the terminal state of an attempt and, when the policy
// allows one, its successor attempt and queue row, in a single transaction.
//
// The whole outcome is decided before anything is written, so no side effect
// can roll the decision back. Logging, notification and capture are
// best-effort and run strictly after the durable write.
func (d *Daemon) finishRun(run *runs.Run, m *config.Manifest, f runs.Finish, retryable bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if f.FinishedAt.IsZero() {
		f.FinishedAt = time.Now().UTC()
	}

	willRetry := retryable && m != nil && retry.ShouldRetry(m.MaxAttempts(), run.Attempt)
	var next *retryPlan
	if willRetry {
		next = d.planRetry(run, m)
	}

	// The job's own final log line. Read before this attempt's
	// terminal lifecycle line is written, so it is the job's summary
	// (pages/fetched/written) rather than Otter's own narration.
	detail := d.detailLine(ctx, run.ID)

	if err := d.commitOutcome(ctx, run.ID, f, next); err != nil {
		d.fallbackOutcome(run, f, next, err)
		return
	}

	run.Status = f.Status
	run.FinishedAt = &f.FinishedAt

	d.reportOutcome(run, f, detail, retryable, next)
}

// commitOutcome persists the terminal state and its successor with a bounded
// retry. A commit that reports failure may still have been applied -- SQLite
// can fail after the write became durable -- so the recorded state is re-read
// before retrying. That is what keeps an ambiguous commit from creating a
// second successor.
func (d *Daemon) commitOutcome(ctx context.Context, runID string, f runs.Finish, next *retryPlan) error {
	var lastErr error
	for attempt := 1; attempt <= finishWriteAttempts; attempt++ {
		if err := d.persistOutcome(ctx, runID, f, next); err != nil {
			lastErr = err
		} else {
			return nil
		}
		if d.outcomeRecorded(ctx, runID, f, next) {
			return nil
		}
		if attempt == finishWriteAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(finishWriteBackoff):
		}
	}
	return lastErr
}

// persistOutcome writes one attempt's terminal state and, when plan is not nil,
// its successor attempt and queue row, in one transaction. Either the whole
// outcome is durable or none of it is.
func (d *Daemon) persistOutcome(ctx context.Context, runID string, f runs.Finish, plan *retryPlan) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("runs: finish %s: begin: %w", runID, err)
	}
	// Safe after Commit: Rollback returns ErrTxDone, which we ignore.
	defer func() { _ = tx.Rollback() }()

	if err := d.runs.FinishTx(ctx, tx, runID, f); err != nil {
		return err
	}
	if plan != nil {
		if err := d.runs.CreateTx(ctx, tx, plan.run); err != nil {
			return err
		}
		if err := d.queue.EnqueueTx(ctx, tx, plan.run.ID, plan.run.JobID, plan.availableAt); err != nil {
			return err
		}
	}
	if err := finishCommit(tx); err != nil {
		return fmt.Errorf("runs: finish %s: commit: %w", runID, err)
	}
	return nil
}

// outcomeRecorded reports whether the outcome commitOutcome was trying to write
// is already durable. It is how a commit that failed after its write landed is
// told apart from one that rolled back.
func (d *Daemon) outcomeRecorded(ctx context.Context, runID string, f runs.Finish, next *retryPlan) bool {
	stored, err := d.runs.Get(ctx, runID)
	if err != nil || stored.Status != f.Status {
		return false
	}
	if next == nil {
		return true
	}
	if _, err := d.runs.Get(ctx, next.run.ID); err != nil {
		return false
	}
	queued, err := d.queue.Contains(ctx, next.run.ID)
	return err == nil && queued
}

// fallbackOutcome is the explicit rule for a terminal write that could not be
// persisted. Returning silently is forbidden: a run left `running` after a
// successful child is re-executed by crash recovery, duplicating its external
// effects. The outcome is recorded in a durable sidecar journal that recovery
// applies instead of inventing a crash failure.
func (d *Daemon) fallbackOutcome(run *runs.Run, f runs.Finish, next *retryPlan, cause error) {
	d.log.Error("run_finish_failed", cause,
		"run_id", run.ID,
		"job", run.JobID,
		"status", string(f.Status),
		"action", "journalled")

	rec := finishRecord{
		RunID:      run.ID,
		Status:     f.Status,
		ExitCode:   f.ExitCode,
		Error:      f.Error,
		FinishedAt: f.FinishedAt,
		Retry:      next != nil,
		RecordedAt: time.Now().UTC(),
	}
	if err := d.appendFinishRecord(rec); err != nil {
		// Nothing durable remains. A restart will treat the run as
		// interrupted; say so unambiguously rather than let a log line imply
		// the outcome was saved.
		d.log.Error("run_finish_fallback_failed", err,
			"run_id", run.ID, "status", string(f.Status))
	}
}

// reportOutcome emits the best-effort side effects of an attempt whose outcome
// is already durable. None of it runs before the write, and none of it may
// change what was recorded.
func (d *Daemon) reportOutcome(run *runs.Run, f runs.Finish, detail string, retryable bool, next *retryPlan) {
	durationMS := run.Duration().Milliseconds()
	fields := []any{
		"job", run.JobID,
		"run_id", run.ID,
		"attempt", run.Attempt,
		"status", string(f.Status),
		"duration_ms", durationMS,
	}
	if f.Error != "" {
		fields = append(fields, "error", f.Error)
	}
	if f.ExitCode != nil {
		fields = append(fields, "exit_code", *f.ExitCode)
	}
	if detail != "" {
		// The job's own last log line: for a successful sync that is
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

	if next != nil {
		d.scheduleSuccessor(run, next)
		return
	}

	// Decide whether this attempt is the end of the road before notifying.
	//
	// An intermediate failure is not worth an alert: the retry policy exists
	// precisely because some failures recover, and a job that retries
	// three times would otherwise send three alerts. Alert fatigue is how a
	// working notification becomes an ignored one, so the signal reported is
	// "this run has given up", not "an attempt failed".
	if retryable {
		d.log.Info("run_retries_exhausted",
			"job", run.JobID, "run_id", run.ID, "attempts", run.Attempt)
	}
	d.notifyFailure(run, f, detail, durationMS)
}

// scheduleSuccessor performs the best-effort side effects of a retry whose run
// record and queue row are already durable: its capture recording, the log
// lines, and waking the workers.
func (d *Daemon) scheduleSuccessor(previous *runs.Run, plan *retryPlan) {
	next := plan.run

	// Each attempt owns its own recording, created before the retry can be
	// claimed so its child never submits capture for a missing summary. A retry
	// of a run that predates capture keeps no recording at all: inventing one
	// would report "incomplete" for a run that was never captured.
	if next.CapturePolicy != "" {
		if policy, err := inspection.ParsePolicy(next.CapturePolicy); err == nil {
			d.beginCapture(next.ID, next.JobID, policy)
		}
	}

	d.log.Info("run_retry_scheduled",
		"job", next.JobID,
		"run_id", next.ID,
		"retry_of", previous.ID,
		"attempt", next.Attempt,
		"max_attempts", plan.maxAttempts,
		"delay", plan.delay.String())

	d.appendOtterLog(previous.ID,
		fmt.Sprintf("retry %d of %d scheduled in %s as run %s",
			next.Attempt, plan.maxAttempts, plan.delay, next.ID))

	d.notifyWorkers()
}

// notifyFailure reports a terminal failure to the configured endpoint.
//
// It is best-effort by design: the run is already recorded, and a notification
// that cannot be delivered must never change what the run did. The failure is
// logged and dropped.
//
// Only scheduler-driven failures are reported here. An operator cancelling a
// run is not a failure: they already know.
func (d *Daemon) notifyFailure(run *runs.Run, f runs.Finish, detail string, durationMS int64) {
	if !d.notifier.Wants(string(f.Status)) {
		return
	}

	payload := notify.Payload{
		Job:        run.JobID,
		RunID:      run.ID,
		Status:     string(f.Status),
		Attempt:    run.Attempt,
		Error:      f.Error,
		Detail:     detail,
		DurationMS: durationMS,
		ExitCode:   f.ExitCode,
		Release:    run.ReleaseDigest,
		Host:       d.hostname,
	}

	// Bounded so a slow endpoint cannot hold the worker. The notifier already
	// retries with its own timeout; this is the outer bound.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.notifier.Send(ctx, payload); err != nil {
		// A missing alert is worth knowing about, but it is not a run failure.
		d.log.Warn("notification_failed", "run_id", run.ID, "error", err.Error())
	}
}

// detailLine returns the job's last own log line for an attempt, read
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

// planRetry builds the successor attempt for a failed run from the manifest's
// retry policy, without writing anything. The caller persists it together with
// the terminal state in one transaction.
func (d *Daemon) planRetry(previous *runs.Run, m *config.Manifest) *retryPlan {
	delay := retry.Delay(m.Retry, previous.Attempt)
	now := time.Now().UTC()
	parentID := previous.ID

	next := &runs.Run{
		ID:                uuid.NewString(),
		JobID:             previous.JobID,
		JobName:           previous.JobName,
		JobGeneration:     previous.JobGeneration,
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
		// The schedule an occurrence belongs to and the configuration version it
		// was accepted with both belong to the submission, so a retry keeps them:
		// the retry is the same occurrence and must read the same values.
		ScheduleID:    previous.ScheduleID,
		ConfigVersion: previous.ConfigVersion,
		// The capture policy belongs to the submission, not the attempt: a retry
		// records exactly what the operator asked for, and owns its own requests.
		CapturePolicy: previous.CapturePolicy,
	}
	return &retryPlan{
		run:         next,
		availableAt: now.Add(delay),
		delay:       delay,
		maxAttempts: m.MaxAttempts(),
	}
}

// appendOtterLog writes a runtime lifecycle line into the same log stream as
// the job's own output, so `otter logs <run-id>` tells the whole story.
func (d *Daemon) appendOtterLog(runID, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := d.logs.Append(ctx, runs.LogEntry{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Stream:    runs.StreamOtter,
		Message:   message,
		Origin:    runs.OriginDaemon,
	})
	if err != nil {
		d.log.Error("run_log_write_failed", err, "run_id", runID)
	}
}

// runLogSink batches captured lines so a chatty job does not cause one
// SQLite transaction per line.
//
// It also remembers the last thing the job wrote to stderr, plus the
// SDK's own failure line, so a failed run can be reported with the job's
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
		Origin:    runs.OriginChild,
	})

	if stream == runs.StreamStderr && strings.TrimSpace(message) != "" {
		// The last stderr line of a Python traceback is the exception itself.
		s.lastStderr = strings.TrimSpace(message)
	}
	if stream == runs.StreamOtter && strings.Contains(message, "job failed:") {
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

// FailureHint returns the job's own error, if it produced one.
func (s *runLogSink) FailureHint() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	hint := s.lastStderr
	if hint == "" {
		hint = strings.TrimPrefix(s.lastSdkFailed, "job failed: ")
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
