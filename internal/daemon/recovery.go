package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/retry"
	"github.com/tkoizumi/otter/internal/runs"
)

// crashMessage is recorded on runs that were executing when a previous daemon
// instance died.
const crashMessage = "otter daemon restarted during execution"

// recoverRuns handles runs left in `running` by a previous daemon instance.
//
// The child process is gone (or orphaned and unusable), so the attempt cannot
// be completed. It is marked failed and, when the manifest's retry policy
// allows another attempt, a fresh attempt is enqueued -- both in one
// transaction. Doing this at startup, before triggers are registered,
// guarantees the work is picked up again without ever running twice.
//
// Before treating a `running` run as interrupted it consults the fallback
// journal: a run whose child actually finished may have been left running only
// because its terminal write failed, and re-executing a successful child would
// duplicate its external effects.
func (d *Daemon) recoverRuns(ctx context.Context) error {
	recorded, err := d.readFinishJournal()
	if err != nil {
		// Failing open here would re-execute a child whose outcome the journal
		// already holds. Leave every running run alone and report it: a
		// pending run is visible and repairable, a duplicated effect is not.
		return fmt.Errorf("recover runs: read finish journal: %w", err)
	}

	stale, err := d.runs.ListByStatusAll(ctx, runs.StatusRunning)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		// Nothing is running, so no journal record can be applied; leaving one
		// behind would replay an outcome whose run already reached a state.
		return d.rewriteFinishJournal(nil)
	}

	d.log.Warn("recovering_interrupted_runs", "count", len(stale))

	keep := map[string]finishRecord{}
	for _, run := range stale {
		rec, journalled := recorded[run.ID]
		if !journalled {
			d.recoverInterrupted(ctx, run)
			continue
		}
		if !d.applyFinishRecord(ctx, run, rec) {
			// The record could not be consumed; keep it for the next restart.
			keep[run.ID] = rec
		}
	}
	return d.rewriteFinishJournal(keep)
}

// recoverInterrupted terminalises one run left `running` by a previous daemon
// instance and, when the policy allows, creates its successor in the same
// transaction as the terminal write.
func (d *Daemon) recoverInterrupted(ctx context.Context, run *runs.Run) {
	entry, ok := d.reg.get(run.IntegrationID)
	if !ok || entry.Manifest == nil {
		d.log.Warn("recovery_no_manifest",
			"integration", run.IntegrationID, "run_id", run.ID)
	}

	var next *retryPlan
	if ok && entry.Manifest != nil && retry.ShouldRetry(entry.Manifest.MaxAttempts(), run.Attempt) {
		next = d.planRetry(run, entry.Manifest)
	}

	f := runs.Finish{
		Status:     runs.StatusFailed,
		Error:      crashMessage,
		FinishedAt: time.Now().UTC(),
	}
	if err := d.persistOutcome(ctx, run.ID, f, next); err != nil {
		d.log.Error("recovery_finish_failed", err, "run_id", run.ID)
		return
	}

	d.appendOtterLog(run.ID, "marked failed: "+crashMessage)
	d.log.Warn("run_recovered",
		"integration", run.IntegrationID,
		"run_id", run.ID,
		"attempt", run.Attempt)

	if next != nil {
		d.scheduleSuccessor(run, next)
	}
}

// applyFinishRecord applies an outcome the fallback journal recorded when its
// transaction could not be written. The recorded status is honoured rather than
// replaced by a crash failure: a child that already succeeded must not run
// again. It reports whether the record was consumed.
func (d *Daemon) applyFinishRecord(ctx context.Context, run *runs.Run, rec finishRecord) bool {
	if !rec.Status.Terminal() {
		return false
	}
	f := runs.Finish{
		Status:     rec.Status,
		ExitCode:   rec.ExitCode,
		Error:      rec.Error,
		FinishedAt: rec.FinishedAt,
	}
	if f.FinishedAt.IsZero() {
		f.FinishedAt = time.Now().UTC()
	}

	var next *retryPlan
	if rec.Retry {
		if entry, ok := d.reg.get(run.IntegrationID); ok && entry.Manifest != nil &&
			retry.ShouldRetry(entry.Manifest.MaxAttempts(), run.Attempt) {
			next = d.planRetry(run, entry.Manifest)
		}
	}

	if err := d.persistOutcome(ctx, run.ID, f, next); err != nil {
		d.log.Error("recovery_outcome_apply_failed", err, "run_id", run.ID)
		return false
	}

	d.appendOtterLog(run.ID, "recorded outcome recovered: "+string(rec.Status))
	d.log.Warn("run_outcome_recovered",
		"integration", run.IntegrationID,
		"run_id", run.ID,
		"status", string(rec.Status),
		"attempt", run.Attempt)

	if next != nil {
		d.scheduleSuccessor(run, next)
	}
	return true
}

// reconcileQueue repairs the one inconsistency a crash can leave behind: a run
// whose status says it is waiting, but which has no queue row. This should not
// happen because creation is transactional, but re-queueing is cheap insurance
// for durable work.
func (d *Daemon) reconcileQueue(ctx context.Context) error {
	requeued := 0

	for _, status := range []runs.Status{runs.StatusQueued, runs.StatusRetrying} {
		list, err := d.runs.ListByStatusAll(ctx, status)
		if err != nil {
			return err
		}

		for _, run := range list {
			present, err := d.queue.Contains(ctx, run.ID)
			if err != nil {
				return err
			}
			if present {
				continue
			}
			if err := d.queue.Enqueue(ctx, run.ID, run.IntegrationID, time.Now().UTC()); err != nil {
				return err
			}
			requeued++
			d.log.Warn("run_requeued",
				"integration", run.IntegrationID,
				"run_id", run.ID,
				"status", string(status))
		}
	}

	if requeued > 0 {
		d.log.Info("queue_reconciled", "requeued", requeued)
	}
	return nil
}
