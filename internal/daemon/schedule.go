package daemon

import (
	"context"
	"fmt"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/scheduler"
)

// SetSchedule implements api.Backend. It replaces a job's cadence, or clears it
// when cron is empty.
//
// The stored value is written first and the in-memory scheduler is re-armed
// second, in the same order as a pause: a daemon that restarts between the two
// comes back with the schedule already in force, whereas the reverse order
// would lose the change to a crash.
//
// Clearing writes an empty cron rather than deleting the row. That distinction
// is the whole migration story — no row means "import the manifest's
// trigger.cron", an empty row means "the operator removed the schedule and it
// must stay removed".
func (d *Daemon) SetSchedule(ctx context.Context, ref, cron string) (api.ScheduleView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.ScheduleView{}, err
	}

	inst := entry.Instance
	label := entry.Job.Name
	if label == "" {
		label = entry.Job.ID
	}
	if inst.ID.IsZero() {
		return api.ScheduleView{}, fmt.Errorf("job %q has no registry identity: %w", label, api.ErrNotFound)
	}
	if !inst.Status.AcceptsWork() {
		return api.ScheduleView{}, fmt.Errorf("job %q is %s and cannot be scheduled: %w",
			label, inst.Status, api.ErrConflict)
	}
	if cron != "" {
		// Validate before writing, so a bad expression is rejected while the
		// previous schedule is still in force rather than after it is gone.
		if _, err := scheduler.Parse(cron); err != nil {
			return api.ScheduleView{}, fmt.Errorf("invalid cron expression %q: %w", cron, api.ErrInvalid)
		}
	}

	jobID := inst.ID.String()

	rec, changed, err := d.schedules.Set(ctx, jobID, cron)
	if err != nil {
		return api.ScheduleView{}, err
	}

	if rec.Cron == "" {
		d.sched.Unregister(jobID)
	} else {
		d.sched.Replace(jobID, rec.Cron, d.cronJob(jobID, rec.Cron))
	}
	if changed {
		d.log.Info("schedule_set", "job", label, "id", jobID, "cron", rec.Cron)
	}

	return d.scheduleView(jobID, label, rec.Cron, changed), nil
}

// scheduleView renders the stored cadence plus the next fire time the scheduler
// actually holds. A stored cron with no next time means the job is paused, and
// the view says so rather than reporting a fire time that will not happen.
func (d *Daemon) scheduleView(jobID, label, cron string, changed bool) api.ScheduleView {
	view := api.ScheduleView{
		JobID:   jobID,
		Name:    label,
		Cron:    cron,
		Changed: changed,
	}
	if next, ok := d.sched.Next(jobID); ok {
		at := next
		view.NextRunAt = &at
	} else if cron != "" && d.paused.Paused(jobID) {
		view.Paused = true
	}
	return view
}

// purgeSchedule forgets a job's schedule when its identity is deleted. A failed
// purge is logged rather than returned: the identity is already gone, and
// leaving a row behind is a leak, not a state the caller can act on.
func (d *Daemon) purgeSchedule(ctx context.Context, jobID string) {
	if err := d.schedules.Delete(ctx, jobID); err != nil {
		d.log.Error("schedule_purge_failed", err, "job", jobID)
	}
}
