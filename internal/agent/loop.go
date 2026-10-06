package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
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
	return 5 * time.Second
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
		if wait > 0 {
			if !sleepCtx(ctx, wait) {
				return nil
			}
			backoff = bump(backoff)
			continue
		}
		backoff = l.base()
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
		l.logger().Warn("agent_runtime_observe_failed", "error", err.Error())
		return l.applyJitter(l.base()), nil
	}

	want, err := l.Control.Desired(ctx, observed)
	if err != nil {
		// Cloud unreachable: keep running the current release. This is the
		// table's first row and the most important one -- a partition must not
		// cause the agent to do anything at all.
		l.logger().Warn("agent_control_unavailable", "error", err.Error())
		return l.applyJitter(l.base()), nil
	}

	// The runtime id travels back on every report: the control plane refuses a
	// report that does not name one, so an omission here would make every report
	// fail while every local test passed.
	want.RuntimeID = l.RuntimeID
	rep, applyErr := Apply(ctx, l.Runtime, want, gen.applied, l.Drain)
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
	wait, err := l.step(ctx, &gen)
	if err != nil {
		return err
	}
	if wait > 0 {
		// A single iteration should not sleep: the caller asked for one cycle.
		l.logger().Info("agent_once_retry_suggested", "after", wait.String())
	}
	return nil
}
