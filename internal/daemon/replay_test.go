package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/schedule"
	"github.com/tkoizumi/otter/internal/scheduler"
)

// MissedCoalesceForTest aliases the store's policy name so the assertion reads
// as the value under test rather than as a string literal.
const MissedCoalesceForTest = schedule.MissedCoalesce

// scheduleForJob returns the job's single reconciled schedule.
func scheduleForJob(d *Daemon, name string) (schedule.Schedule, bool) {
	got, ok := d.GetJob(name)
	if !ok || got.ID == "" {
		return schedule.Schedule{}, false
	}
	for _, rec := range d.schedules.All() {
		if rec.JobID == got.ID {
			return rec, true
		}
	}
	return schedule.Schedule{}, false
}

// mustParse parses a cron expression for the replay tests.
func mustParse(t *testing.T, spec string) interface {
	Next(time.Time) time.Time
} {
	t.Helper()
	parsed, err := scheduler.Parse(spec)
	if err != nil {
		t.Fatalf("parse %q: %v", spec, err)
	}
	return parsed
}

// The missed window is exactly (last, now] — strictly after the occurrence that fired — oldest first, and the walk follows
// the expression rather than an assumed interval -- a schedule that skips a day
// has no interval to divide by.
func TestMissedOccurrencesEnumeratesTheWindow(t *testing.T) {
	hourly := mustParse(t, "0 * * * *")
	last := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC)

	got, dropped, err := missedOccurrences(hourly, last, now, schedule.MaxCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	want := []time.Time{
		time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
	}
	if len(got) != len(want) {
		t.Fatalf("occurrences = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("occurrence %d = %s, want %s", i, got[i], want[i])
		}
	}

	// The occurrence named by last_fired_at already produced a run, so it is
	// not part of the window: replaying it would re-fire it.
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	got, _, err = missedOccurrences(hourly, at, now, schedule.MaxCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Equal(at) {
		t.Errorf("window starting on an occurrence = %v, want it excluded", got)
	}

	// Nothing missed: the schedule is current.
	if got, _, err = missedOccurrences(hourly, now, now, schedule.MaxCatchUp); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Errorf("a current schedule reported misses: %v", got)
	}
}

// The cap is a real bound, not a truncation that loses the count: occurrences
// beyond it are reported as dropped so an operator learns work was skipped.
func TestMissedOccurrencesHonoursTheCap(t *testing.T) {
	hourly := mustParse(t, "0 * * * *")
	last := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) // 108 hours

	got, dropped, err := missedOccurrences(hourly, last, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Errorf("replayed = %d, want the cap of 10", len(got))
	}
	if dropped != 98 {
		t.Errorf("dropped = %d, want 98", dropped)
	}
	// Oldest first: the replayed occurrences are the earliest ones, so the
	// backlog is replayed in the order it was due rather than backwards.
	if !got[0].Equal(last.Add(time.Hour)) {
		t.Errorf("first replayed = %s, want %s", got[0], last.Add(time.Hour))
	}

	// A zero or negative bound means the daemon default rather than "replay
	// nothing", which is what the stored zero means.
	got, _, err = missedOccurrences(hourly, last, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != schedule.MaxCatchUp {
		t.Errorf("default bound replayed %d, want %d", len(got), schedule.MaxCatchUp)
	}
}

// A schedule whose expression does not fire every hour is walked correctly,
// which is the case an interval-based implementation gets wrong.
func TestMissedOccurrencesFollowsANonUniformExpression(t *testing.T) {
	// 09:00 on weekdays only.
	weekdays := mustParse(t, "0 9 * * 1-5")
	// Friday 2026-10-02 09:00, missed through Monday 2026-10-05 23:00.
	last := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC)

	got, dropped, err := missedOccurrences(weekdays, last, now, schedule.MaxCatchUp)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	want := []time.Time{
		time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), // Monday
	}
	if len(got) != len(want) {
		t.Fatalf("occurrences = %v, want %v (the weekend is not a backlog)", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("occurrence %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// max_queue_depth bounds autonomous admission and never a manual run. Without
// workers running, nothing drains the queue, so the bound is reached and stays
// reached -- which is exactly the state an operator's hand-run must survive.
func TestMaxQueueDepthBoundsAutonomousAdmissionButNotManualRuns(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", `
version: 1
name: ticker
entrypoint: main.py
timeout: 30
max_queue_depth: 2
trigger:
  cron: "@every 6h"
retry:
  attempts: 0
`, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	// Fill the queue with autonomous work.
	for i := 0; i < 2; i++ {
		if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerCron}); err != nil {
			t.Fatalf("autonomous run %d: %v", i+1, err)
		}
	}

	// The third autonomous trigger is refused, and the refusal is a distinct
	// error so the API can answer 429 rather than 503 or 409.
	for _, trigger := range []string{api.TriggerCron, api.TriggerWebhook} {
		if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: trigger}); !errors.Is(err, api.ErrOverloaded) {
			t.Errorf("%s trigger at the depth bound = %v, want ErrOverloaded", trigger, err)
		}
	}

	// A manual run is always accepted, bound or no bound.
	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual}); err != nil {
		t.Fatalf("a manual run must be admitted past the bound: %v", err)
	}
	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{}); err != nil {
		t.Errorf("a run with no declared trigger type is manual and must be admitted: %v", err)
	}

	// The refusal is counted and timestamped so it is visible after the fact.
	refusals, err := d.AdmissionRefusals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 1 {
		t.Fatalf("depth refusals = %v, want one job", refusals)
	}
	for _, rec := range refusals {
		if rec.Total != 2 {
			t.Errorf("refused_total = %d, want 2", rec.Total)
		}
		if rec.LastAt == nil || rec.LastAt.IsZero() {
			t.Error("the refusal was not timestamped")
		}
	}
}

// A job with no max_queue_depth is unbounded, which is today's meaning and what
// the compatibility policy requires of an additive minor.
func TestAbsentMaxQueueDepthStaysUnbounded(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", cronTicker, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerCron}); err != nil {
			t.Fatalf("autonomous run %d with no bound: %v", i+1, err)
		}
	}
}

// coalesce bounds uptime as well as downtime: while one run is waiting, a later
// occurrence folds into it instead of adding a second queued run. Without
// workers running, nothing drains the queue, so what the ticks produce is
// exactly what the policy decided.
func TestCoalesceFoldsUptimeOccurrencesIntoOnePendingRun(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", `
version: 1
name: ticker
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

	rec, ok := scheduleForJob(d, "ticker")
	if !ok {
		t.Fatal("the coalesce schedule was not reconciled")
	}
	if rec.MissedPolicy != MissedCoalesceForTest {
		t.Fatalf("stored policy = %q, want coalesce", rec.MissedPolicy)
	}

	base := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		d.cronTick(rec.ID, base.Add(time.Duration(i)*time.Hour))
	}

	waiting, err := d.runs.ListByStatus(ctx, runs.StatusQueued, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 {
		t.Fatalf("quiet queue holds %d runs, want exactly 1: coalesce must leave one pending", len(waiting))
	}

	// The surviving run says how many occurrences it stands for, so a reader
	// can tell a coalesced run from an ordinary fire.
	var meta struct {
		Coalesced struct {
			Count int    `json:"count"`
			First string `json:"first"`
			Last  string `json:"last"`
		} `json:"coalesced"`
	}
	if err := json.Unmarshal(waiting[0].Metadata, &meta); err != nil {
		t.Fatalf("decode run metadata %s: %v", waiting[0].Metadata, err)
	}
	if meta.Coalesced.Count != 3 {
		t.Errorf("coalesced count = %d, want 3 (the three occurrences after the first)", meta.Coalesced.Count)
	}
	if meta.Coalesced.First == "" || meta.Coalesced.Last == "" {
		t.Errorf("the coalesced window is not recorded: %+v", meta.Coalesced)
	}

	// A duplicate tick for an occurrence already folded is a no-op, not a
	// second fold: the ledger is the source of truth for "this is done".
	d.cronTick(rec.ID, base.Add(time.Hour))
	again, err := d.runs.ListByStatus(ctx, runs.StatusQueued, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("a duplicate tick produced %d pending runs, want 1", len(again))
	}
	var meta2 struct {
		Coalesced struct {
			Count int `json:"count"`
		} `json:"coalesced"`
	}
	if err := json.Unmarshal(again[0].Metadata, &meta2); err != nil {
		t.Fatal(err)
	}
	if meta2.Coalesced.Count != meta.Coalesced.Count {
		t.Errorf("a duplicate tick changed the count from %d to %d", meta.Coalesced.Count, meta2.Coalesced.Count)
	}
}

// The bound counts accepted-and-unfinished work, not just the wait queue. A
// job whose run is executing is as saturated as one whose run is waiting, so a
// schedule must not be able to pile more onto it.
func TestMaxQueueDepthCountsRunningRunsToo(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", `
version: 1
name: ticker
entrypoint: main.py
timeout: 30
max_queue_depth: 1
trigger:
  cron: "@every 6h"
retry:
  attempts: 0
`, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	runID, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Move it to running: the queue is empty, but the job has unfinished work.
	if err := d.runs.SetStatus(ctx, runID, runs.StatusRunning, ""); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerCron}); !errors.Is(err, api.ErrOverloaded) {
		t.Errorf("autonomous trigger with one run executing = %v, want ErrOverloaded", err)
	}
	// The manual bypass is unaffected by which state the pending work is in.
	if _, err := d.SubmitRun(ctx, "ticker", api.TriggerPayload{Type: api.TriggerManual}); err != nil {
		t.Errorf("a manual run must be admitted with a run executing: %v", err)
	}
}

// The startup replay is what makes the policies real: skip drops the window,
// coalesce turns it into one run, catch_up replays it oldest-first up to the
// bound. Each case drives the same entry point the daemon calls before arming
// the cron runner.
func TestStartupReplayAppliesEachPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy    string
		wantRuns  int
		wantCount int // occurrences the surviving run says it absorbed (coalesce only)
	}{
		{"skip", 0, 0},
		{"coalesce", 1, 3},
		{"catch_up", 3, 0},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			root := t.TempDir()
			writeJob(t, root, "ticker", `
version: 1
name: ticker
entrypoint: main.py
timeout: 30
trigger:
  cron: "0 * * * *"
  missed_policy: `+tc.policy+`
retry:
  attempts: 0
`, noopPython)

			d := newDaemon(t, root, "", nil, nil)
			ctx := context.Background()

			rec, ok := scheduleForJob(d, "ticker")
			if !ok {
				t.Fatal("the schedule was not reconciled")
			}
			// A window of three missed occurrences: 10:00, 11:00 and 12:00.
			last := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
			// Seed the window: the ledger says the 09:00 occurrence ran, so
			// everything after it is what the policy has to account for.
			d.schedules.NoteFired(rec.ID, last)
			d.replayMissedOccurrences(ctx, time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC))

			waiting, err := d.runs.ListByStatus(ctx, runs.StatusQueued, 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(waiting) != tc.wantRuns {
				t.Fatalf("%s replayed %d runs, want %d", tc.policy, len(waiting), tc.wantRuns)
			}
			if tc.policy != "coalesce" {
				if tc.policy == "catch_up" {
					// Oldest first, so the replayed work arrives in order.
					first, err := time.Parse(time.RFC3339Nano, mustScheduledAt(t, waiting[0].Metadata))
					if err != nil {
						t.Fatal(err)
					}
					if !first.Equal(last.Add(time.Hour)) {
						t.Errorf("first replayed occurrence = %s, want %s", first, last.Add(time.Hour))
					}
				}
				return
			}
			var meta struct {
				Coalesced struct {
					Missed int `json:"missed"`
				} `json:"coalesced"`
			}
			if err := json.Unmarshal(waiting[0].Metadata, &meta); err != nil {
				t.Fatalf("decode metadata %s: %v", waiting[0].Metadata, err)
			}
			if meta.Coalesced.Missed != tc.wantCount {
				t.Errorf("coalesced missed = %d, want %d", meta.Coalesced.Missed, tc.wantCount)
			}
		})
	}
}

// mustScheduledAt pulls the occurrence out of a run's trigger metadata.
func mustScheduledAt(t *testing.T, metadata []byte) string {
	t.Helper()
	var meta struct {
		ScheduledAt string `json:"scheduled_at"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatalf("decode metadata %s: %v", metadata, err)
	}
	if meta.ScheduledAt == "" {
		t.Fatalf("metadata has no scheduled_at: %s", metadata)
	}
	return meta.ScheduledAt
}

// The per-schedule bound is applied by the replay, not merely stored: a long
// outage replays at most max_catch_up occurrences and the rest are skipped. A
// schedule with no bound gets the daemon default, which is what makes the
// column's zero mean "default" rather than "replay nothing".
func TestStartupReplayHonoursThePerScheduleCatchUpBound(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "ticker", `
version: 1
name: ticker
entrypoint: main.py
timeout: 30
trigger:
  cron: "0 * * * *"
  missed_policy: catch_up
  max_catch_up: 2
retry:
  attempts: 0
`, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	rec, ok := scheduleForJob(d, "ticker")
	if !ok {
		t.Fatal("the schedule was not reconciled")
	}
	if rec.CatchUpLimit() != 2 {
		t.Fatalf("stored bound resolves to %d, want 2", rec.CatchUpLimit())
	}
	// Ten hours missed, bound of two.
	last := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	d.schedules.NoteFired(rec.ID, last)
	d.replayMissedOccurrences(ctx, time.Date(2026, 10, 5, 10, 30, 0, 0, time.UTC))

	waiting, err := d.runs.ListByStatus(ctx, runs.StatusQueued, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 2 {
		t.Fatalf("replayed %d runs, want the bound of 2 (8 occurrences skipped)", len(waiting))
	}
	// The two replayed are the earliest, oldest first.
	for i, want := range []time.Time{last.Add(time.Hour), last.Add(2 * time.Hour)} {
		got, err := time.Parse(time.RFC3339Nano, mustScheduledAt(t, waiting[i].Metadata))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Errorf("replayed run %d at %s, want %s", i, got, want)
		}
	}
}
