package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/schedule"
)

// noCronJob declares no trigger, so the only cadence it can have is one written
// through the API.
const noCronJob = `
version: 1
name: plain
entrypoint: main.py
timeout: 30
retry:
  attempts: 0
`

// jobScheduleID returns the single schedule id of a test job. Every fixture used
// by these tests holds exactly one schedule when it holds any.
func jobScheduleID(t *testing.T, d *Daemon, jobID string) string {
	t.Helper()
	recs := d.schedules.ForJob(jobID)
	if len(recs) != 1 {
		t.Fatalf("job %s holds %d schedules, want exactly 1", jobID, len(recs))
	}
	return recs[0].ID
}

// jobCronSpec reports the armed cron expression of a job's schedule, if any.
func jobCronSpec(d *Daemon, jobID string) (string, bool) {
	for _, rec := range d.schedules.ForJob(jobID) {
		if spec, ok := d.sched.Spec(rec.ID); ok {
			return spec, true
		}
	}
	return "", false
}

// jobNextRun reports the soonest armed fire time across a job's schedules.
func jobNextRun(d *Daemon, jobID string) (time.Time, bool) {
	var (
		soonest time.Time
		found   bool
	)
	for _, rec := range d.schedules.ForJob(jobID) {
		if next, ok := d.sched.Next(rec.ID); ok {
			if !found || next.Before(soonest) {
				soonest, found = next, true
			}
		}
	}
	return soonest, found
}

// A manifest-declared trigger SEEDS a row. The row is editable and its seed is
// remembered, so a reload with an unchanged manifest leaves an edit alone; a
// changed manifest applies. The old "the file owns it" refusal is gone.
func TestManifestScheduleSeedsAnEditableRow(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "ticker")

	recs := d.schedules.ForJob(id)
	if len(recs) != 1 {
		t.Fatalf("schedule rows = %d, want 1", len(recs))
	}
	if recs[0].Cron != "@every 6h" || recs[0].Origin != schedule.OriginManifest {
		t.Fatalf("reconciled row = %+v, want a manifest row with @every 6h", recs[0])
	}
	if spec, armed := jobCronSpec(d, id); !armed || spec != "@every 6h" {
		t.Fatalf("armed trigger = %q (armed=%v), want @every 6h", spec, armed)
	}

	// The dashboard edits the seeded row, and a reload with the SAME manifest
	// cron keeps that edit.
	if _, err := d.SetSchedule(ctx, "id:"+id, "@every 1h"); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if got := d.schedules.ForJob(id)[0]; got.Cron != "@every 1h" {
		t.Fatalf("dashboard edit = %q, want @every 1h", got.Cron)
	}
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := d.schedules.ForJob(id)[0]; got.Cron != "@every 1h" {
		t.Fatalf("reload with an unchanged manifest overwrote the edit: %q", got.Cron)
	}

	// The schedule-id API edits it too.
	cron := "@every 2h"
	if _, err := d.UpdateSchedule(ctx, recs[0].ID, api.ScheduleUpdateRequest{Cron: &cron}); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}

	// Removing it clears the seeded row rather than deleting it, so a reload
	// does not resurrect the cadence.
	if err := d.DeleteSchedule(ctx, recs[0].ID); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if got := d.schedules.ForJob(id); len(got) != 1 || got[0].Cron != "" {
		t.Fatalf("after delete = %+v, want one cleared row", got)
	}

	// An API schedule may be added alongside, and a reload leaves it exactly as
	// it is while the manifest row still reconcile from the file.
	created, err := d.CreateSchedule(ctx, "id:"+id, api.ScheduleCreateRequest{Cron: "@every 1h"}, "key-1")
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if created.ID == "" || created.Origin != string(schedule.OriginAPI) {
		t.Fatalf("created schedule = %+v, want an api-owned row with an id", created)
	}

	// A retried create with the same key produces no second schedule.
	again, err := d.CreateSchedule(ctx, "id:"+id, api.ScheduleCreateRequest{Cron: "@every 1h"}, "key-1")
	if err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if again.ID != created.ID || again.Changed {
		t.Fatalf("duplicate create = %+v (changed=%v), want the existing row unchanged", again, again.Changed)
	}

	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after := d.schedules.ForJob(id)
	if len(after) != 2 {
		t.Fatalf("schedule rows after reload = %d, want 2 (manifest + api)", len(after))
	}
	var foundAPI bool
	for _, rec := range after {
		if rec.ID == created.ID && rec.Cron == "@every 1h" && rec.Origin == schedule.OriginAPI {
			foundAPI = true
		}
	}
	if !foundAPI {
		t.Fatalf("reload changed or removed the API-created schedule: %+v", after)
	}
}

// A job with no manifest trigger keeps the v0.3.0 single-cadence contract: the
// deprecated endpoint owns it, clearing is sticky, and a reload never invents a
// manifest row.
func TestLegacyCadenceIsRuntimeState(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "plain")

	if _, err := d.SetSchedule(ctx, "id:"+id, "@every 1h"); err != nil {
		t.Fatalf("set schedule: %v", err)
	}
	if spec, armed := jobCronSpec(d, id); !armed || spec != "@every 1h" {
		t.Fatalf("armed trigger = %q (armed=%v), want @every 1h", spec, armed)
	}

	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	recs := d.schedules.ForJob(id)
	if len(recs) != 1 || recs[0].Cron != "@every 1h" {
		t.Fatalf("rows after reload = %+v, want one legacy row @every 1h", recs)
	}

	// Clearing keeps the row with an empty cron so a reload cannot re-import a
	// manifest default.
	if _, err := d.SetSchedule(ctx, "id:"+id, ""); err != nil {
		t.Fatalf("clear schedule: %v", err)
	}
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	recs = d.schedules.ForJob(id)
	if len(recs) != 1 || recs[0].Cron != "" {
		t.Fatalf("rows after clear+reload = %+v, want one cleared row", recs)
	}
	if spec, armed := jobCronSpec(d, id); armed {
		t.Fatalf("a cleared cadence is still armed as %q", spec)
	}
}

// A rejected expression must not destroy the cadence it was replacing.
func TestSetScheduleRejectsAnInvalidExpression(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "plain")

	if _, err := d.SetSchedule(ctx, "id:"+id, "@every 6h"); err != nil {
		t.Fatalf("set schedule: %v", err)
	}
	if _, err := d.SetSchedule(ctx, "id:"+id, "not a cron expression"); err == nil {
		t.Fatalf("an invalid expression was accepted")
	}
	recs := d.schedules.ForJob(id)
	if len(recs) != 1 || recs[0].Cron != "@every 6h" {
		t.Fatalf("a rejected change altered the stored cadence: %+v", recs)
	}
	if spec, armed := jobCronSpec(d, id); !armed || spec != "@every 6h" {
		t.Fatalf("a rejected change altered the armed trigger: %q (armed=%v)", spec, armed)
	}
}

// The job view says what owns the cadence it reports. The dashboard may edit an
// api-owned schedule; a manifest one is the file's, and offering Save on it is
// what produced a 409 the user could not act on.
func TestJobViewReportsTheCronOrigin(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)
	d := newDaemon(t, root, "", nil, nil)
	id := runtimeID(t, d, "ticker")
	view, ok := d.GetJob("id:" + id)
	if !ok {
		t.Fatal("manifest job not found")
	}
	if view.Triggers.CronOrigin != "manifest" {
		t.Fatalf("manifest cron origin = %q, want manifest", view.Triggers.CronOrigin)
	}

	root2 := t.TempDir()
	writeJob(t, root2, "plain", noCronJob, noopPython)
	d2 := newDaemon(t, root2, "", nil, nil)
	id2 := runtimeID(t, d2, "plain")
	if _, err := d2.CreateSchedule(context.Background(), "id:"+id2, api.ScheduleCreateRequest{Cron: "@every 1h"}, "key"); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	view2, ok := d2.GetJob("id:" + id2)
	if !ok {
		t.Fatal("api job not found")
	}
	if view2.Triggers.CronOrigin != "api" {
		t.Fatalf("api cron origin = %q, want api", view2.Triggers.CronOrigin)
	}
}
