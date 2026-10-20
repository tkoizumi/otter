package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
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
