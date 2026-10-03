package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/schedule"
	"github.com/tkoizumi/otter/internal/scheduler"
)

// SetSchedule implements api.Backend's deprecated single-cadence endpoint. It
// replaces a job's cadence, or clears it when cron is empty.
//
// The stored value is written first and the in-memory scheduler is re-armed
// second, in the same order as a pause: a daemon that restarts between the two
// comes back with the schedule already in force, whereas the reverse order
// would lose the change to a crash.
//
// Clearing writes an empty cron rather than deleting the row. That distinction
// is the whole migration story -- no row means "the manifest's trigger.cron may
// seed it", an empty row means "the operator removed the cadence and it must
// stay removed". A manifest-owned schedule is refused with a conflict: the file
// is its owner, and the API may not fight a reload.
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
	for _, rec := range d.schedules.ForJob(jobID) {
		if rec.Origin == schedule.OriginManifest {
			return api.ScheduleView{}, fmt.Errorf(
				"job %q is scheduled by its manifest; change trigger.cron and reload, or use a schedule id: %w",
				label, api.ErrConflict)
		}
	}

	rec, changed, err := d.schedules.SetLegacyCadence(ctx, jobID, cron)
	if err != nil {
		return api.ScheduleView{}, err
	}
	d.armSchedule(rec)
	if changed {
		d.log.Info("schedule_set", "job", label, "id", rec.ID, "cron", rec.Cron)
	}

	view := d.scheduleView(rec)
	view.Changed = changed
	return view, nil
}

// ListSchedules implements api.Backend.
func (d *Daemon) ListSchedules(ctx context.Context, ref string) ([]api.ScheduleView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return nil, err
	}
	if entry.Instance.ID.IsZero() {
		return nil, fmt.Errorf("job %q has no registry identity: %w", entry.Job.Name, api.ErrNotFound)
	}
	recs := d.schedules.ForJob(entry.Instance.ID.String())
	out := make([]api.ScheduleView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, d.scheduleView(rec))
	}
	return out, nil
}

// CreateSchedule implements api.Backend.
func (d *Daemon) CreateSchedule(ctx context.Context, ref string, req api.ScheduleCreateRequest, idempotencyKey string) (api.ScheduleView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.ScheduleView{}, err
	}
	label := entry.Job.Name
	if label == "" {
		label = entry.Job.ID
	}
	inst := entry.Instance
	if inst.ID.IsZero() {
		return api.ScheduleView{}, fmt.Errorf("job %q has no registry identity: %w", label, api.ErrNotFound)
	}
	if !inst.Status.AcceptsWork() {
		return api.ScheduleView{}, fmt.Errorf("job %q is %s and cannot be scheduled: %w",
			label, inst.Status, api.ErrConflict)
	}

	rec, changed, err := d.schedules.Create(ctx, schedule.CreateInput{
		JobID:          inst.ID.String(),
		Cron:           req.Cron,
		Timezone:       req.Timezone,
		Payload:        req.Payload,
		MissedPolicy:   schedule.MissedPolicy(req.MissedPolicy),
		Origin:         schedule.OriginAPI,
		OriginRef:      "api",
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return api.ScheduleView{}, scheduleError(err)
	}
	if changed {
		d.log.Info("schedule_created", "job", label, "id", rec.ID, "cron", rec.Cron)
	}
	d.armSchedule(rec)

	view := d.scheduleView(rec)
	view.Changed = changed
	return view, nil
}

// GetSchedule implements api.Backend.
func (d *Daemon) GetSchedule(ctx context.Context, scheduleID string) (api.ScheduleView, error) {
	rec, ok := d.schedules.Get(scheduleID)
	if !ok {
		return api.ScheduleView{}, fmt.Errorf("schedule %q: %w", scheduleID, api.ErrNotFound)
	}
	return d.scheduleView(rec), nil
}

// UpdateSchedule implements api.Backend.
func (d *Daemon) UpdateSchedule(ctx context.Context, scheduleID string, req api.ScheduleUpdateRequest) (api.ScheduleView, error) {
	if req.Cron != nil && *req.Cron == "" {
		return api.ScheduleView{}, fmt.Errorf("cron may not be empty; delete the schedule instead: %w", api.ErrInvalid)
	}
	in := schedule.UpdateInput{Cron: req.Cron, Timezone: req.Timezone, Payload: req.Payload}
	if req.MissedPolicy != nil {
		policy := schedule.MissedPolicy(*req.MissedPolicy)
		in.MissedPolicy = &policy
	}

	rec, changed, err := d.schedules.Update(ctx, scheduleID, in)
	if err != nil {
		return api.ScheduleView{}, scheduleError(err)
	}
	if changed {
		d.log.Info("schedule_updated", "id", rec.ID, "job", rec.JobID, "cron", rec.Cron)
	}
	d.armSchedule(rec)

	view := d.scheduleView(rec)
	view.Changed = changed
	return view, nil
}

// DeleteSchedule implements api.Backend.
func (d *Daemon) DeleteSchedule(ctx context.Context, scheduleID string) error {
	if err := d.schedules.Delete(ctx, scheduleID); err != nil {
		return scheduleError(err)
	}
	d.sched.Unregister(scheduleID)
	d.log.Info("schedule_deleted", "id", scheduleID)
	return nil
}

// SetSchedulePaused implements api.Backend.
func (d *Daemon) SetSchedulePaused(ctx context.Context, scheduleID string, paused bool) (api.ScheduleView, error) {
	rec, changed, err := d.schedules.SetPaused(ctx, scheduleID, paused)
	if err != nil {
		return api.ScheduleView{}, scheduleError(err)
	}
	if changed {
		d.log.Info("schedule_pause_set", "id", rec.ID, "job", rec.JobID, "paused", paused)
	}
	d.armSchedule(rec)

	view := d.scheduleView(rec)
	view.Changed = changed
	return view, nil
}

// armSchedule makes the in-memory cron runner match one stored schedule: armed
// when it has a cron and neither it nor its job is paused, unarmed otherwise.
// Unregistering an absent key is a no-op, so this is safe to call after any
// mutation.
func (d *Daemon) armSchedule(rec schedule.Schedule) {
	if rec.Cron == "" || rec.Paused() || d.paused.Paused(rec.JobID) {
		d.sched.Unregister(rec.ID)
		return
	}
	loc, err := rec.Location()
	if err != nil {
		// Stored zones are validated on write; a value that no longer resolves
		// (a removed tzdata entry) must not stop the schedule from firing.
		d.log.Warn("schedule_timezone", "id", rec.ID, "timezone", rec.Timezone, "error", err.Error())
		loc = time.UTC
	}
	if err := d.sched.Replace(rec.ID, rec.Cron, loc, d.cronJob(rec.ID)); err != nil {
		d.log.Error("schedule_arm_failed", err, "id", rec.ID, "cron", rec.Cron)
	}
}

// scheduleView renders a stored schedule plus the next fire time the scheduler
// actually holds. A stored cron with no next time means the schedule or its job
// is paused, and the view says so rather than reporting a fire time that will
// not happen.
func (d *Daemon) scheduleView(rec schedule.Schedule) api.ScheduleView {
	view := api.ScheduleView{
		ID:           rec.ID,
		JobID:        rec.JobID,
		Name:         d.jobLabel(rec.JobID),
		Cron:         rec.Cron,
		Timezone:     rec.Timezone,
		Payload:      rec.Payload,
		MissedPolicy: string(rec.MissedPolicy),
		Origin:       string(rec.Origin),
		Paused:       rec.Paused(),
		PausedAt:     rec.PausedAt,
		LastFiredAt:  rec.LastFiredAt,
	}
	if next, ok := d.sched.Next(rec.ID); ok {
		at := next
		view.NextRunAt = &at
	} else if rec.Cron != "" && (rec.Paused() || d.paused.Paused(rec.JobID)) {
		view.Paused = true
	}
	return view
}

// jobLabel resolves a job id to its manifest label, falling back to the id.
// It is display only, so an unknown id is not an error.
func (d *Daemon) jobLabel(jobID string) string {
	if entry, ok := d.reg.get(jobID); ok && entry.Job.Name != "" {
		return entry.Job.Name
	}
	return jobID
}

// jobCron resolves the cadence the job view reports. A manifest-owned schedule
// wins, because that is the row the file declares; otherwise the earliest
// API-owned schedule is shown, and finally the live manifest trigger is a
// pre-reconciliation fallback for a job the store has not seen yet.
func (d *Daemon) jobCron(jobID string, m *config.Manifest) string {
	if d.schedules != nil {
		var first *schedule.Schedule
		for _, rec := range d.schedules.ForJob(jobID) {
			if rec.Origin == schedule.OriginManifest {
				return rec.Cron
			}
			if first == nil {
				chosen := rec
				first = &chosen
			}
		}
		if first != nil {
			return first.Cron
		}
	}
	if m == nil {
		return ""
	}
	return m.Cron()
}

// scheduleError maps the schedule package's sentinels onto the API's, so the
// HTTP layer can choose a status code without knowing the storage vocabulary.
func scheduleError(err error) error {
	switch {
	case errors.Is(err, schedule.ErrNotFound):
		return fmt.Errorf("%v: %w", err, api.ErrNotFound)
	case errors.Is(err, schedule.ErrManifestOwned):
		return fmt.Errorf("%v: %w", err, api.ErrConflict)
	case errors.Is(err, schedule.ErrConflict):
		return fmt.Errorf("%v: %w", err, api.ErrConflict)
	case errors.Is(err, schedule.ErrInvalid):
		return fmt.Errorf("%v: %w", err, api.ErrInvalid)
	default:
		return err
	}
}

// purgeSchedules forgets every schedule of a job when its identity is deleted.
// A failed purge is logged rather than returned: the identity is already gone,
// and leaving rows behind is a leak, not a state the caller can act on.
func (d *Daemon) purgeSchedules(ctx context.Context, jobID string) {
	if err := d.schedules.DeleteForJob(ctx, jobID); err != nil {
		d.log.Error("schedule_purge_failed", err, "job", jobID)
	}
}
