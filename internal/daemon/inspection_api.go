package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/executor"
	"github.com/tkoizumi/otter/internal/inspection"
)

// captureRetentionInterval is how often expired capture is pruned. It is
// deliberately independent of the retention window so that a long window still
// bounds the work any single sweep performs.
const captureRetentionInterval = time.Hour

// IngestCaptureEvents implements api.Backend.
//
// The integration identity comes from the run record, never from the caller, so
// a run token cannot attribute traffic to another integration. A run with no
// capture configuration is refused rather than silently starting one: the row is
// written at submission precisely so ingestion cannot invent a recording.
func (d *Daemon) IngestCaptureEvents(ctx context.Context, runID string, batch inspection.EventBatch) (*inspection.IngestResult, error) {
	if d.inspection == nil {
		return nil, fmt.Errorf("http capture is not available: %w", api.ErrConflict)
	}
	if _, err := d.runs.Get(ctx, runID); err != nil {
		return nil, err
	}
	return d.inspection.Ingest(ctx, runID, batch)
}

// CaptureSummary implements api.Backend. A run that exists but was never
// configured for capture reports "unavailable" rather than an empty history.
func (d *Daemon) CaptureSummary(ctx context.Context, runID string) (*inspection.RunCapture, error) {
	if _, err := d.runs.Get(ctx, runID); err != nil {
		return nil, err
	}
	if d.inspection == nil {
		return inspection.UnavailableCapture(runID), nil
	}
	capture, err := d.inspection.Capture(ctx, runID)
	if errors.Is(err, inspection.ErrNotConfigured) {
		return inspection.UnavailableCapture(runID), nil
	}
	if err != nil {
		return nil, err
	}
	return capture, nil
}

// ListCaptureRequests implements api.Backend.
func (d *Daemon) ListCaptureRequests(ctx context.Context, runID string, afterID int64, limit int) ([]inspection.ExchangeSummary, error) {
	if _, err := d.runs.Get(ctx, runID); err != nil {
		return nil, err
	}
	if d.inspection == nil {
		return []inspection.ExchangeSummary{}, nil
	}
	return d.inspection.List(ctx, runID, afterID, limit)
}

// GetCaptureRequest implements api.Backend.
func (d *Daemon) GetCaptureRequest(ctx context.Context, runID, requestID string) (*inspection.Exchange, error) {
	if _, err := d.runs.Get(ctx, runID); err != nil {
		return nil, err
	}
	if d.inspection == nil {
		return nil, inspection.ErrNotFound
	}
	return d.inspection.Get(ctx, runID, requestID)
}

// maxAmbiguousRuns bounds how many run ids an ambiguity error names, so a
// pathological set of matches cannot turn an error message into a flood.
const maxAmbiguousRuns = 5

// GetCaptureRequestByID implements api.Backend.
//
// The owning run is read from the stored row, never parsed out of the id: the id
// is SDK-supplied, so a prefix or any other client convention would be a claim
// rather than a fact. Because request id uniqueness is only enforced per run, an
// id recorded by more than one run is reported as ambiguous -- the reader has to
// name the run rather than have one guessed for them.
func (d *Daemon) GetCaptureRequestByID(ctx context.Context, requestID string) (*inspection.Exchange, error) {
	if d.inspection == nil {
		return nil, inspection.ErrNotFound
	}
	matches, err := d.inspection.FindByRequestID(ctx, requestID)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, inspection.ErrNotFound
	case 1:
		return matches[0], nil
	}

	runIDs := make([]string, 0, maxAmbiguousRuns)
	for _, match := range matches {
		if len(runIDs) == maxAmbiguousRuns {
			runIDs = append(runIDs, "...")
			break
		}
		runIDs = append(runIDs, match.RunID)
	}
	return nil, fmt.Errorf("%w: request id %q appears in runs %s; pass the run id",
		inspection.ErrAmbiguous, requestID, strings.Join(runIDs, ", "))
}

// beginCapture records the resolved capture policy for a run. It is called at
// submission, before any child exists, so that a run which observes nothing is
// still distinguishable from a run that predates capture.
func (d *Daemon) beginCapture(runID, integrationID string, policy inspection.Policy) {
	if d.inspection == nil || !policy.Valid() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := d.inspection.Begin(ctx, inspection.CaptureSettings{
		RunID:         runID,
		IntegrationID: integrationID,
		Policy:        policy,
	})
	if err != nil {
		// Capture is diagnostic: failing to record must not fail a submission.
		d.log.Warn("capture_begin_failed", "run_id", runID, "error", err.Error())
		return
	}
	if policy.Enabled() {
		d.log.Debug("capture_started", "run_id", runID, "policy", policy.String())
	}
}

// finalizeCapture records how complete a run's recording is once its child has
// exited.
//
// The daemon, not the child, decides whether the recording was truncated: a
// process killed by a signal never gets to report anything, and a process that
// exits without reporting leaves its recording pending. A child that did report
// a clean finish is left alone, because the daemon has nothing better to add.
func (d *Daemon) finalizeCapture(runID string, res *executor.Result, reason cancelReason) {
	if d.inspection == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	killed := true
	var note string
	switch {
	case res == nil:
		note = "the run produced no execution result; capture may be incomplete"
	case res.StartError != nil:
		note = "the run never started; no capture was observed"
	case res.TimedOut:
		note = "the run exceeded its timeout and was terminated; capture may be incomplete"
	case res.Cancelled:
		note = "the run was cancelled and terminated; capture may be incomplete"
	case reason == reasonShutdown:
		note = "the daemon shut down during execution; capture may be incomplete"
	default:
		killed = false
	}

	var err error
	if killed {
		err = d.inspection.Finalize(ctx, runID, inspection.FinalizationAbrupt, note)
	} else {
		err = d.inspection.FinalizeUnreported(ctx, runID,
			"the process exited without reporting a complete recording")
	}
	if err != nil {
		d.log.Warn("capture_finalize_failed", "run_id", runID, "error", err.Error())
	}
}

// runCaptureRetention marks recordings left pending by a previous process and
// then prunes expired payloads on a timer. Both halves are bounded: cleanup must
// never hold the daemon's single database connection for long.
func (d *Daemon) runCaptureRetention(ctx context.Context) {
	if d.inspection == nil {
		return
	}

	// Nothing can still be executing before the workers start, so any recording
	// left pending belongs to a process that died without finalizing.
	if n, err := d.inspection.MarkStalePending(ctx, d.startedAt); err != nil {
		d.log.Warn("capture_stale_mark_failed", "error", err.Error())
	} else if n > 0 {
		d.log.Info("capture_stale_marked", "runs", n)
	}

	if d.cfg.CaptureRetention <= 0 {
		d.log.Info("capture_retention_disabled")
		return
	}

	ticker := time.NewTicker(captureRetentionInterval)
	defer ticker.Stop()
	for {
		d.expireCapture(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
		}
	}
}

// expireCapture removes expired payloads in bounded batches so one sweep cannot
// monopolise the connection.
func (d *Daemon) expireCapture(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-d.cfg.CaptureRetention)
	for i := 0; i < 100; i++ {
		expired, deleted, err := d.inspection.ExpireOlderThan(ctx, cutoff, 50)
		if err != nil {
			d.log.Warn("capture_expiry_failed", "error", err.Error())
			return
		}
		if expired == 0 {
			return
		}
		d.log.Info("capture_expired", "runs", expired, "requests", deleted)
	}
}
