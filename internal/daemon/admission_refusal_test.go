package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/secrets"
)

// A scheduled occurrence whose job has no active release used to leave no trace
// at all: no run, no queue entry, and an error only the daemon's log ever saw.
//
// That is not a hypothetical. On a pooled host it is what a managed-Python job
// whose environment could not be prepared looks like — the job registers, the
// schedule imports, `next_run_at` advances, and nothing ever runs. The operator
// sees a healthy job that is silently doing nothing, because the one signal is a
// log line on a machine they are not reading.
//
// The refusal is now recorded in the same accounting `/health` already publishes
// for max_queue_depth refusals, and it carries its reason.
func TestARefusedOccurrenceIsRecordedNotJustLogged(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "unreleased", `
version: 1
name: unreleased
entrypoint: main.py
timeout: 30
trigger:
  cron: "*/5 * * * *"
`, `
print("this job has never been released")
`)

	// Nothing is released, which is the state the refusal is about.
	d := newDaemonUnreleased(t, root, "", secrets.NewStaticProvider(map[string]string{}), nil)
	startDaemon(t, d)

	ctx := context.Background()
	occurrence := time.Now().UTC()

	_, err := d.SubmitRun(ctx, "unreleased", api.TriggerPayload{
		Type:        api.TriggerCron,
		ScheduledAt: &occurrence,
	})
	if err == nil {
		t.Fatal("a job with no active release should refuse a scheduled occurrence")
	}
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}

	refusals, err := d.AdmissionRefusals(ctx)
	if err != nil {
		t.Fatalf("admission refusals: %v", err)
	}
	if len(refusals) != 1 {
		t.Fatalf("refusals = %v, want exactly one job recorded", refusals)
	}
	for jobID, entry := range refusals {
		if entry.Total != 1 {
			t.Errorf("job %s: total = %d, want 1", jobID, entry.Total)
		}
		if entry.Reason != "no_active_release" {
			t.Errorf("job %s: reason = %q, want no_active_release; a count without a reason does not say what to fix",
				jobID, entry.Reason)
		}
		if entry.LastAt == nil {
			t.Errorf("job %s: no last_refused_at", jobID)
		}
	}

	// A MANUAL run is refused the same way but is NOT counted: the caller gets
	// the error synchronously and can act on it, so there is nothing silent to
	// make visible. Counting it would double-report a refusal an operator
	// already saw.
	if _, err := d.SubmitRun(ctx, "unreleased", api.TriggerPayload{Type: api.TriggerManual}); err == nil {
		t.Fatal("a manual run against an unreleased job should also be refused")
	}
	after, err := d.AdmissionRefusals(ctx)
	if err != nil {
		t.Fatalf("admission refusals: %v", err)
	}
	for jobID, entry := range after {
		if entry.Total != 1 {
			t.Errorf("job %s: total = %d after a manual refusal, want it unchanged at 1", jobID, entry.Total)
		}
	}
}

// corruptActiveRelease replaces the manifest INSIDE a job's active release with
// one LoadAndValidate rejects. The live manifest that discovery reads is left
// alone, so the occurrence reaches admission and fails there -- for a reason
// submitRun does not classify, which is the shape this accounting was missing.
func corruptActiveRelease(t *testing.T, d *Daemon, jobID string) {
	t.Helper()

	dir, _, ok, err := release.ActiveSourceDir(d.cfg.DataDir, jobID)
	if err != nil {
		t.Fatalf("resolve active release for %s: %v", jobID, err)
	}
	if !ok {
		t.Fatalf("job %s has no active release to corrupt", jobID)
	}
	path := filepath.Join(dir, config.ManifestFileName)
	if err := os.WriteFile(path, []byte("version: [unterminated\n"), 0o644); err != nil {
		t.Fatalf("corrupt active release manifest %s: %v", path, err)
	}
}

// A cron submit that fails for a reason submitRun does not classify used to be
// visible only in the daemon's log. An invalid active release is exactly that:
// it is a conflict, but it is not the "no active release" case submitRun
// records, so before the caller accounted for it the occurrence vanished and
// /health said nothing. It must be recorded once, under cron_submit_failed, and
// not again by any later manual failure of the same job.
func TestCronSubmitFailureIsRecordedOnce(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")
	corruptActiveRelease(t, d, id)

	rec, ok := scheduleForJob(d, "ticker")
	if !ok {
		t.Fatal("the schedule was not reconciled")
	}
	d.cronTick(rec.ID, time.Now().UTC())

	refusals, err := d.AdmissionRefusals(ctx)
	if err != nil {
		t.Fatalf("admission refusals: %v", err)
	}
	entry, found := refusals[id]
	if !found || len(refusals) != 1 {
		t.Fatalf("refusals = %+v, want exactly job %s", refusals, id)
	}
	if entry.Total != 1 {
		t.Errorf("refused_total = %d, want 1: the failure was counted more than once", entry.Total)
	}
	if entry.Reason != "cron_submit_failed" {
		t.Errorf("reason = %q, want cron_submit_failed", entry.Reason)
	}
	if entry.LastAt == nil {
		t.Error("the refusal was not timestamped")
	}

	// A failure is not an admitted run: the occurrence did not become work.
	list, err := d.ListRuns(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("runs = %d after a failed cron submit, want 0", len(list))
	}

	// The same fault on a MANUAL submission is still not counted: the caller
	// saw that error directly.
	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual}); err == nil {
		t.Fatal("a manual run against the invalid release should be refused")
	}
	after, err := d.AdmissionRefusals(ctx)
	if err != nil {
		t.Fatalf("admission refusals: %v", err)
	}
	if after[id].Total != 1 {
		t.Errorf("refused_total = %d after a manual failure, want it unchanged at 1", after[id].Total)
	}
}

// The two refusals submitRun records itself must still be counted exactly once
// when the caller is cronTick: the refusalRecorded marker exists so cronTick
// does not add a second cron_submit_failed for the same event.
func TestCronTickCountsRecordedRefusalsOnce(t *testing.T) {
	t.Run("no_active_release", func(t *testing.T) {
		root := t.TempDir()
		writeJob(t, root, "unreleased", `
version: 1
name: unreleased
entrypoint: main.py
timeout: 30
trigger:
  cron: "@every 6h"
retry:
  attempts: 0
`, noopPython)

		d := newDaemonUnreleased(t, root, "", nil, nil)
		ctx := context.Background()
		id := runtimeID(t, d, "unreleased")

		rec, ok := scheduleForJob(d, "unreleased")
		if !ok {
			t.Fatal("the schedule was not reconciled")
		}
		d.cronTick(rec.ID, time.Now().UTC())

		refusals, err := d.AdmissionRefusals(ctx)
		if err != nil {
			t.Fatalf("admission refusals: %v", err)
		}
		if len(refusals) != 1 || refusals[id].Total != 1 {
			t.Fatalf("refusals = %+v, want exactly one for job %s", refusals, id)
		}
		if refusals[id].Reason != "no_active_release" {
			t.Errorf("reason = %q, want no_active_release", refusals[id].Reason)
		}
	})

	t.Run("max_queue_depth", func(t *testing.T) {
		root := t.TempDir()
		writeJob(t, root, "ticker", `
version: 1
name: ticker
entrypoint: main.py
timeout: 300
concurrency: 1
max_queue_depth: 2
trigger:
  cron: "@every 6h"
retry:
  attempts: 0
`, noopPython)

		d := newDaemon(t, root, "", nil, nil)
		ctx := context.Background()
		id := runtimeID(t, d, "ticker")

		for i := 0; i < 2; i++ {
			if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual}); err != nil {
				t.Fatalf("seed run %d: %v", i+1, err)
			}
		}

		rec, ok := scheduleForJob(d, "ticker")
		if !ok {
			t.Fatal("the schedule was not reconciled")
		}
		d.cronTick(rec.ID, time.Now().UTC())

		refusals, err := d.AdmissionRefusals(ctx)
		if err != nil {
			t.Fatalf("admission refusals: %v", err)
		}
		if len(refusals) != 1 || refusals[id].Total != 1 {
			t.Fatalf("refusals = %+v, want exactly one for job %s", refusals, id)
		}
		if refusals[id].Reason != "max_queue_depth" {
			t.Errorf("reason = %q, want max_queue_depth", refusals[id].Reason)
		}
	})
}

// A duplicate wake-up is not a refusal: the occurrence already became a run, so
// counting it would inflate the signal with a non-event.
func TestDuplicateCronOccurrenceAddsNoRefusal(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")

	rec, ok := scheduleForJob(d, "ticker")
	if !ok {
		t.Fatal("the schedule was not reconciled")
	}
	occurrence := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	d.cronTick(rec.ID, occurrence)
	d.cronTick(rec.ID, occurrence)

	list, err := d.ListRuns(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("runs after a duplicate tick = %d, want 1", len(list))
	}
	refusals, err := d.AdmissionRefusals(ctx)
	if err != nil {
		t.Fatalf("admission refusals: %v", err)
	}
	if len(refusals) != 0 {
		t.Fatalf("a duplicate occurrence recorded %+v, want no refusals", refusals)
	}
}

// The wrapper around a recorded refusal must be transparent: the cause and the
// message are exactly what the caller saw before, and a manual failure is still
// not counted.
func TestRecordedRefusalKeepsCauseAndMessage(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "unreleased", `
version: 1
name: unreleased
entrypoint: main.py
timeout: 30
`, noopPython)

	d := newDaemonUnreleased(t, root, "", nil, nil)
	ctx := context.Background()

	occurrence := time.Now().UTC()
	_, autonomousErr := d.submitRun(ctx, "id:"+runtimeID(t, d, "unreleased"), api.TriggerPayload{
		Type:        api.TriggerCron,
		ScheduledAt: &occurrence,
	}, api.SubmitRunOptions{}, nil)
	if autonomousErr == nil {
		t.Fatal("a scheduled run against an unreleased job should be refused")
	}
	if !errors.Is(autonomousErr, api.ErrConflict) {
		t.Errorf("recorded refusal = %v, want errors.Is ErrConflict", autonomousErr)
	}
	if !refusalCounted(autonomousErr) {
		t.Error("submitRun recorded the refusal but did not mark the error")
	}

	_, manualErr := d.SubmitRun(ctx, "unreleased", api.TriggerPayload{Type: api.TriggerManual})
	if manualErr == nil {
		t.Fatal("a manual run against an unreleased job should be refused")
	}
	if autonomousErr.Error() != manualErr.Error() {
		t.Errorf("recorded refusal message = %q, manual message = %q; the wrapper changed what the caller sees",
			autonomousErr.Error(), manualErr.Error())
	}
	if refusalCounted(manualErr) {
		t.Error("a manual refusal was marked as recorded even though it is deliberately not counted")
	}

	refusals, err := d.AdmissionRefusals(ctx)
	if err != nil {
		t.Fatalf("admission refusals: %v", err)
	}
	if len(refusals) != 1 {
		t.Fatalf("refusals = %+v, want one job recorded", refusals)
	}
	for jobID, entry := range refusals {
		if entry.Total != 1 || entry.Reason != "no_active_release" {
			t.Errorf("job %s: total = %d reason = %q, want 1 and no_active_release",
				jobID, entry.Total, entry.Reason)
		}
	}
}

// coalesceTick has the same submit path as cronTick, so a failure its caller
// does not classify must land in the same accounting under its own reason.
func TestCoalesceSubmitFailureIsRecordedOnce(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "coalescer", `
version: 1
name: coalescer
entrypoint: main.py
timeout: 30
trigger:
  cron: "@every 1h"
  missed_policy: coalesce
retry:
  attempts: 0
`, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "coalescer")
	corruptActiveRelease(t, d, id)

	rec, ok := scheduleForJob(d, "coalescer")
	if !ok {
		t.Fatal("the coalesce schedule was not reconciled")
	}
	d.cronTick(rec.ID, time.Now().UTC())

	refusals, err := d.AdmissionRefusals(ctx)
	if err != nil {
		t.Fatalf("admission refusals: %v", err)
	}
	entry, found := refusals[id]
	if !found || len(refusals) != 1 {
		t.Fatalf("refusals = %+v, want exactly job %s", refusals, id)
	}
	if entry.Total != 1 {
		t.Errorf("refused_total = %d, want 1", entry.Total)
	}
	if entry.Reason != "coalesce_submit_failed" {
		t.Errorf("reason = %q, want coalesce_submit_failed", entry.Reason)
	}
}
