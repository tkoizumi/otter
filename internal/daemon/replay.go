package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/schedule"
	"github.com/tkoizumi/otter/internal/scheduler"
)

// replayMissedOccurrences accounts for the occurrences a schedule missed while
// the daemon was not running, according to the schedule's missed_policy.
//
// The ledger decides what "missed" means: last_fired_at is written in the same
// transaction as the run and its ledger row, so the window is exactly
// (last_fired_at, now] -- strictly after the last occurrence that fired, so a
// replayed occurrence is always one that never ran. An occurrence the ledger already holds cannot be
// replayed twice however this is scheduled, because submitRun writes the
// ledger row first and loses the race to the primary key.
//
// It runs once per schedule at startup, before the cron runner is armed and
// before any worker can claim work, so a replayed occurrence cannot race the
// normal fire path.
func (d *Daemon) replayMissedOccurrences(ctx context.Context, now time.Time) {
	for _, rec := range d.schedules.All() {
		if rec.Cron == "" || rec.Paused() || d.paused.Paused(rec.JobID) {
			continue
		}
		switch rec.MissedPolicy {
		case schedule.MissedCoalesce, schedule.MissedCatchUp:
		default:
			// skip, and the absent policy, mean the window is dropped. That is
			// the default and it stays the default.
			continue
		}
		if err := d.replaySchedule(ctx, rec, now); err != nil {
			d.log.Error("schedule_replay_failed", err, "schedule", rec.ID, "job", rec.JobID, "policy", string(rec.MissedPolicy))
		}
	}
}

// replaySchedule applies one schedule's policy to its missed window.
func (d *Daemon) replaySchedule(ctx context.Context, rec schedule.Schedule, now time.Time) error {
	if rec.LastFiredAt == nil {
		// A schedule that has never fired has no window to replay. Reaching
		// back to the epoch would turn a fresh schedule into an instant herd,
		// so the first occurrence is always the next one in the future.
		return nil
	}

	parsed, err := d.scheduleExpression(rec)
	if err != nil {
		return err
	}

	missed, skipped, err := missedOccurrences(parsed, *rec.LastFiredAt, now, rec.CatchUpLimit())
	if err != nil {
		return err
	}
	if len(missed) == 0 {
		return nil
	}

	switch rec.MissedPolicy {
	case schedule.MissedCatchUp:
		// Oldest first, so the replayed work arrives in the order it was due.
		for _, at := range missed {
			if err := d.fireOccurrence(ctx, rec, at, nil); err != nil {
				return err
			}
		}
		d.log.Info("catch_up_replayed",
			"schedule", rec.ID, "job", rec.JobID,
			"replayed", len(missed), "skipped", skipped,
			"from", missed[0].Format(time.RFC3339), "to", missed[len(missed)-1].Format(time.RFC3339),
			"limit", rec.CatchUpLimit())
		if skipped > 0 {
			// Reported once with the window, not per occurrence: the operator
			// needs to know work was dropped, not to read it a hundred times.
			d.log.Warn("catch_up_truncated",
				"schedule", rec.ID, "job", rec.JobID,
				"skipped", skipped, "limit", rec.CatchUpLimit(),
				"from", missed[0].Format(time.RFC3339), "to", missed[len(missed)-1].Format(time.RFC3339))
		}
		return nil

	case schedule.MissedCoalesce:
		// One run stands for the whole window. It is the most recent occurrence
		// that carries it, so the run's scheduled time is the latest one due,
		// and the window it absorbed is recorded on the run rather than implied.
		last := missed[len(missed)-1]
		meta := map[string]any{
			"coalesced": map[string]any{
				"missed":  len(missed) + skipped,
				"window":  map[string]any{"first": missed[0].UTC().Format(time.RFC3339Nano), "last": last.UTC().Format(time.RFC3339Nano)},
				"skipped": skipped,
			},
		}
		if err := d.fireOccurrence(ctx, rec, last, meta); err != nil {
			return err
		}
		d.log.Info("coalesce_fired",
			"schedule", rec.ID, "job", rec.JobID,
			"absorbed", len(missed)+skipped, "skipped", skipped,
			"from", missed[0].Format(time.RFC3339), "to", last.Format(time.RFC3339))
		return nil
	}
	return nil
}

// scheduleExpression parses a schedule's cron expression, which is required to
// enumerate the occurrences it missed.
func (d *Daemon) scheduleExpression(rec schedule.Schedule) (cron.Schedule, error) {
	parsed, err := scheduler.Parse(rec.Cron)
	if err != nil {
		return nil, fmt.Errorf("parse cron %q: %w", rec.Cron, err)
	}
	return parsed, nil
}

// missedOccurrences enumerates the occurrences in (last, now], oldest first,
// and reports how many beyond max were dropped.
//
// The walk uses the cron expression's own Next repeatedly rather than assuming
// a fixed interval: a schedule that skips weekends, or fires on the last day of
// a month, has no interval to divide by. It is bounded by max plus one, so a
// schedule that missed a year costs the same as one that missed the cap.
func missedOccurrences(parsed cron.Schedule, last, now time.Time, max int) ([]time.Time, int, error) {
	if max <= 0 {
		max = schedule.MaxCatchUp
	}
	var (
		out     []time.Time
		dropped int
	)
	// Next is exclusive, so step back a second to include an occurrence exactly
	// at last. The ledger would refuse a duplicate anyway; including it keeps
	// The walk is strictly after last: last_fired_at names an occurrence that
	// already produced a run, so including it would re-fire it. Next is
	// exclusive already, which makes the cursor exactly right.
	cursor := last
	for {
		next := parsed.Next(cursor)
		if next.IsZero() || next.After(now) {
			break
		}
		cursor = next
		if len(out) < max {
			out = append(out, next.UTC())
			continue
		}
		dropped++
	}
	return out, dropped, nil
}

// fireOccurrence submits one run for an occurrence, carrying any extra trigger
// metadata. It reuses the cron path's payload construction so a replayed run is
// indistinguishable from a live one except for what the metadata records.
func (d *Daemon) fireOccurrence(ctx context.Context, rec schedule.Schedule, occurrence time.Time, extra map[string]any) error {
	at := occurrence.UTC()
	payload := api.TriggerPayload{Type: api.TriggerCron, ScheduledAt: &at}
	if body := strings.TrimSpace(string(rec.Payload)); body != "" && body != "{}" {
		payload.Body = rec.Payload
	}
	_, err := d.submitRun(ctx, "id:"+rec.JobID, payload, api.SubmitRunOptions{Metadata: extra},
		&fireRequest{ScheduleID: rec.ID, Occurrence: occurrence})
	if err != nil && err != errAlreadyFired {
		return err
	}
	return nil
}
