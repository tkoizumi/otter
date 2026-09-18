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

func TestRegisterAndInspect(t *testing.T) {
	s := New(testLogger())

	if err := s.Register("shopify", "*/5 * * * *", func() {}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.Register("nightly", "0 2 * * *", func() {}); err != nil {
		t.Fatalf("register: %v", err)
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

func TestRegisterRejectsDuplicatesAndBadInput(t *testing.T) {
	s := New(testLogger())

	if err := s.Register("a", "*/5 * * * *", func() {}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.Register("a", "* * * * *", func() {}); err == nil {
		t.Error("registering the same integration twice should fail")
	}
	if err := s.Register("b", "not a cron", func() {}); err == nil {
		t.Error("an invalid cron expression should fail")
	}
	if err := s.Register("c", "", func() {}); err == nil {
		t.Error("an empty cron expression should fail")
	}
	if err := s.Register("d", "* * * * *", nil); err == nil {
		t.Error("a nil job should fail")
	}
	if s.Count() != 1 {
		t.Errorf("count = %d, want 1: failed registrations must not count", s.Count())
	}
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
	if err := s.Register("ticker", "@every 100ms", func() {
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
		if err := s.Register(id, "@every 1h", func() {}); err != nil {
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
	if err := s.Register("a", "@every 1h", func() {}); err != nil {
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
			_ = s.Register("same", "@every 1h", func() {})
		}()
	}
	wg.Wait()

	if s.Count() != 1 {
		t.Errorf("count = %d, want 1: only one registration should win", s.Count())
	}
}
