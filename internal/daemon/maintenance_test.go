package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/runs"
)

// newGatedDaemon builds a daemon that started in maintenance, which is the
// pooled lifecycle: start gated, validate, activate deliberately.
func newGatedDaemon(t *testing.T, root string) *Daemon {
	t.Helper()
	return newDaemon(t, root, "", nil, func(cfg *config.DaemonConfig) {
		cfg.StartInMaintenance = true
	})
}

// Every path that admits work is closed, not just the autonomous ones. A pause
// lets a manual run through; maintenance is a property of the runtime, so it
// does not -- that difference is the whole reason it is a separate mechanism.
func TestMaintenanceRefusesEveryAdmissionPath(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newGatedDaemon(t, root)
	ctx := context.Background()

	if view, err := d.Maintenance(ctx); err != nil || view.AcceptingWork {
		t.Fatalf("a gated runtime reports accepting_work: %+v (%v)", view, err)
	}

	for _, trigger := range []string{api.TriggerManual, api.TriggerCron, api.TriggerWebhook} {
		_, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: trigger})
		if !errors.Is(err, api.ErrGated) {
			t.Errorf("%s trigger while gated = %v, want ErrGated", trigger, err)
		}
	}

	// Activation admits work again, which is what makes the gate a window
	// rather than a retirement.
	view, err := d.ExitMaintenance(ctx)
	if err != nil {
		t.Fatalf("ExitMaintenance: %v", err)
	}
	if !view.AcceptingWork || view.Mode != "serving" {
		t.Errorf("after activation: %+v", view)
	}
	for _, trigger := range []string{api.TriggerManual, api.TriggerCron, api.TriggerWebhook} {
		if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: trigger}); err != nil {
			t.Errorf("%s trigger after activation: %v", trigger, err)
		}
	}
}

// Entering maintenance is idempotent and reports the resulting mode, and the
// mode it reports is the honest one: work may still be executing, so it is
// draining rather than maintenance.
func TestEnterMaintenanceReportsDrainingAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	first, err := d.EnterMaintenance(ctx, "planned upgrade")
	if err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	// The mode is derived from the live running count, so with nothing running
	// this is already maintenance. That is the honest answer and the one an
	// operator snapshots on -- see TestDrainTransitionsOnlyWhenNothingIsRunning
	// for the other side, where a held slot keeps it draining.
	if first.Mode != "maintenance" {
		t.Errorf("mode = %q, want maintenance (nothing is running)", first.Mode)
	}
	if first.AcceptingWork {
		t.Error("a draining runtime reports accepting work")
	}
	if first.Reason != "planned upgrade" {
		t.Errorf("reason = %q, want the caller's", first.Reason)
	}
	if first.Since == "" {
		t.Error("the view does not say when the window began")
	}

	// A repeated call is a no-op but still reports the state, so a deploy
	// script can call it unconditionally.
	second, err := d.EnterMaintenance(ctx, "retry")
	if err != nil {
		t.Fatalf("second EnterMaintenance: %v", err)
	}
	if second.Since != first.Since {
		t.Errorf("the window moved from %s to %s on a repeat", first.Since, second.Since)
	}
	if second.Reason != "planned upgrade" {
		t.Errorf("reason = %q, want the original to survive a repeat", second.Reason)
	}
}

// Queued work is durable and unclaimed across the gate: entering maintenance
// must not discard accepted work, and activating must let it run. This is what
// makes the gate safe to use around a snapshot instead of a shutdown.
func TestMaintenanceKeepsQueuedWorkDurableAndUnclaimed(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	// Accept two runs while serving, then gate. Nothing drains them: no worker
	// is running in this harness, which is exactly the state a drain sees
	// before the children are gone.
	for i := 0; i < 2; i++ {
		if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual}); err != nil {
			t.Fatalf("seed run %d: %v", i+1, err)
		}
	}
	if _, err := d.EnterMaintenance(ctx, "snapshot"); err != nil {
		t.Fatal(err)
	}

	queued, err := d.runs.ListByStatus(ctx, runs.StatusQueued, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 {
		t.Fatalf("queued work after entering maintenance = %d, want the 2 that were accepted", len(queued))
	}

	// A new submission is refused while the accepted work stays put.
	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual}); !errors.Is(err, api.ErrGated) {
		t.Errorf("submission while gated = %v, want ErrGated", err)
	}
	if _, err := d.ExitMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	stillQueued, err := d.runs.ListByStatus(ctx, runs.StatusQueued, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillQueued) != 2 {
		t.Errorf("queued work after activation = %d, want the same 2", len(stillQueued))
	}
}

// Leaving maintenance must not silently unpause anything. The two mechanisms
// are independent: an operator activating a runtime has said nothing about a
// job they paused, and clearing it for them would be a surprise.
func TestActivationPreservesJobPause(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	if _, err := d.SetPaused(ctx, "ticker", true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := d.EnterMaintenance(ctx, "window"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExitMaintenance(ctx); err != nil {
		t.Fatal(err)
	}

	got, ok := d.GetJob("ticker")
	if !ok {
		t.Fatal("the job disappeared")
	}
	if !got.Triggers.Paused {
		t.Error("activating the runtime resumed a job the operator had paused")
	}
	// And the pause still refuses autonomous triggers, so the two gates did not
	// merge into one.
	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerCron}); err == nil {
		t.Error("an autonomous trigger was admitted for a paused job after activation")
	}
}

// A drain is not finished while something is still executing, and the state
// must not claim otherwise: an operator snapshots a data directory on the
// strength of this answer.
func TestDrainTransitionsOnlyWhenNothingIsRunning(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	if _, err := d.EnterMaintenance(ctx, "window"); err != nil {
		t.Fatal(err)
	}
	// With no worker pool started there is nothing running, so the drain has
	// nothing to wait for and the view must already say maintenance.
	if view, _ := d.Maintenance(ctx); view.Mode != "maintenance" {
		t.Errorf("mode with nothing running = %q, want maintenance", view.Mode)
	}

	// Simulate a run still executing by holding a capacity slot.
	d.cap.Reserve("job-held")
	if view, _ := d.Maintenance(ctx); view.Mode != "draining" || view.Running != 1 {
		t.Errorf("with one run held: mode=%q running=%d, want draining/1", view.Mode, view.Running)
	}
	d.cap.Release("job-held")
	if view, _ := d.Maintenance(ctx); view.Mode != "maintenance" {
		t.Errorf("after the slot was released: mode=%q, want maintenance", view.Mode)
	}
}

// A gate set by a start is not an operator's window, and an operator entering
// maintenance takes ownership of it: their reason and their clock replace the
// boot's, so "held back for 40 minutes" means what it says.
func TestOperatorWindowReplacesAStartGate(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newGatedDaemon(t, root)
	ctx := context.Background()

	booted, err := d.Maintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if booted.Explicit {
		t.Error("a start gate reported itself as an operator's decision")
	}

	// Move the recorded start back so a replaced window is distinguishable.
	time.Sleep(1100 * time.Millisecond)

	entered, err := d.EnterMaintenance(ctx, "operator window")
	if err != nil {
		t.Fatal(err)
	}
	if !entered.Explicit {
		t.Error("an operator's window did not record itself as explicit")
	}
	if entered.Reason != "operator window" {
		t.Errorf("reason = %q, want the operator's", entered.Reason)
	}
	if entered.Since == booted.Since {
		t.Error("the window still reports the boot time rather than the operator's")
	}
}

// retryingJob fails on purpose so the retry producer has something to build a
// successor from.
const retryingJob = `
version: 1
name: retrying
entrypoint: main.py
timeout: 30
concurrency: 1
trigger:
  cron: "@every 6h"
retry:
  attempts: 3
  backoff: linear
  initial_delay: 1s
  max_delay: 2s
`

const failingPython = `import sys
sys.exit(1)
`

// A retry producer runs while a child finishes, which is exactly the moment a
// drain is in progress. The successor may be recorded -- it derives from work
// accepted before the gate -- but it must not execute, or maintenance would not
// actually stop work.
//
// The clock is what makes this testable: the successor waits out its backoff,
// so a worker that still claimed would run it within the delay.
func TestRetryIsRecordedDuringADrainButDoesNotExecute(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "retrying", retryingJob, failingPython)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)
	ctx := context.Background()

	runID, err := d.SubmitRun(ctx, "retrying", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Wait for the first attempt to fail and produce its successor. Gate as
	// soon as the retrying state appears, which is while the successor is still
	// waiting out its backoff.
	deadline := time.Now().Add(60 * time.Second)
	var successorSeen bool
	for time.Now().Before(deadline) {
		retrying, err := d.runs.ListByStatus(ctx, runs.StatusRetrying, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(retrying) > 0 {
			successorSeen = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !successorSeen {
		t.Fatalf("no retry successor appeared for run %s; the fixture did not exercise the retry producer", runID)
	}

	if _, err := d.EnterMaintenance(ctx, "drain with a pending retry"); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}

	// Hold the gate well past the retry delay. If the worker were still
	// claiming, the parked retry would have run and failed again by now.
	time.Sleep(3 * time.Second)

	afterRetrying, err := d.runs.ListByStatus(ctx, runs.StatusRetrying, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRetrying) != 1 {
		t.Fatalf("retrying runs after the window = %d, want the 1 recorded successor", len(afterRetrying))
	}
	running, err := d.runs.ListByStatus(ctx, runs.StatusRunning, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 {
		t.Errorf("%d runs executed after the gate; maintenance did not stop work", len(running))
	}
	succeeded, err := d.runs.ListByStatus(ctx, runs.StatusSucceeded, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(succeeded) != 0 {
		t.Errorf("a run succeeded during maintenance: %v", succeeded)
	}

	// The successor is durable, not discarded: leaving maintenance lets it run.
	if _, err := d.ExitMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	d.notifyWorkers()
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		left, err := d.runs.ListByStatus(ctx, runs.StatusRetrying, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) == 0 {
			return // the parked retry was picked up and advanced
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("the recorded retry never resumed after activation")
}

// Maintenance and the work it protects survive a restart. A gate that cleared
// on restart would make the window meaningless, and work accepted before it
// would be lost rather than waiting.
func TestMaintenanceAndQueuedWorkSurviveARestart(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	dataDir := t.TempDir()
	d := newDaemon(t, root, dataDir, nil, nil)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual}); err != nil {
			t.Fatalf("seed run %d: %v", i+1, err)
		}
	}
	if _, err := d.EnterMaintenance(ctx, "restart window"); err != nil {
		t.Fatal(err)
	}

	// A second daemon over the same data directory is what a restart is, but
	// the first must release the data lock first: leaving it held would be a
	// second process running, not a restart.
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	restarted := newDaemon(t, root, dataDir, nil, nil)

	view, err := restarted.Maintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.AcceptingWork {
		t.Error("the gate did not survive the restart")
	}
	if view.Reason != "restart window" {
		t.Errorf("reason after restart = %q, want the operator's to survive", view.Reason)
	}

	// Queued work is still there, and still refused a new submission.
	queued, err := restarted.runs.ListByStatus(ctx, runs.StatusQueued, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 {
		t.Errorf("queued runs after restart = %d, want the 2 accepted before it", len(queued))
	}
	if _, err := restarted.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual}); !errors.Is(err, api.ErrGated) {
		t.Errorf("submission after restart = %v, want ErrGated", err)
	}
}
