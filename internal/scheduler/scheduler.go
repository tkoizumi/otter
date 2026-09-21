// Package scheduler registers cron triggers.
//
// Cron schedules come from integration manifests and are reconciled from disk
// on every daemon start and on every reload, so a restart never loses a future
// schedule and a reload never disturbs one whose expression did not change.
// Occurrences missed while the daemon was down are deliberately not replayed.
package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/tkoizumi/otter/internal/logging"
)

// Scheduler wraps a cron runner keyed by integration id.
type Scheduler struct {
	logger *logging.Logger
	cron   *cron.Cron

	mu    sync.Mutex
	ids   map[string]cron.EntryID
	specs map[string]string
}

// New creates an empty scheduler. The parser accepts the standard five-field
// cron expression (minute hour day-of-month month day-of-week) plus the
// familiar @hourly / @daily descriptors.
func New(logger *logging.Logger) *Scheduler {
	parser := cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)
	return &Scheduler{
		logger: logger,
		cron: cron.New(
			cron.WithParser(parser),
			cron.WithLogger(&cronLogger{logger: logger}),
		),
		ids:   map[string]cron.EntryID{},
		specs: map[string]string{},
	}
}

// Parse validates a cron expression without registering it.
func Parse(spec string) (cron.Schedule, error) {
	parser := cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)
	return parser.Parse(spec)
}

// Replace makes an integration's cron trigger match spec: adding one when it
// has none, leaving an unchanged one exactly as it is, or swapping a changed
// one.
//
// Leaving an unchanged trigger alone is the point, not an optimisation. The
// cron runner computes an entry's next fire time when the entry is added, so
// re-adding an unchanged schedule would recompute it from the moment of the
// call and can skip an occurrence that was about to fire. Reload calls Replace
// for every integration on every pass, so this is the common path.
func (s *Scheduler) Replace(integrationID, spec string, fn func()) error {
	if spec == "" {
		return fmt.Errorf("scheduler: empty cron expression for %s", integrationID)
	}
	if fn == nil {
		return fmt.Errorf("scheduler: nil job for %s", integrationID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID, exists := s.ids[integrationID]; exists {
		if s.specs[integrationID] == spec {
			return nil
		}
		s.cron.Remove(entryID)
		delete(s.ids, integrationID)
		delete(s.specs, integrationID)
	}

	entryID, err := s.cron.AddFunc(spec, s.wrap(integrationID, fn))
	if err != nil {
		return fmt.Errorf("scheduler: register %s (%s): %w", integrationID, spec, err)
	}

	s.ids[integrationID] = entryID
	s.specs[integrationID] = spec
	return nil
}

// Unregister drops an integration's cron trigger.
//
// Unregistering an integration that has none is not an error: a reload
// reconciles the whole set against the manifests, and "already absent" is the
// state it was trying to reach.
func (s *Scheduler) Unregister(integrationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entryID, exists := s.ids[integrationID]
	if !exists {
		return
	}
	s.cron.Remove(entryID)
	delete(s.ids, integrationID)
	delete(s.specs, integrationID)
}

// IDs returns every integration that currently has a cron trigger.
func (s *Scheduler) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}

// wrap recovers from panics so one bad job cannot take down the cron runner.
func (s *Scheduler) wrap(integrationID string, fn func()) func() {
	return func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("cron_job_panic", fmt.Errorf("panic: %v", r), "integration", integrationID)
			}
		}()
		fn()
	}
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

// Next returns the next scheduled fire time for an integration.
func (s *Scheduler) Next(integrationID string) (time.Time, bool) {
	s.mu.Lock()
	entryID, ok := s.ids[integrationID]
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

// Spec returns the registered cron expression for an integration.
func (s *Scheduler) Spec(integrationID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[integrationID]
	return spec, ok
}

// Count returns how many cron triggers are registered.
func (s *Scheduler) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ids)
}

// Entries returns every registered integration id with its next fire time.
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
