package daemon

import (
	"context"
	"testing"
)

// A job's cadence is runtime state, and the whole point of the schedule table
// is that it stays that way. The manifest is imported once so an existing
// deployment keeps firing, an API change survives a reload, and clearing the
// schedule is sticky rather than a return to the manifest default.
func TestScheduleIsRuntimeState(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")

	// First sight imports the manifest's trigger.cron.
	rec, ok := d.schedules.Get(id)
	if !ok {
		t.Fatalf("the manifest cron was not imported")
	}
	if rec.Cron != "@every 6h" {
		t.Fatalf("imported cron = %q, want %q", rec.Cron, "@every 6h")
	}

	// Changing the cadence through the API wins.
	if _, err := d.SetSchedule(ctx, "id:"+id, "@every 1h"); err != nil {
		t.Fatalf("set schedule: %v", err)
	}

	// A reload must not put the manifest's value back. That drift is exactly
	// what the table exists to prevent.
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rec, _ := d.schedules.Get(id); rec.Cron != "@every 1h" {
		t.Fatalf("reload reverted the cadence to %q", rec.Cron)
	}
	if spec, armed := d.sched.Spec(id); !armed || spec != "@every 1h" {
		t.Fatalf("armed trigger after reload = %q (armed=%v), want %q", spec, armed, "@every 1h")
	}

	// Clearing is a first-class state. The row stays with an empty cron so a
	// reload cannot re-import the manifest.
	if _, err := d.SetSchedule(ctx, "id:"+id, ""); err != nil {
		t.Fatalf("clear schedule: %v", err)
	}
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rec, ok = d.schedules.Get(id)
	if !ok {
		t.Fatalf("clearing removed the row, so a reload could re-import the manifest")
	}
	if rec.Cron != "" {
		t.Fatalf("a cleared cadence came back as %q", rec.Cron)
	}
	if spec, armed := d.sched.Spec(id); armed {
		t.Fatalf("a cleared schedule is still armed as %q", spec)
	}
}

// A rejected expression must not destroy the cadence it was replacing.
func TestSetScheduleRejectsAnInvalidExpression(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")

	if _, err := d.SetSchedule(ctx, "id:"+id, "not a cron expression"); err == nil {
		t.Fatalf("an invalid expression was accepted")
	}
	if rec, _ := d.schedules.Get(id); rec.Cron != "@every 6h" {
		t.Fatalf("a rejected change altered the stored cadence: %q", rec.Cron)
	}
	if spec, armed := d.sched.Spec(id); !armed || spec != "@every 6h" {
		t.Fatalf("a rejected change altered the armed trigger: %q (armed=%v)", spec, armed)
	}
}
