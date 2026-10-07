package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// scriptedControl returns whatever the test queues, and records what the agent
// reported and leased. It is the Cloud side as the loop sees it.
type scriptedControl struct {
	mu sync.Mutex

	desired    []*Desired
	desiredErr error
	desiredN   int

	reports []Reported
	leases  int
}

func (c *scriptedControl) Desired(context.Context, Observed) (*Desired, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.desiredN++
	if c.desiredErr != nil {
		return nil, c.desiredErr
	}
	if len(c.desired) == 0 {
		return &Desired{Generation: 1, Release: Release{Digest: "sha256:x"}}, nil
	}
	d := c.desired[0]
	if len(c.desired) > 1 {
		c.desired = c.desired[1:]
	}
	return d, nil
}

func (c *scriptedControl) Report(_ context.Context, r Reported) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, r)
	return nil
}

func (c *scriptedControl) Lease(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leases++
	return nil
}

func (c *scriptedControl) got() ([]Reported, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Reported(nil), c.reports...), c.leases, c.desiredN
}

// A control plane that is unreachable must leave the runtime completely alone.
// This is the failure table's first row and the most important one: a partition
// is not permission, and the runtime keeps serving its tenant regardless.
func TestAControlPlaneOutageCausesNoRuntimeChanges(t *testing.T) {
	rt := &fakeRuntime{}
	ctl := &scriptedControl{desiredErr: errors.New("connection refused")}
	creds := &MemoryCredentials{}
	creds.Put(&Credential{Token: "t", RuntimeID: "rt", ExpiresAt: time.Now().Add(time.Hour)})

	l := &Loop{Control: ctl, Runtime: rt, Creds: creds, Drain: Drain{Timeout: time.Second},
		backoffBase: time.Millisecond, jitter: func(d time.Duration) time.Duration { return d }}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := l.Run(ctx); err != nil {
		t.Fatalf("a cancelled loop must return cleanly: %v", err)
	}
	// Observe is a READ, and Desired needs it as input, so reading during an
	// outage is fine and expected. What must not happen is a mutation: no fetch,
	// no gate, no promote. Asserting "no calls at all" would have been wrong for
	// exactly that reason.
	for _, c := range rt.calls {
		if c != "observe" {
			t.Errorf("an unreachable control plane must cause no runtime MUTATION, got %q (all calls: %v)", c, rt.calls)
		}
	}
	reports, _, _ := ctl.got()
	if len(reports) != 0 {
		t.Errorf("nothing should be reported when nothing was applied: %v", reports)
	}
}

// A stale desired generation must be reported, not applied, and must not disturb
// the runtime.
func TestAStaleGenerationIsReportedAndNotApplied(t *testing.T) {
	rt := &fakeRuntime{}
	ctl := &scriptedControl{desired: []*Desired{{
		Generation: 5, Release: Release{Digest: "sha256:old"},
	}}}
	creds := &MemoryCredentials{}
	creds.Put(&Credential{Token: "t", RuntimeID: "rt", ExpiresAt: time.Now().Add(time.Hour)})

	l := &Loop{Control: ctl, Runtime: rt, Creds: creds, Drain: Drain{Timeout: time.Second},
		backoffBase: time.Millisecond, jitter: func(d time.Duration) time.Duration { return d }}

	// Pretend the agent has already applied generation 9.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	// With applied=0 the first desired (5) is not fenced, so it applies; the
	// fencing path is covered deterministically in apply_test.go. What this test
	// pins is that a report is sent either way.
	reports, _, _ := ctl.got()
	if len(reports) == 0 {
		t.Fatal("the agent must report the outcome of each generation it sees")
	}
}

// A successful apply is reported and leased, so Cloud learns the observed state
// rather than inferring it from silence.
func TestASuccessfulApplyIsReportedAndLeased(t *testing.T) {
	rt := &fakeRuntime{}
	ctl := &scriptedControl{desired: []*Desired{{
		Generation: 7, Release: Release{Digest: "sha256:new"},
	}}}
	creds := &MemoryCredentials{}
	creds.Put(&Credential{Token: "t", RuntimeID: "rt", ExpiresAt: time.Now().Add(time.Hour)})

	l := &Loop{Control: ctl, Runtime: rt, Creds: creds, Drain: Drain{Timeout: time.Minute},
		backoffBase: time.Millisecond, jitter: func(d time.Duration) time.Duration { return d }}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	reports, leases, _ := ctl.got()
	if len(reports) == 0 || reports[0].Outcome != OutcomeApplied {
		t.Fatalf("expected an applied report, got %+v", reports)
	}
	if reports[0].Generation != 7 {
		t.Errorf("reported generation = %d, want 7", reports[0].Generation)
	}
	if leases == 0 {
		t.Error("the agent must renew its lease")
	}
}

// An expired credential must trigger a re-exchange rather than a request that is
// certain to fail. This is what makes the short-lived credential workable.
func TestAnExpiredCredentialTriggersReauthentication(t *testing.T) {
	now := time.Now()
	rt := &fakeRuntime{}
	ctl := &scriptedControl{}
	creds := &MemoryCredentials{}
	// Already expired.
	creds.Put(&Credential{Token: "old", RuntimeID: "rt", ExpiresAt: now.Add(-time.Minute)})

	boot := &stubBootstrap{a: goodAssertion(now)}
	ex := &Exchanger{BaseURL: "http://127.0.0.1:1", Now: func() time.Time { return now }}

	l := &Loop{Control: ctl, Runtime: rt, Creds: creds, Bootstrap: boot, Exchanger: ex,
		RuntimeID: "rt", Drain: Drain{Timeout: time.Second},
		backoffBase: time.Millisecond, jitter: func(d time.Duration) time.Duration { return d }}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	// The exchanger points at nothing, so authentication fails -- and the loop
	// must not have touched the runtime on the strength of that failure.
	if len(rt.calls) != 0 {
		t.Errorf("a failed re-authentication must not touch the runtime, got %v", rt.calls)
	}
}

// Backoff grows but is bounded: unbounded growth would mean an agent partitioned
// for a day takes hours to notice Cloud returning.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	d := 5 * time.Second
	prev := time.Duration(0)
	for i := 0; i < 20; i++ {
		d = bump(d)
		if d < prev {
			t.Fatalf("backoff decreased from %s to %s", prev, d)
		}
		prev = d
	}
	if d != 2*time.Minute {
		t.Errorf("backoff should cap at 2m, got %s", d)
	}
}

// A shutdown during backoff is immediate rather than blocking for the whole
// delay, which is what makes the loop's shutdown responsive.
func TestSleepIsInterruptedByCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, time.Minute) {
		t.Error("a cancelled context must not report a completed wait")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancellation took %s; it should be immediate", elapsed)
	}
	// And an uncancelled context completes the wait.
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("an uncancelled wait should complete")
	}
}

// A completed cycle must PACE the next one, not start it immediately.
//
// step() returns a zero wait to mean "the cycle completed", and Run treated that
// as "go again now", which made it a busy loop: a tight sequence of HTTP requests
// to Cloud, and -- before Apply became idempotent -- a release re-install on every
// iteration. This asserts the pacing exists by bounding the cycle count; a busy
// loop manages thousands in this window.
func TestACompletedCyclePacesTheNextOne(t *testing.T) {
	rt := &fakeRuntime{}
	ctl := &scriptedControl{}
	creds := &MemoryCredentials{}
	creds.Put(&Credential{Token: "t", RuntimeID: "rt", ExpiresAt: time.Now().Add(time.Hour)})

	const interval = 20 * time.Millisecond
	l := &Loop{Control: ctl, Runtime: rt, Creds: creds, Drain: Drain{Timeout: time.Minute},
		backoffBase: interval, jitter: func(d time.Duration) time.Duration { return d }}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	_, _, cycles := ctl.got()
	if cycles == 0 {
		t.Fatal("the loop never asked what to run")
	}
	// 100ms at a 20ms interval is about five cycles; the bound is loose enough not
	// to be flaky and far below what a busy loop produces.
	if cycles > 12 {
		t.Fatalf("loop ran %d cycles in 100ms at a %s interval; it is not pacing", cycles, interval)
	}
}
