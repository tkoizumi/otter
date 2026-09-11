package daemon

import (
	"context"
	"time"

	"github.com/otter-runtime/otter/internal/retry"
	"github.com/otter-runtime/otter/internal/runs"
)

// crashMessage is recorded on runs that were executing when a previous daemon
// instance died.
const crashMessage = "otter daemon restarted during execution"

// recoverRuns handles runs left in `running` by a previous daemon instance.
//
// The child process is gone (or orphaned and unusable), so the attempt cannot
// be completed. It is marked failed, and when the manifest's retry policy
// allows another attempt, a fresh attempt is enqueued. Doing this at startup,
// before triggers are registered, guarantees the work is picked up again
// without ever running twice.
func (d *Daemon) recoverRuns(ctx context.Context) error {
	stale, err := d.runs.ListByStatus(ctx, runs.StatusRunning, 10000)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}

	d.log.Warn("recovering_interrupted_runs", "count", len(stale))

	for _, run := range stale {
		if err := d.runs.Finish(ctx, run.ID, runs.Finish{
			Status:     runs.StatusFailed,
			Error:      crashMessage,
			FinishedAt: time.Now().UTC(),
		}); err != nil {
			d.log.Error("recovery_finish_failed", err, "run_id", run.ID)
			continue
		}

		d.appendOtterLog(run.ID, "marked failed: "+crashMessage)
		d.log.Warn("run_recovered",
			"integration", run.IntegrationID,
			"run_id", run.ID,
			"attempt", run.Attempt)

		entry, ok := d.reg.get(run.IntegrationID)
		if !ok || entry.Manifest == nil {
			d.log.Warn("recovery_no_manifest",
				"integration", run.IntegrationID, "run_id", run.ID)
			continue
		}
		if !retry.ShouldRetry(entry.Manifest.MaxAttempts(), run.Attempt) {
			continue
		}
		if err := d.scheduleRetry(ctx, run, entry.Manifest); err != nil {
			d.log.Error("recovery_retry_failed", err, "run_id", run.ID)
		}
	}

	return nil
}

// reconcileQueue repairs the one inconsistency a crash can leave behind: a run
// whose status says it is waiting, but which has no queue row. This should not
// happen because creation is transactional, but re-queueing is cheap insurance
// for durable work.
func (d *Daemon) reconcileQueue(ctx context.Context) error {
	requeued := 0

	for _, status := range []runs.Status{runs.StatusQueued, runs.StatusRetrying} {
		list, err := d.runs.ListByStatus(ctx, status, 10000)
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
