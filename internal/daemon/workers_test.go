package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
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

// retryableManifest allows three attempts, so the retry policy has room for a
// successor after the first failure.
const retryableManifest = `
version: 1
name: job
entrypoint: main.py
timeout: 30
retry:
  attempts: 3
  backoff: none
`

// runningRun builds the record executeRun leaves behind once an attempt has
// started.
func runningRun(id, integrationID, integrationName string, attempt int, startedAt time.Time) *runs.Run {
	return &runs.Run{
		ID:              id,
		IntegrationID:   integrationID,
		IntegrationName: integrationName,
		TriggerType:     runs.TriggerManual,
		Status:          runs.StatusRunning,
		Attempt:         attempt,
		CreatedAt:       startedAt,
		StartedAt:       &startedAt,
	}
}

// resolvedManifest returns the manifest the daemon resolved for integrationID.
func resolvedManifest(t *testing.T, d *Daemon, integrationID string) *config.Manifest {
	t.Helper()
	entry, ok := d.reg.get(integrationID)
	if !ok || entry.Manifest == nil {
		t.Fatalf("no manifest for integration %s", integrationID)
	}
	return entry.Manifest
}

// breakFinishCommit makes every finish transaction commit fail. It is the
// injection seam for a transactional failure, the same technique commitQueueTx
// uses in the queue package: an injected failure is the only way to exercise
// the all-or-nothing path against a healthy SQLite file.
func breakFinishCommit(t *testing.T) func() {
	t.Helper()
	prev := finishCommit
	finishCommit = func(*sql.Tx) error { return errors.New("injected commit failure") }
	restore := func() { finishCommit = prev }
	t.Cleanup(restore)
	return restore
}

// TestFinishRunPersistsOutcomeAndSuccessorAtomically proves the happy path of
// the one-transaction design: the terminal state and the successor attempt
// (with its queue row) both appear.
func TestFinishRunPersistsOutcomeAndSuccessorAtomically(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", retryableManifest, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	jobID := runtimeID(t, d, "job")
	m := resolvedManifest(t, d, jobID)

	run := runningRun("finish-atomic", jobID, "job", 1, time.Now().UTC().Add(-time.Second))
	if err := d.runs.Create(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	d.finishRun(run, m, runs.Finish{Status: runs.StatusFailed, Error: "boom"}, true)

	stored, err := d.runs.Get(ctx, "finish-atomic")
	if err != nil {
		t.Fatalf("get finished run: %v", err)
	}
	if stored.Status != runs.StatusFailed || stored.FinishedAt == nil {
		t.Fatalf("finished run = %s (finished %v), want failed with finished_at",
			stored.Status, stored.FinishedAt)
	}

	successors, err := d.runs.List(ctx, runs.Filter{ParentRunID: "finish-atomic"})
	if err != nil {
		t.Fatalf("list successors: %v", err)
	}
	if len(successors) != 1 {
		t.Fatalf("successors = %d, want 1", len(successors))
	}
	next := successors[0]
	if next.Attempt != 2 || next.Status != runs.StatusRetrying {
		t.Errorf("successor = attempt %d %s, want attempt 2 retrying", next.Attempt, next.Status)
	}
	if next.ParentRunID == nil || *next.ParentRunID != "finish-atomic" {
		t.Errorf("successor parent = %v, want finish-atomic", next.ParentRunID)
	}

	queued, err := d.queue.Contains(ctx, next.ID)
	if err != nil {
		t.Fatalf("queue contains: %v", err)
	}
	if !queued {
		t.Error("the successor must be queued in the same transaction as the terminal write")
	}

	if recs, err := d.readFinishJournal(); err != nil {
		t.Fatalf("read journal: %v", err)
	} else if len(recs) != 0 {
		t.Errorf("a committed finish must leave no fallback record, got %+v", recs)
	}
}

// TestFinishCommitFailureLeavesNoPartialOutcome is the regression test for the
// crash between finish and retry. The terminal write and the successor are one
// transaction, so a failure at commit leaves the attempt exactly as it was --
// still running, with no successor -- instead of a terminal failure whose retry
// is missing.
func TestFinishCommitFailureLeavesNoPartialOutcome(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", retryableManifest, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	jobID := runtimeID(t, d, "job")
	m := resolvedManifest(t, d, jobID)

	run := runningRun("interrupted", jobID, "job", 1, time.Now().UTC().Add(-time.Second))
	if err := d.runs.Create(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	restore := breakFinishCommit(t)
	d.finishRun(run, m, runs.Finish{Status: runs.StatusFailed, Error: "boom"}, true)
	restore()

	// Nothing partial survived the failed transaction.
	stored, err := d.runs.Get(ctx, "interrupted")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Status != runs.StatusRunning || stored.FinishedAt != nil {
		t.Errorf("run = %s (finished %v), want still running with no terminal state",
			stored.Status, stored.FinishedAt)
	}
	successors, err := d.runs.List(ctx, runs.Filter{ParentRunID: "interrupted"})
	if err != nil {
		t.Fatalf("list successors: %v", err)
	}
	if len(successors) != 0 {
		t.Errorf("a rolled-back finish must not leave a successor, got %d", len(successors))
	}
	depth, err := d.queue.Depth(ctx)
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if depth != 0 {
		t.Errorf("queue depth = %d, want 0 after a rolled-back finish", depth)
	}

	// Returning silently is forbidden: the decision must be durable so a
	// restart applies it instead of re-running the child.
	recs, err := d.readFinishJournal()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	rec, ok := recs["interrupted"]
	if !ok {
		t.Fatalf("a failed finish must leave a durable fallback; journal = %+v", recs)
	}
	if rec.Status != runs.StatusFailed || !rec.Retry {
		t.Errorf("fallback record = %+v, want failed with a retry", rec)
	}
}

// TestFallbackJournalIsAppliedOnRestart proves the durable fallback closes the
// loop. A successful child whose terminal write failed must come back as
// succeeded, never re-executed; a retryable failure must come back with its
// successor. Both runs are left `running` with only a journal record, exactly
// as a crash between the child's exit and the write would leave them.
func TestFallbackJournalIsAppliedOnRestart(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", retryableManifest, noopPython)

	dataDir := t.TempDir()
	jobID := identityIDFor(t, root, dataDir, "job")

	seed, err := database.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := database.Migrate(context.Background(), seed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := runs.NewStore(seed.DB)
	startedAt := time.Now().UTC().Add(-time.Minute)
	for _, id := range []string{"successful-child", "failed-child"} {
		if err := store.Create(context.Background(),
			runningRun(id, jobID, "job", 1, startedAt)); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	// The fallback the crashed daemon left behind.
	journal := &Daemon{cfg: config.DaemonConfig{DataDir: dataDir}}
	for _, rec := range []finishRecord{
		{RunID: "successful-child", Status: runs.StatusSucceeded, FinishedAt: startedAt},
		{RunID: "failed-child", Status: runs.StatusFailed, Error: "boom", Retry: true, FinishedAt: startedAt},
	} {
		rec.RecordedAt = time.Now().UTC()
		if err := journal.appendFinishRecord(rec); err != nil {
			t.Fatalf("write fallback record for %s: %v", rec.RunID, err)
		}
	}

	// Constructing a daemon performs recovery, which consumes the journal.
	d := newDaemon(t, root, dataDir, nil, nil)
	ctx := context.Background()

	success, err := d.runs.Get(ctx, "successful-child")
	if err != nil {
		t.Fatalf("get successful-child: %v", err)
	}
	if success.Status != runs.StatusSucceeded {
		t.Errorf("successful-child = %s, want succeeded: a successful child must never be re-run",
			success.Status)
	}
	successors, err := d.runs.List(ctx, runs.Filter{ParentRunID: "successful-child"})
	if err != nil {
		t.Fatalf("list successors: %v", err)
	}
	if len(successors) != 0 {
		t.Errorf("successful-child gained %d successors; a success is not retried", len(successors))
	}

	failed, err := d.runs.Get(ctx, "failed-child")
	if err != nil {
		t.Fatalf("get failed-child: %v", err)
	}
	if failed.Status != runs.StatusFailed {
		t.Errorf("failed-child = %s, want failed", failed.Status)
	}
	if failed.ErrorString() != "boom" {
		t.Errorf("failed-child error = %q, want the recorded failure, not a crash message",
			failed.ErrorString())
	}
	successors, err = d.runs.List(ctx, runs.Filter{ParentRunID: "failed-child"})
	if err != nil {
		t.Fatalf("list successors: %v", err)
	}
	if len(successors) != 1 || successors[0].Status != runs.StatusRetrying {
		t.Fatalf("failed-child successors = %+v, want one retrying attempt", successors)
	}

	// The consumed records must not be replayed on the next restart.
	remaining, err := d.readFinishJournal()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("journal still holds %+v after recovery applied it", remaining)
	}
}

// TestUnreadableFallbackJournalDoesNotReRunRunningRuns pins the rule.
// Recovery cannot tell an interrupted child from one whose outcome is recorded
// in a journal it cannot read, so it must leave the run alone rather than risk
// executing a successful child twice.
//
// The startup refusal this triggers on its own is pinned separately by
// TestStartupFailClosedOnIncompleteRecovery; the override is what lets this
// test observe the run state the hold protects.
func TestUnreadableFallbackJournalDoesNotReRunRunningRuns(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "job", retryableManifest, noopPython)

	dataDir := t.TempDir()
	jobID := identityIDFor(t, root, dataDir, "job")

	seed, err := database.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := database.Migrate(context.Background(), seed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := runs.NewStore(seed.DB).Create(context.Background(),
		runningRun("running-child", jobID, "job", 1, time.Now().UTC().Add(-time.Minute))); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	// A directory where the journal file belongs makes every read fail.
	if err := os.Mkdir(filepath.Join(dataDir, finishJournalName), 0o700); err != nil {
		t.Fatalf("occupy journal path: %v", err)
	}

	d := newDaemon(t, root, dataDir, nil, func(cfg *config.DaemonConfig) {
		cfg.AllowIncompleteRecovery = true
	})
	ctx := context.Background()

	run, err := d.runs.Get(ctx, "running-child")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != runs.StatusRunning {
		t.Errorf("run = %s, want it left running when the journal cannot be read", run.Status)
	}
	successors, err := d.runs.List(ctx, runs.Filter{ParentRunID: "running-child"})
	if err != nil {
		t.Fatalf("list successors: %v", err)
	}
	if len(successors) != 0 {
		t.Errorf("an unreadable journal must not schedule a retry, got %d successors", len(successors))
	}
}
