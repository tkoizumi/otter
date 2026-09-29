package daemon

import (
	"context"
	"time"

	"github.com/tkoizumi/otter/internal/runs"
)

// retentionInterval is how often automatic run and log retention sweeps. It is
// deliberately independent of the configured windows so a long window still
// bounds the work any single sweep performs.
const retentionInterval = time.Hour

// maxRetentionSweeps bounds how many batches one sweep drains. A database with
// more expired work than this is left for the next hourly sweep rather than
// holding the connection for an unbounded time.
const maxRetentionSweeps = 100

// runRetention retires expired runs and prunes the logs of expired runs on a
// timer.
//
// Both windows default to zero -- retain forever -- so with no retention flags
// this returns without touching the database. That is a deliberate regression
// guard: deleting run history is a destructive default change, and a daemon an
// operator did not configure for retention must not prune anything.
func (d *Daemon) runRetention(ctx context.Context) {
	if d.cfg.RunRetention <= 0 && d.cfg.LogRetention <= 0 {
		return
	}

	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		d.expireRetention(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
		}
	}
}

// expireRetention runs both windows. Runs first: retiring a chain removes its
// logs too, so the log sweep then has less to consider.
func (d *Daemon) expireRetention(ctx context.Context) {
	if d.cfg.RunRetention > 0 {
		d.expireRuns(ctx)
	}
	if d.cfg.LogRetention > 0 {
		d.expireLogs(ctx)
	}
}

// expireRuns deletes the run rows of expired retry chains, in bounded batches.
// One line is logged per sweep, with the cutoff and the total removed, so a
// multi-batch sweep does not flood the log.
func (d *Daemon) expireRuns(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-d.cfg.RunRetention)
	var retired int64

	for i := 0; i < maxRetentionSweeps; i++ {
		ids, err := d.runs.ExpiredRunIDs(ctx, cutoff, runs.RetentionBatchChains)
		if err != nil {
			d.log.Warn("runs_retention_failed", "error", err.Error(), "runs", retired)
			break
		}
		if len(ids) == 0 {
			break
		}

		// Capture first, per run. Deleting it is idempotent, so a batch that
		// fails here is retried whole on the next sweep; if the run rows went
		// first and this failed, the capture could never be found again.
		if d.inspection != nil {
			failed := false
			for _, id := range ids {
				if _, err := d.inspection.DeleteForRun(ctx, id); err != nil {
					d.log.Warn("runs_retention_capture_failed",
						"run_id", id, "error", err.Error(), "runs", retired)
					failed = true
					break
				}
			}
			if failed {
				break
			}
		}

		removed, err := d.runs.DeleteRuns(ctx, ids)
		if err != nil {
			d.log.Warn("runs_retention_failed", "error", err.Error(), "runs", retired)
			break
		}
		retired += removed
	}

	if retired > 0 {
		d.log.Info("runs_retained",
			"runs", retired,
			"cutoff", cutoff.Format(time.RFC3339))
	}
}

// expireLogs prunes the logs of expired runs, in bounded batches, without
// removing the run rows themselves. As with runs, one line is logged per sweep.
func (d *Daemon) expireLogs(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-d.cfg.LogRetention)
	var pruned int64

	for i := 0; i < maxRetentionSweeps; i++ {
		removed, err := d.logs.DeleteOlderThan(ctx, cutoff)
		if err != nil {
			d.log.Warn("logs_retention_failed", "error", err.Error(), "logs", pruned)
			break
		}
		if removed == 0 {
			break
		}
		pruned += removed
	}

	if pruned > 0 {
		d.log.Info("logs_retained",
			"logs", pruned,
			"cutoff", cutoff.Format(time.RFC3339))
	}
}
