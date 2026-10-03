package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/runs"
)

// Firing a schedule creates exactly one run per occurrence, writes the ledger
// entry, and carries the schedule's payload into the run's trigger metadata.
// A duplicate wake-up for the same occurrence produces no second run.
func TestCronFireRecordsOccurrenceAndPayload(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "plain")

	created, err := d.CreateSchedule(ctx, "id:"+id, api.ScheduleCreateRequest{
		Cron:    "@every 1h",
		Payload: json.RawMessage(`{"dataset":42}`),
	}, "key-1")
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if !created.Changed {
		t.Fatal("a fresh schedule reported changed = false")
	}

	occurrence := time.Now().UTC().Truncate(time.Second)
	d.cronTick(created.ID, occurrence)

	list, err := d.ListRuns(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("runs after one occurrence = %d, want 1", len(list))
	}
	run := list[0]
	if run.ScheduleID != created.ID {
		t.Errorf("run schedule_id = %q, want %q", run.ScheduleID, created.ID)
	}
	if run.TriggerType != runs.TriggerCron {
		t.Errorf("run trigger = %q, want cron", run.TriggerType)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(run.Metadata, &meta); err != nil {
		t.Fatalf("run metadata = %s: %v", run.Metadata, err)
	}
	if got := string(meta["body"]); got != `{"dataset":42}` {
		t.Errorf("trigger body = %s, want the schedule payload", got)
	}
	if _, ok := meta["scheduled_at"]; !ok {
		t.Errorf("trigger metadata has no scheduled_at: %s", run.Metadata)
	}

	// The same occurrence again is a duplicate: no second run.
	d.cronTick(created.ID, occurrence)
	list, err = d.ListRuns(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("a duplicate occurrence created %d run(s), want 1", len(list))
	}

	// A later occurrence is a new run.
	d.cronTick(created.ID, occurrence.Add(time.Hour))
	list, err = d.ListRuns(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("a second occurrence produced %d run(s), want 2", len(list))
	}

	// The fire surviving the daemon's memory is the whole point: a second
	// store over the same database still finds the ledger entry.
	if found, err := d.schedules.FireRecorded(ctx, created.ID, occurrence); err != nil || !found {
		t.Fatalf("FireRecorded = %v, %v; want true", found, err)
	}
}

// A schedule the operator paused individually does not fire, even while its job
// is running normally.
func TestCronTickSkipsAPausedSchedule(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "plain")

	created, err := d.CreateSchedule(ctx, "id:"+id, api.ScheduleCreateRequest{Cron: "@every 1h"}, "key-1")
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if _, err := d.SetSchedulePaused(ctx, created.ID, true); err != nil {
		t.Fatalf("pause schedule: %v", err)
	}

	d.cronTick(created.ID, time.Now().UTC())
	list, err := d.ListRuns(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a paused schedule created %d run(s), want 0", len(list))
	}

	// Resuming re-arms it and the next occurrence fires.
	if _, err := d.SetSchedulePaused(ctx, created.ID, false); err != nil {
		t.Fatalf("resume schedule: %v", err)
	}
	d.cronTick(created.ID, time.Now().UTC().Add(time.Minute))
	list, err = d.ListRuns(ctx, runs.Filter{JobID: id})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("after resume, runs = %d, want 1", len(list))
	}
}
