package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/database"
	"github.com/tkoizumi/otter/internal/queue"
	"github.com/tkoizumi/otter/internal/runs"
)

// The health signals WS4 adds are only useful if they are exact: an age that is
// roughly right cannot distinguish "busy" from "stuck since yesterday". This
// seeds a queue with a known-old submission, a retry parked in the future and a
// later failure after a success, then asserts every number and instant.
func TestHealthSignalsReportQueueAgeDepthRetriesAndFreshness(t *testing.T) {
	d := newHealthDaemon(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	oldest := now.Add(-11 * time.Minute)
	newer := now.Add(-time.Minute)
	nextRetry := now.Add(5 * time.Minute)
	jobASuccess := now.Add(-30 * time.Minute)
	jobBSuccess := now.Add(-3 * time.Minute)

	// Two claimable runs for job-a: one submitted eleven minutes ago, one a
	// minute ago. Only the first is the queue's age.
	seedHealthRun(t, d, "a-old", "job-a", runs.StatusQueued, time.Time{})
	seedHealthQueue(t, d, "a-old", "job-a", oldest, oldest)
	seedHealthRun(t, d, "a-new", "job-a", runs.StatusQueued, time.Time{})
	seedHealthQueue(t, d, "a-new", "job-a", newer, newer)

	// A retry parked five minutes into the future. It is submitted later than
	// a-old, so it must not become the queue's age, and it is not claimable.
	seedHealthRun(t, d, "b-retry", "job-b", runs.StatusRetrying, time.Time{})
	seedHealthQueue(t, d, "b-retry", "job-b", nextRetry, now.Add(-2*time.Minute))

	// Freshness: job-a's last success predates a later failure; job-b succeeded
	// recently; job-c has never succeeded.
	seedHealthRun(t, d, "a-done", "job-a", runs.StatusSucceeded, jobASuccess)
	seedHealthRun(t, d, "a-failed", "job-a", runs.StatusFailed, now.Add(-5*time.Minute))
	seedHealthRun(t, d, "b-done", "job-b", runs.StatusSucceeded, jobBSuccess)
	seedHealthRun(t, d, "c-failed", "job-c", runs.StatusFailed, now.Add(-time.Hour))

	stats, err := d.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats() error = %v", err)
	}
	wantByJob := map[string]int{"job-a": 2, "job-b": 1}
	if len(stats.ByJob) != len(wantByJob) || stats.ByJob["job-a"] != 2 || stats.ByJob["job-b"] != 1 {
		t.Errorf("ByJob = %v, want %v", stats.ByJob, wantByJob)
	}
	if stats.OldestWaitingAt == nil {
		t.Fatal("OldestWaitingAt is nil, want the run submitted eleven minutes ago")
	}
	if !stats.OldestWaitingAt.Equal(oldest) {
		t.Errorf("OldestWaitingAt = %s, want %s", stats.OldestWaitingAt, oldest)
	}
	if stats.Retrying != 1 {
		t.Errorf("Retrying = %d, want 1", stats.Retrying)
	}
	if stats.NextRetryAt == nil {
		t.Fatal("NextRetryAt is nil, want the parked retry")
	}
	if !stats.NextRetryAt.Equal(nextRetry) {
		t.Errorf("NextRetryAt = %s, want %s", stats.NextRetryAt, nextRetry)
	}

	fresh, err := d.LastSuccessByJob(ctx)
	if err != nil {
		t.Fatalf("LastSuccessByJob() error = %v", err)
	}
	if len(fresh) != 2 {
		t.Fatalf("LastSuccessByJob() = %v, want only job-a and job-b", fresh)
	}
	if got := fresh["job-a"]; !got.Equal(jobASuccess) {
		t.Errorf("job-a last success = %s, want %s (the later failure is not a success)", got, jobASuccess)
	}
	if got := fresh["job-b"]; !got.Equal(jobBSuccess) {
		t.Errorf("job-b last success = %s, want %s", got, jobBSuccess)
	}
	if _, ok := fresh["job-c"]; ok {
		t.Error("job-c has never succeeded and must be absent")
	}
}

// The views carry freshness so `otter jobs --schedule` can render it without a
// run listing per job. It is attached by the listing itself, from one grouped
// read, so a view cannot be served stale by a caller that forgot to ask.
func TestListJobsCarriesLastSuccess(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "carry", manifestFor("shared"), noopPython)
	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	views := d.ListJobs(ctx)
	if len(views) != 1 {
		t.Fatalf("ListJobs() = %+v, want one view", views)
	}
	jobID := views[0].ID
	if views[0].LastSuccessAt != nil {
		t.Fatalf("a job with no run claims a last success: %s", views[0].LastSuccessAt)
	}

	succeeded := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	seedHealthRun(t, d, "run-1", jobID, runs.StatusSucceeded, succeeded)

	views = d.ListJobs(ctx)
	if len(views) != 1 {
		t.Fatalf("ListJobs() = %+v, want one view", views)
	}
	if views[0].LastSuccessAt == nil {
		t.Fatal("LastSuccessAt is nil, want the job's last success")
	}
	if !views[0].LastSuccessAt.Equal(succeeded) {
		t.Errorf("LastSuccessAt = %s, want %s", views[0].LastSuccessAt, succeeded)
	}
}

// Storage is reported from the daemon's own handle, so a remote operator can
// read it through the API instead of needing a shell on the host.
func TestStorageStatsReportDatabaseAndDisk(t *testing.T) {
	d := newHealthDaemon(t)
	ctx := context.Background()
	seedHealthRun(t, d, "run-1", "job-a", runs.StatusSucceeded, time.Now().UTC())

	stats, err := d.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats() error = %v", err)
	}
	if stats.DBBytes <= 0 {
		t.Errorf("DBBytes = %d, want the migrated database's size", stats.DBBytes)
	}
	if stats.DiskTotalBytes <= 0 {
		t.Errorf("DiskTotalBytes = %d, want the data directory's filesystem size", stats.DiskTotalBytes)
	}
	if stats.DiskFreeBytes <= 0 || stats.DiskFreeBytes > stats.DiskTotalBytes {
		t.Errorf("DiskFreeBytes = %d, want a positive value no larger than %d", stats.DiskFreeBytes, stats.DiskTotalBytes)
	}
}

// newHealthDaemon is the smallest daemon the health signals need: a migrated
// database, the run store and the queue.
func newHealthDaemon(t *testing.T) *Daemon {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	db, err := database.Open(ctx, dir)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("database.Migrate() error = %v", err)
	}
	return &Daemon{
		cfg:   config.DaemonConfig{DataDir: dir},
		db:    db,
		runs:  runs.NewStore(db.DB),
		queue: queue.New(db.DB),
		log:   testLogger(),
	}
}

// seedHealthRun writes a run row. A run that is not in a terminal state is
// created in that state directly; a terminal one is finished with an explicit
// instant, which is what freshness reads.
func seedHealthRun(t *testing.T, d *Daemon, id, jobID string, status runs.Status, finished time.Time) {
	t.Helper()
	ctx := context.Background()
	run := &runs.Run{
		ID:          id,
		JobID:       jobID,
		TriggerType: runs.TriggerManual,
		Status:      status,
		Attempt:     1,
		CreatedAt:   time.Now().UTC(),
	}
	if status.Terminal() {
		run.Status = runs.StatusRunning
	}
	if err := d.runs.Create(ctx, run); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
	if !status.Terminal() {
		return
	}
	if err := d.runs.Finish(ctx, id, runs.Finish{Status: status, FinishedAt: finished}); err != nil {
		t.Fatalf("finish run %s: %v", id, err)
	}
}

// seedHealthQueue writes a queue row with explicit times, because Enqueue
// always records the submission as "now".
func seedHealthQueue(t *testing.T, d *Daemon, runID, jobID string, availableAt, createdAt time.Time) {
	t.Helper()
	if _, err := d.db.ExecContext(context.Background(),
		`INSERT INTO run_queue (run_id, job_id, available_at, created_at) VALUES (?, ?, ?, ?)`,
		runID, jobID, database.FormatTime(availableAt), database.FormatTime(createdAt)); err != nil {
		t.Fatalf("seed queue row %s: %v", runID, err)
	}
}
