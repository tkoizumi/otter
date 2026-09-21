package scheduler

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/logging"
)

func testLogger() *logging.Logger {
	return logging.New(io.Discard, logging.FormatJSON, logging.LevelError)
}

func TestReplaceAndInspect(t *testing.T) {
	s := New(testLogger())

	if err := s.Replace("shopify", "*/5 * * * *", func() {}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := s.Replace("nightly", "0 2 * * *", func() {}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	if s.Count() != 2 {
		t.Errorf("count = %d, want 2", s.Count())
	}
	if spec, ok := s.Spec("shopify"); !ok || spec != "*/5 * * * *" {
		t.Errorf("spec = %q (ok=%v), want */5 * * * *", spec, ok)
	}
	if _, ok := s.Spec("missing"); ok {
		t.Error("an unregistered integration should not report a spec")
	}
	if _, ok := s.Next("shopify"); ok {
		t.Error("Next should be unknown before the scheduler starts")
	}
}

func TestReplaceRejectsBadInput(t *testing.T) {
	s := New(testLogger())

	if err := s.Replace("a", "*/5 * * * *", func() {}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := s.Replace("b", "not a cron", func() {}); err == nil {
		t.Error("an invalid cron expression should fail")
	}
	if err := s.Replace("c", "", func() {}); err == nil {
		t.Error("an empty cron expression should fail")
	}
	if err := s.Replace("d", "* * * * *", nil); err == nil {
		t.Error("a nil job should fail")
	}
	if s.Count() != 1 {
		t.Errorf("count = %d, want 1: failed registrations must not count", s.Count())
	}
}

// Replacing with the same expression must keep the live entry, because the
// entry carries the already-computed next fire time. A reload passes every
// integration through Replace, and re-adding an unchanged schedule would move
// that time and could skip an occurrence.
func TestReplaceKeepsAnUnchangedEntry(t *testing.T) {
	s := New(testLogger())

	if err := s.Replace("ticker", "@every 1h", func() {}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	s.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	next, ok := waitForNext(t, s, "ticker")
	if !ok {
		t.Fatal("the entry never reported a next fire time")
	}

	if err := s.Replace("ticker", "@every 1h", func() {}); err != nil {
		t.Fatalf("replace with an identical expression: %v", err)
	}

	after, ok := s.Next("ticker")
	if !ok {
		t.Fatal("the entry disappeared after a no-op replace")
	}
	if !after.Equal(next) {
		t.Errorf("next fire time moved from %s to %s: an unchanged schedule must not be re-added", next, after)
	}
	if s.Count() != 1 {
		t.Errorf("count = %d, want 1", s.Count())
	}
}

func TestReplaceSwapsAChangedEntryAndUnregisterRemovesIt(t *testing.T) {
	s := New(testLogger())

	if err := s.Replace("ticker", "@every 1h", func() {}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if spec, ok := s.Spec("ticker"); !ok || spec != "@every 1h" {
		t.Fatalf("spec = %q (ok=%v), want @every 1h", spec, ok)
	}

	if err := s.Replace("ticker", "@every 2h", func() {}); err != nil {
		t.Fatalf("replace with a new expression: %v", err)
	}
	if spec, ok := s.Spec("ticker"); !ok || spec != "@every 2h" {
		t.Errorf("spec = %q (ok=%v), want @every 2h", spec, ok)
	}
	if s.Count() != 1 {
		t.Errorf("count = %d, want 1: a swap must not leave the old entry behind", s.Count())
	}

	s.Unregister("ticker")
	if s.Count() != 0 {
		t.Errorf("count = %d after unregister, want 0", s.Count())
	}
	if _, ok := s.Spec("ticker"); ok {
		t.Error("an unregistered integration should not report a spec")
	}

	// Removing something that is not there is the state a reload wanted, so it
	// must not be an error or a surprise.
	s.Unregister("ticker")
}

func TestIDsReportsRegisteredTriggers(t *testing.T) {
	s := New(testLogger())
	for _, id := range []string{"a", "b"} {
		if err := s.Replace(id, "@every 1h", func() {}); err != nil {
			t.Fatalf("replace %s: %v", id, err)
		}
	}

	got := map[string]bool{}
	for _, id := range s.IDs() {
		got[id] = true
	}
	if len(got) != 2 || !got["a"] || !got["b"] {
		t.Errorf("IDs() = %v, want a and b", s.IDs())
	}

	s.Unregister("a")
	if ids := s.IDs(); len(ids) != 1 || ids[0] != "b" {
		t.Errorf("IDs() = %v after unregister, want [b]", ids)
	}
}

// waitForNext polls until the running cron runner has computed a fire time.
func waitForNext(t *testing.T, s *Scheduler, id string) (time.Time, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if next, ok := s.Next(id); ok && !next.IsZero() {
			return next, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Time{}, false
}

func TestParseAcceptsStandardExpressionsAndDescriptors(t *testing.T) {
	for _, spec := range []string{"*/5 * * * *", "0 2 * * *", "@hourly", "@daily", "@every 30s"} {
		if _, err := Parse(spec); err != nil {
			t.Errorf("Parse(%q) = %v, want nil", spec, err)
		}
	}
	for _, spec := range []string{"", "* * * *", "nope", "99 * * * *"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", spec)
		}
	}
}

func TestJobFiresAndNextIsExposed(t *testing.T) {
	s := New(testLogger())

	fired := make(chan struct{}, 1)
	if err := s.Replace("ticker", "@every 100ms", func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	s.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()

	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("the cron job never fired")
	}

	// The cron runner populates Next once it is running.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if next, ok := s.Next("ticker"); ok && !next.IsZero() {
			if next.Before(time.Now().Add(-time.Minute)) {
				t.Errorf("next fire time %s is in the past", next)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Next never reported a fire time for a running trigger")
}

func TestEntriesReportsEveryTrigger(t *testing.T) {
	s := New(testLogger())
	for _, id := range []string{"a", "b"} {
		if err := s.Replace(id, "@every 1h", func() {}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}

	s.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries := s.Entries()
		if len(entries) == 2 {
			for id, next := range entries {
				if next.IsZero() {
					t.Errorf("entry %s has a zero next time", id)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Entries never reported both triggers")
}

func TestJobPanicIsRecoveredAndDoesNotStopTheScheduler(t *testing.T) {
	s := New(testLogger())

	var (
		mu    sync.Mutex
		after bool
	)

	panicking := s.wrap("boom", func() { panic("kaboom") })
	// Must not propagate the panic.
	panicking()

	// A later job still runs, which is how we observe that the runner and the
	// process survived.
	s.wrap("ok", func() {
		mu.Lock()
		after = true
		mu.Unlock()
	})()

	mu.Lock()
	defer mu.Unlock()
	if !after {
		t.Error("the scheduler did not continue after a panicking job")
	}
}

func TestStopIsSafeWithoutStart(t *testing.T) {
	s := New(testLogger())
	if err := s.Replace("a", "@every 1h", func() {}); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Errorf("Stop before Start = %v, want nil", err)
	}
}

func TestConcurrentRegistrationIsSafe(t *testing.T) {
	s := New(testLogger())

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Replace("same", "@every 1h", func() {})
		}()
	}
	wg.Wait()

	if s.Count() != 1 {
		t.Errorf("count = %d, want 1: only one registration should win", s.Count())
	}
}
