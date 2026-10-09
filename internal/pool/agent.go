package pool

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Agent is the host's steady state: ask for work, do it, report what happened.
//
// THE ORDER OF THE THREE STEPS IS THE DESIGN, and each is deliberately separate so
// a crash between them is recoverable rather than invisible:
//
//  1. **ask** — the control plane hands out a plan with a lease. Nothing has
//     happened on this machine yet, so a crash here costs only the lease, which
//     expires and lets another host take the work.
//  2. **do** — provision or reconcile. This is the only step with side effects, and
//     it is designed to be repeatable: a matching container is success, not a
//     conflict.
//  3. **report** — the ONLY thing that tells Cloud a runtime exists. If this is lost,
//     the lease expires and the work is handed out again, where step 2 reconciles
//     and step 3 succeeds. That is why the bootstrap secret is stable across
//     attempts: the container created by a lost attempt must still authenticate.
//
// A FAILURE IS ALWAYS REPORTED, never retried silently. A host that keeps a failing
// tenant to itself is a host whose queue never drains and whose operator never
// learns why.
type Agent struct {
	Client *Client
	// CloudURL is the control plane address written into each tenant's container.
	// It is the host's own value rather than the client's base URL, because the
	// tenant must reach the same control plane over the public internet and those are
	// not always the same string.
	CloudURL string
	Options  Options
}

// Run polls until the context is cancelled, or once when `Once` is set.
func (a *Agent) Run(ctx context.Context) error {
	o := a.Options.withDefaults()
	if a.Client == nil {
		return errors.New("pool: no control-plane client")
	}
	if o.Once {
		return a.Step(ctx)
	}
	// The loop never gives up on an unreachable control plane: a pool host is
	// long-lived infrastructure, and a network partition is not permission to stop
	// reporting. The backoff is bounded so recovery is quick without hammering.
	backoff := o.PollInterval
	for {
		err := a.Step(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			backoff = o.PollInterval
		case IsUnauthorized(err):
			// A refused credential will not become valid by waiting, and retrying it
			// forever burns the control plane's request budget. Reported and backed off
			// hard, which is the one case where an operator must intervene.
			o.Logger("pool agent: the control plane refused this host's pool token: %v", err)
			backoff = maxDuration(backoff*4, 5*time.Minute)
		case IsUnavailable(err):
			o.Logger("pool agent: the control plane cannot serve the host channel yet: %v", err)
			backoff = maxDuration(backoff*2, 60*time.Second)
		default:
			o.Logger("pool agent: cycle failed: %v", err)
			backoff = maxDuration(backoff*2, 60*time.Second)
		}
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
	}
}

// Step runs one cycle: observe, ask, act, report.
func (a *Agent) Step(ctx context.Context) error {
	o := a.Options.withDefaults()

	// Observe before asking, so the heartbeat carries what the host can actually see
	// rather than what it remembers. This is also how Cloud learns about drift.
	tenants, err := ListTenants(ctx, o)
	if err != nil {
		// A host that cannot list its containers can still provision; it just cannot
		// vouch for what is on it. Reported as an empty observation rather than
		// failing the cycle, so one broken `docker ps` does not stop the queue.
		o.Logger("pool agent: could not list tenants: %v", err)
		tenants = nil
	}
	count := len(tenants)
	placements := make([]Placement, 0, len(tenants))
	for _, tenant := range tenants {
		placements = append(placements, Placement{
			RuntimeID: tenant.RuntimeID,
			State:     tenant.State,
			Note:      "observed by the host agent",
		})
	}

	work, err := a.Client.Work(ctx, a.CloudURL, Report{
		Placements:  placements,
		FreeDiskMB:  FreeDiskMB(ctx, o),
		TenantCount: &count,
	})
	if err != nil {
		return err
	}
	if work == nil || work.Work == nil {
		o.Logger("pool agent: no work (capacity %d, %d assigned, %d in flight)", work.Capacity, work.Usage.Assigned, work.Usage.InFlight)
		return nil
	}

	plan := *work.Work
	o.Logger("pool agent: provisioning %s as %s (attempt %d)", plan.RuntimeID, plan.Tenant, plan.Attempts)

	outcome, execErr := Execute(ctx, plan, o)

	report := Report{RuntimeID: plan.RuntimeID}
	if execErr != nil {
		// The reason is the operator's and the user's only clue. Never a bare "failed".
		report.State = "failed"
		report.Reason = trimOutput(execErr.Error())
		report.TenantState = "unknown"
		o.Logger("pool agent: %s failed: %v", plan.RuntimeID, execErr)
	} else {
		report.State = "active"
		report.Note = outcome.Detail
		if outcome.AlreadyPresent {
			report.TenantState = "running"
		} else {
			report.TenantState = "running"
		}
		o.Logger("pool agent: %s %s", plan.RuntimeID, outcome.Detail)
	}

	if err := a.Client.Report(ctx, report); err != nil {
		// The report is the only thing that makes the runtime real, so losing it is a
		// failure of the cycle even when the container exists. The lease will expire
		// and the next attempt reconciles onto the same container.
		return fmt.Errorf("report %s: %w", plan.RuntimeID, err)
	}
	return nil
}

// maxDuration returns the larger of two durations.
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// sleepCtx waits, returning false when the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
