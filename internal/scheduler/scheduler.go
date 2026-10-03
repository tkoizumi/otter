// Package scheduler registers cron triggers.
//
// Triggers are keyed by schedule id, not by job: a job may hold several
// schedules, each with its own expression and time zone. The runner is
// reconciled from the schedule store on every daemon start and on every reload,
// so a restart never loses a future schedule and a reload never disturbs one
// whose expression did not change. Occurrences missed while the daemon was down
// are deliberately not replayed.
package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/tkoizumi/otter/internal/logging"
)

// Scheduler wraps a cron runner keyed by schedule id.
type Scheduler struct {
	logger *logging.Logger
	cron   *cron.Cron

	mu        sync.Mutex
	ids       map[string]cron.EntryID
	specs     map[string]string
	schedules map[string]cron.Schedule
	zones     map[string]string
	// last remembers the occurrence most recently handed to a job, so a raced
	// read of the runner's own Prev field can be told apart from the fire we
	// are already handling.
	last map[string]time.Time
}

// New creates an empty scheduler. Occurrences are resolved in each schedule's
// own time zone; the runner's clock is pinned to UTC so an occurrence is always
// an absolute instant.
func New(logger *logging.Logger) *Scheduler {
	return &Scheduler{
		logger: logger,
		cron: cron.New(
			cron.WithLocation(time.UTC),
			cron.WithLogger(&cronLogger{logger: logger}),
		),
		ids:       map[string]cron.EntryID{},
		specs:     map[string]string{},
		schedules: map[string]cron.Schedule{},
		zones:     map[string]string{},
		last:      map[string]time.Time{},
	}
}

// Parse validates a cron expression without registering it. It is the same
// grammar the scheduler accepts; the location does not matter for validation.
func Parse(spec string) (cron.Schedule, error) {
	parser := cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)
	return parser.Parse(spec)
}

// Replace makes a schedule's cron trigger match spec in loc: adding one when it
// has none, leaving an unchanged one exactly as it is, or swapping a changed
// one. A nil loc means UTC.
//
// Leaving an unchanged trigger alone is the point, not an optimisation. The
// cron runner computes an entry's next fire time when the entry is added, so
// re-adding an unchanged schedule would recompute it from the moment of the
// call and can skip an occurrence that was about to fire. Reload calls Replace
// for every schedule on every pass, so this is the common path.
func (s *Scheduler) Replace(key, spec string, loc *time.Location, fn func(occurrence time.Time)) error {
	if spec == "" {
		return fmt.Errorf("scheduler: empty cron expression for %s", key)
	}
	if fn == nil {
		return fmt.Errorf("scheduler: nil job for %s", key)
	}
	if loc == nil {
		loc = time.UTC
	}
	zone := loc.String()

	// The expression is parsed in the schedule's own zone, so "0 3 * * *" means
	// 03:00 where the operator said it does rather than wherever the host's
	// clock happens to be set. robfig/cron accepts a CRON_TZ= prefix for this;
	// it is an implementation detail of the prefix rather than of the schedule,
	// so errors name the operator's expression.
	sched, err := Parse("CRON_TZ=" + zone + " " + spec)
	if err != nil {
		return fmt.Errorf("scheduler: register %s (%s): %w", key, spec, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID, exists := s.ids[key]; exists {
		if s.specs[key] == spec && s.zones[key] == zone {
			return nil
		}
		s.cron.Remove(entryID)
		delete(s.ids, key)
		delete(s.specs, key)
		delete(s.schedules, key)
		delete(s.zones, key)
	}

	entryID := s.cron.Schedule(sched, cron.FuncJob(s.wrap(key, fn)))
	s.ids[key] = entryID
	s.specs[key] = spec
	s.schedules[key] = sched
	s.zones[key] = zone
	return nil
}

// Unregister drops a schedule's cron trigger.
//
// Unregistering one that has none is not an error: a reload reconciles the
// whole set against the schedule store, and "already absent" is the state it
// was trying to reach.
func (s *Scheduler) Unregister(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entryID, exists := s.ids[key]
	if !exists {
		return
	}
	s.cron.Remove(entryID)
	delete(s.ids, key)
	delete(s.specs, key)
	delete(s.schedules, key)
	delete(s.zones, key)
	delete(s.last, key)
}

// IDs returns every schedule that currently has a cron trigger.
func (s *Scheduler) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}

// wrap recovers from panics so one bad job cannot take down the cron runner,
// and resolves the occurrence the fire belongs to.
func (s *Scheduler) wrap(key string, fn func(time.Time)) func() {
	return func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("cron_job_panic", fmt.Errorf("panic: %v", r), "schedule", key)
			}
		}()
		occurrence := s.occurrence(key)
		if occurrence.IsZero() {
			// Unreachable for a registered schedule; a defensive fallback keeps
			// the job runnable rather than silently dropping it.
			occurrence = time.Now().UTC()
		}
		fn(occurrence)
	}
}

// occurrence returns the scheduled time of the fire currently being handled.
//
// robfig/cron sets Entry.Prev immediately after it launches the job, so a read
// from the job races that write. Rather than trust a racy read, wait briefly for
// Prev to become non-zero and differ from the occurrence already reported for
// this schedule. If it never does -- a stalled runner, or a clock that could not
// be read -- fall back to stepping the expression forward from the last
// occurrence so the ledger still receives a unique, monotonic value.
func (s *Scheduler) occurrence(key string) time.Time {
	s.mu.Lock()
	entryID, ok := s.ids[key]
	sched := s.schedules[key]
	last := s.last[key]
	s.mu.Unlock()
	if !ok {
		return time.Time{}
	}

	deadline := time.Now().Add(50 * time.Millisecond)
	for {
		prev := s.cron.Entry(entryID).Prev
		if !prev.IsZero() && !prev.Equal(last) {
			at := prev.UTC()
			s.noteLast(key, at)
			return at
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Microsecond)
	}

	if sched == nil {
		return time.Time{}
	}
	var at time.Time
	if last.IsZero() {
		at = sched.Next(time.Now().Add(-time.Minute)).UTC()
		if at.IsZero() {
			return time.Time{}
		}
	} else {
		at = sched.Next(last).UTC()
		if at.IsZero() {
			return time.Time{}
		}
	}
	s.noteLast(key, at)
	return at
}

func (s *Scheduler) noteLast(key string, at time.Time) {
	s.mu.Lock()
	s.last[key] = at
	s.mu.Unlock()
}

// Start begins running registered jobs.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop stops the scheduler and waits briefly for in-flight jobs. Jobs are
// expected to be quick: they only enqueue runs.
func (s *Scheduler) Stop(ctx context.Context) error {
	stopped := s.cron.Stop()
	select {
	case <-stopped.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Next returns the next scheduled fire time for a schedule.
func (s *Scheduler) Next(key string) (time.Time, bool) {
	s.mu.Lock()
	entryID, ok := s.ids[key]
	s.mu.Unlock()
	if !ok {
		return time.Time{}, false
	}
	entry := s.cron.Entry(entryID)
	if entry.Next.IsZero() {
		return time.Time{}, false
	}
	return entry.Next.UTC(), true
}

// Spec returns the registered cron expression for a schedule.
func (s *Scheduler) Spec(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[key]
	return spec, ok
}

// Count returns how many cron triggers are registered.
func (s *Scheduler) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ids)
}

// Entries returns every registered schedule id with its next fire time.
func (s *Scheduler) Entries() map[string]time.Time {
	s.mu.Lock()
	ids := make(map[string]cron.EntryID, len(s.ids))
	for id, entryID := range s.ids {
		ids[id] = entryID
	}
	s.mu.Unlock()

	out := make(map[string]time.Time, len(ids))
	for id, entryID := range ids {
		if next := s.cron.Entry(entryID).Next; !next.IsZero() {
			out[id] = next.UTC()
		}
	}
	return out
}

// cronLogger routes robfig/cron's own chatter into the Otter logger.
type cronLogger struct {
	logger *logging.Logger
}

func (c *cronLogger) Info(msg string, kv ...any) {
	c.logger.Debug("cron", append([]any{"message", msg}, kv...)...)
}

func (c *cronLogger) Error(err error, msg string, kv ...any) {
	c.logger.Error("cron_error", err, append([]any{"message", msg}, kv...)...)
}
