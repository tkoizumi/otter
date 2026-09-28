package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/queue"
	"github.com/tkoizumi/otter/internal/runs"
)

// claimFailureBackoffFloor is the smallest backoff the requeue tests accept on
// a re-enqueued run. A re-enqueue with no delay would let a persistent database
// fault spin a worker, so a floor -- not an exact value -- is what the tests
// assert.
const claimFailureBackoffFloor = time.Second

// claimForTest creates one queued run for integrationID and claims it, which is
// the state executeRun sees in production: the run_queue row is gone and a
// worker slot is reserved.
func claimForTest(t *testing.T, d *Daemon, runID, integrationID string) *queue.Item {
	t.Helper()
	ctx := context.Background()

	run := &runs.Run{
		ID:            runID,
		IntegrationID: integrationID,
		TriggerType:   runs.TriggerManual,
		Status:        runs.StatusQueued,
		Attempt:       1,
		CreatedAt:     time.Now().UTC(),
	}
	if err := d.runs.Create(ctx, run); err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	if err := d.queue.Enqueue(ctx, runID, integrationID, time.Now().UTC()); err != nil {
		t.Fatalf("enqueue run %s: %v", runID, err)
	}

	item, err := d.queue.Claim(ctx, time.Now().UTC(), d.cap)
	if err != nil {
		t.Fatalf("claim run %s: %v", runID, err)
	}
	if item.RunID != runID {
		t.Fatalf("claimed %s, want %s", item.RunID, runID)
	}
	return item
}

// onlyQueued returns the single queue row, failing when the depth is not one.
func onlyQueued(t *testing.T, d *Daemon) queue.Item {
	t.Helper()

	items, err := d.queue.List(context.Background())
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("queue depth = %d, want 1 (items: %+v)", len(items), items)
	}
	return items[0]
}

// TestClaimedRunIsRequeuedWhenMarkRunningFails is the regression test for
// "claimed runs dropped". Claim has already deleted the run_queue row, so when
// the transition to running fails the attempt is left as queued with nothing to
// pick it up until the next daemon restart.
func TestClaimedRunIsRequeuedWhenMarkRunningFails(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", minimalManifest("job"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	jobID := runtimeID(t, d, "job")

	item := claimForTest(t, d, "claim-mark-running", jobID)

	// Fail only the UPDATE that starts an attempt. The record stays readable
	// and run_queue stays writable, which is the shape of a transient write
	// fault: Get succeeds and MarkRunning does not.
	if _, err := d.db.ExecContext(ctx,
		`CREATE TRIGGER refuse_mark_running BEFORE UPDATE ON runs
		 BEGIN SELECT RAISE(ABORT, 'injected mark-running failure'); END;`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}

	d.executeRun(item)

	requeued := onlyQueued(t, d)
	if requeued.RunID != item.RunID {
		t.Fatalf("requeued run = %s, want %s", requeued.RunID, item.RunID)
	}
	if !requeued.AvailableAt.After(time.Now().UTC().Add(claimFailureBackoffFloor)) {
		t.Errorf("requeued at %s, want a backoff of at least %s",
			requeued.AvailableAt, claimFailureBackoffFloor)
	}

	// The attempt never started, so the record must still be waiting rather
	// than running or failed.
	stored, err := d.runs.Get(ctx, item.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Status != runs.StatusQueued {
		t.Errorf("status = %s, want queued", stored.Status)
	}

	// The slot reserved by Claim must be given back.
	if got := d.cap.totalRunning(); got != 0 {
		t.Errorf("running slots = %d, want 0 after release", got)
	}
}

// TestClaimedRunIsRequeuedWhenItsRecordCannotBeRead covers the other claim
// failure: the record is there but the read failed. The attempt cannot start,
// and the run must not be lost between claims.
func TestClaimedRunIsRequeuedWhenItsRecordCannotBeRead(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", minimalManifest("job"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	jobID := runtimeID(t, d, "job")

	item := claimForTest(t, d, "claim-load", jobID)

	// Make the record unreadable without touching run_queue: rename the table
	// out from under the reader. Get fails, so the attempt cannot start.
	if _, err := d.db.ExecContext(ctx, `ALTER TABLE runs RENAME TO runs_hidden`); err != nil {
		t.Fatalf("hide runs table: %v", err)
	}

	d.executeRun(item)

	requeued := onlyQueued(t, d)
	if requeued.RunID != item.RunID {
		t.Fatalf("requeued run = %s, want %s", requeued.RunID, item.RunID)
	}
	if !requeued.AvailableAt.After(time.Now().UTC().Add(claimFailureBackoffFloor)) {
		t.Errorf("requeued at %s, want a backoff of at least %s",
			requeued.AvailableAt, claimFailureBackoffFloor)
	}
	if got := d.cap.totalRunning(); got != 0 {
		t.Errorf("running slots = %d, want 0 after release", got)
	}
}

// TestClaimedRunWithNoRecordIsNotRequeued pins the other half of the decision.
// A run whose record no longer exists cannot be terminaled -- Finish matches no
// row -- and must not be re-enqueued: that would manufacture a queue row every
// later claim could only fail on again.
func TestClaimedRunWithNoRecordIsNotRequeued(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", minimalManifest("job"), noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	jobID := runtimeID(t, d, "job")

	// A queue row with no run record: a delete raced the claim.
	if err := d.queue.Enqueue(ctx, "ghost-run", jobID, time.Now().UTC()); err != nil {
		t.Fatalf("enqueue ghost: %v", err)
	}
	item, err := d.queue.Claim(ctx, time.Now().UTC(), d.cap)
	if err != nil {
		t.Fatalf("claim ghost: %v", err)
	}

	d.executeRun(item)

	depth, err := d.queue.Depth(ctx)
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if depth != 0 {
		t.Errorf("queue depth = %d, want 0: a record that no longer exists must not be re-enqueued", depth)
	}
	if got := d.cap.totalRunning(); got != 0 {
		t.Errorf("running slots = %d, want 0 after release", got)
	}
}
