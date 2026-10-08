package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// Loop is the agent's steady state: ask what to run, apply it, report, and renew
// the lease.
//
// The failure semantics are the interesting part, and they are all "do nothing
// drastic". An agent never self-upgrades, self-restores, or assumes it is still
// wanted: a partition is not permission. Pull makes that easy to honour, because
// the runtime keeps serving its tenants whether or not Cloud is reachable --
// which is the property the whole design buys.
type Loop struct {
	Control ControlPlane
	Runtime Runtime
	// Creds is checked before each request so an expired credential triggers a
	// re-exchange rather than a request that is certain to fail.
	Creds CredentialStore
	// Bootstrap re-obtains a credential when the held one is gone or expired.
	Bootstrap Bootstrap
	Exchanger *Exchanger
	// RuntimeID and AgentVersion are sent with bootstrap and reports.
	RuntimeID    string
	AgentVersion string
	// Drain bounds one apply.
	Drain Drain
	// Logger, if set, records what happened. Never logs secret material.
	Logger *slog.Logger
	// now and backoffBase are injectable so tests do not sleep.
	now         func() time.Time
	backoffBase time.Duration
	// jitter is injectable so backoff is deterministic under test.
	jitter func(time.Duration) time.Duration

	// mu guards lastErr, which records why the last cycle did not complete. Both
	// exist so a one-shot diagnostic can name WHICH component was unreachable --
	// "did not complete" alone sends an operator to the wrong one.
	mu      sync.Mutex
	lastErr string
	// reachedControl is set once Desired has ANSWERED. A cycle that never asked
	// is not a completed cycle, and --once reported success for one -- twice.
	reachedControl bool
}

func (l *Loop) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func (l *Loop) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *Loop) base() time.Duration {
	if l.backoffBase > 0 {
		return l.backoffBase
	}
	// Thirty seconds -- a pure cost/deploy-latency dial again.
	//
	// The interval is the price of asking "anything for me?" across a boundary
	// that only opens one way: the agent dials out, Cloud cannot dial in. This
	// loop asks whether a NEW RELEASE should be applied; it no longer carries
	// dashboard reads.
	//
	// It was two seconds as a stopgap for exactly one reason: every pooled
	// runtime's dashboard read was a synchronous round trip that waited for this
	// cycle, so page loads were only as fast as this number and thirty seconds
	// made the job and run pages unusable. Reads no longer ride this number.
	// They go over the separate control channel, whose idle request Cloud HOLDS
	// open (`awaitControl` in `cloud/worker/control-store.ts`), and the agent
	// re-requests immediately after any response that carried work. The apply
	// loop's interval therefore no longer decides read latency at all.
	//
	// WHAT IT STILL DECIDES: how quickly a runtime picks up a new release --
	// thirty seconds of deploy latency, accepted deliberately -- and its share
	// of the request bill. It is a cost/deploy-latency dial and nothing else. Do
	// not lower it again to make a read faster; fix the read's path instead. Do
	// not raise it without deciding that a slower deploy is worth less spend.
	return 30 * time.Second
}

func (l *Loop) applyJitter(d time.Duration) time.Duration {
	if l.jitter != nil {
		return l.jitter(d)
	}
	// Jitter matters when a control plane returns and every agent in a fleet
	// retries at the same instant.
	return d/2 + time.Duration(rand.Int63n(int64(d/2+1)))
}

// generation is the agent's own view of what it has applied. It is passed to
// Desired so Cloud can see what the agent believes, and used to fence a desired
// state that is behind it.
type generationTracker struct {
	applied int64
}

func (g *generationTracker) observe(rep Reported) {
	if rep.Outcome == OutcomeApplied && rep.Generation > g.applied {
		g.applied = rep.Generation
	}
}

// Run drives the loop until the context is cancelled.
//
// Every error path either backs off and retries, or returns -- and the returns
// are reserved for conditions where continuing would be wrong. Nothing here
// touches the runtime on the strength of a failure: the default action on
// trouble is to keep serving what is already running.
func (l *Loop) Run(ctx context.Context) error {
	if l.Control == nil || l.Runtime == nil {
		return fmt.Errorf("agent: loop needs a control plane and a runtime")
	}
	var gen generationTracker
	backoff := l.base()

	for {
		if ctx.Err() != nil {
			return nil
		}

		// Refresh the credential before using it. A request with an expired
		// credential is a guaranteed failure, and re-asserting is the path that
		// exists precisely so this is cheap.
		if l.Creds == nil || l.Creds.Expired(l.clock()) {
			if err := l.bootstrap(ctx); err != nil {
				delay := l.applyJitter(backoff)
				if errors.Is(err, ErrBootstrapRefused) {
					// Refused is not retryable in the same way: the host is not
					// the one Cloud expects, or the registration was revoked.
					// Back off hard and say so, rather than hammering.
					l.logger().Error("agent_bootstrap_refused", "error", err.Error(), "retry_in", delay.String())
					delay = maxDuration(delay, 5*time.Minute)
				} else {
					l.logger().Warn("agent_bootstrap_unavailable", "error", err.Error(), "retry_in", delay.String())
				}
				if !sleepCtx(ctx, delay) {
					return nil
				}
				backoff = bump(backoff)
				continue
			}
			backoff = l.base()
		}

		wait, err := l.step(ctx, &gen)
		if err != nil {
			return err
		}
		if wait <= 0 {
			// The cycle COMPLETED: pace the next one instead of starting it at
			// once. Zero means "nothing suggests a retry", not "poll immediately",
			// and treating it as the latter made Run a busy loop -- a tight
			// sequence of HTTP requests to Cloud once Apply became idempotent, and
			// a re-install per iteration before that.
			wait = l.applyJitter(l.base())
			backoff = l.base()
		} else {
			backoff = bump(backoff)
		}
		if !sleepCtx(ctx, wait) {
			return nil
		}
	}
}

// step performs one cycle: observe, ask, apply, report, lease.
//
// It returns a suggested backoff rather than sleeping, so Run and RunOnce share
// the same logic -- a diagnostic that took a different path from the real loop
// would be worse than no diagnostic. A zero wait means the cycle completed and
// the caller may continue immediately.
func (l *Loop) step(ctx context.Context, gen *generationTracker) (time.Duration, error) {
	observed, err := l.Runtime.Observe(ctx)
	if err != nil {
		// The runtime being unreachable is not a reason to change anything:
		// there is nothing to change it to. Retry.
		l.note("the local runtime at the configured -runtime-url could not be observed: " + err.Error())
		l.logger().Warn("agent_runtime_observe_failed", "error", err.Error())
		return l.applyJitter(l.base()), nil
	}

	want, err := l.Control.Desired(ctx, observed)
	if err == nil {
		l.mu.Lock()
		l.reachedControl = true
		l.mu.Unlock()
	}
	if err != nil {
		// Cloud unreachable: keep running the current release. This is the
		// table's first row and the most important one -- a partition must not
		// cause the agent to do anything at all.
		l.note("the control plane could not be asked what to run: " + err.Error())
		l.logger().Warn("agent_control_unavailable", "error", err.Error())
		return l.applyJitter(l.base()), nil
	}

	// The runtime id travels back on every report: the control plane refuses a
	// report that does not name one, so an omission here would make every report
	// fail while every local test passed.
	want.RuntimeID = l.RuntimeID
	rep, applyErr := Apply(ctx, l.Runtime, want, gen.applied, l.Drain, observed)
	rep.RuntimeID = l.RuntimeID
	gen.observe(rep)
	if applyErr != nil {
		switch {
		case errors.Is(applyErr, ErrFenced):
			// Applies nothing and re-reports. Cloud reconciles; the agent does
			// not try to catch up, because guessing which way to move is how a
			// runtime rolls backwards.
			l.logger().Info("agent_generation_fenced", "desired", want.Generation, "applied", gen.applied)
		case errors.Is(applyErr, ErrDrainTimeout):
			l.logger().Warn("agent_deploy_abandoned", "generation", want.Generation, "reason", rep.Reason)
		default:
			l.logger().Warn("agent_apply_error", "generation", want.Generation, "error", applyErr.Error())
		}
	} else {
		l.logger().Info("agent_apply", "generation", want.Generation, "outcome", rep.Outcome)
	}

	if err := l.Control.Report(ctx, rep); err != nil {
		// A failed report does not undo an applied release, and retrying the
		// apply would be worse than re-reporting later. Log and move on.
		l.logger().Warn("agent_report_failed", "generation", rep.Generation, "error", err.Error())
	}
	if err := l.Control.Lease(ctx); err != nil {
		// A lapsed lease makes the runtime visible as stale. It does not make it
		// eligible for anything, and it does not stop serving.
		l.logger().Warn("agent_lease_failed", "error", err.Error())
	}
	return 0, nil
}

// bootstrap obtains a credential via the Bootstrap provider.
func (l *Loop) bootstrap(ctx context.Context) error {
	if l.Exchanger == nil || l.Bootstrap == nil {
		return fmt.Errorf("agent: no bootstrap mechanism configured")
	}
	cred, err := l.Exchanger.Exchange(ctx, l.Bootstrap, l.RuntimeID, l.AgentVersion)
	if err != nil {
		return err
	}
	if l.Creds != nil {
		l.Creds.Put(cred)
	}
	l.logger().Info("agent_authenticated", "runtime_id", cred.RuntimeID, "expires_at", cred.ExpiresAt.UTC().Format(time.RFC3339))
	return nil
}

// bump grows the backoff toward a ceiling. Unbounded growth would mean an agent
// that has been partitioned for a day takes hours to notice Cloud returning.
func bump(d time.Duration) time.Duration {
	next := d * 2
	if next > 2*time.Minute {
		return 2 * time.Minute
	}
	return next
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// sleepCtx waits, and reports whether the wait completed rather than being
// cancelled -- so a shutdown during backoff is immediate instead of blocking for
// the whole delay.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunOnce performs one iteration and returns, for an operator answering "can
// this host reach its control plane and authenticate" without leaving a process
// running, and for tests that need one cycle rather than a loop.
//
// It shares step() with Run so the two cannot drift: a diagnostic that took a
// different path from the real loop would be worse than no diagnostic.
func (l *Loop) RunOnce(ctx context.Context) error {
	if l.Control == nil || l.Runtime == nil {
		return fmt.Errorf("agent: loop needs a control plane and a runtime")
	}
	var gen generationTracker
	if l.Creds == nil || l.Creds.Expired(l.clock()) {
		if err := l.bootstrap(ctx); err != nil {
			return err
		}
	}
	// What the loop WOULD have waited before retrying. A non-zero value means the
	// cycle did not complete a full pass -- typically because the runtime or the
	// control plane was unreachable -- and reporting that as success would be a
	// diagnostic that lies about the thing it exists to diagnose.
	wait, err := l.step(ctx, &gen)
	if err != nil {
		return err
	}
	// A cycle that never got an answer from the control plane did NOT complete,
	// whatever the retry timing says. This was wrong twice: the loop treats an
	// unreachable runtime or control plane as a retryable condition and returns no
	// error, so `--once` printed "one iteration completed" for a run that never
	// asked what to run. A diagnostic that reports success for the thing it exists
	// to diagnose is worse than no diagnostic.
	l.mu.Lock()
	reached := l.reachedControl
	l.mu.Unlock()
	if wait > 0 || !reached {
		reason := l.lastReason()
		if reached {
			reason = "the cycle completed but suggests a retry"
		}
		return fmt.Errorf("%w: %s (retry suggested in %s)", errIncompleteCycle, reason, wait)
	}
	return nil
}

// lastReason names why the last cycle did not complete. A diagnostic that says
// only "did not complete" sends the operator to the wrong component; the whole
// value of a one-shot run is knowing whether it was the runtime or the control
// plane, and what the far side said.
func (l *Loop) lastReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastErr == "" {
		return "the cycle did not complete"
	}
	return l.lastErr
}

// errIncompleteCycle means one iteration did not complete a pass. It is distinct
// from a failure: nothing is broken, something was simply not reachable yet.
var errIncompleteCycle = fmt.Errorf("agent: incomplete cycle")

// note records why the last cycle did not complete.
//
// Guarded because Run and RunOnce may share a Loop, and a diagnostic that raced
// the loop it reports on would be worse than none.
func (l *Loop) note(why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastErr = why
}
