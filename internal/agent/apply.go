package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The types of the pull protocol. They mirror the wire shape in
// hosting/docs/agent-protocol.md; keeping them here rather than generating from
// the spec means a protocol change is a visible edit to a Go file and a test,
// not a silent regeneration.

// DesiredRequest is what the agent SENDS when asking what to run.
//
// It is a distinct type from Desired because it is a distinct message: the
// request carries the observed state the control plane reconciles against, and
// conflating the two would have the agent sending a release it has not been told
// about yet. The conformance fixtures caught exactly that conflation.
type DesiredRequest struct {
	RuntimeID    string   `json:"runtime_id"`
	Generation   int64    `json:"generation"`
	Observed     Observed `json:"observed"`
	AgentVersion string   `json:"agent_version,omitempty"`
	LeaseID      string   `json:"lease_id,omitempty"`
}

// Desired is what the control plane answers with: which release and
// configuration this runtime should be running, and its generation.
type Desired struct {
	// RuntimeID is echoed by the control plane and MUST be sent back on every
	// request: the endpoints refuse a request that does not name a runtime,
	// because a credential's binding cannot be checked without one.
	RuntimeID   string            `json:"runtime_id"`
	Generation  int64             `json:"generation"`
	Release     Release           `json:"release"`
	Config      map[string]string `json:"config,omitempty"`
	Maintenance *MaintenanceWant  `json:"maintenance,omitempty"`
	LeaseSecond int               `json:"lease_seconds,omitempty"`
}

// Release identifies an immutable, content-addressed release. The digest is the
// trust anchor: it is what makes "run release abc123" something the agent can
// verify rather than trust.
type Release struct {
	Digest string `json:"digest"`
	URL    string `json:"url"`
	Size   int64  `json:"size,omitempty"`
}

// MaintenanceWant is the control plane asking the runtime to be gated. It goes
// through the runtime's own maintenance API, so WP2's linearisation and drain
// semantics are inherited rather than reimplemented.
type MaintenanceWant struct {
	Desired string `json:"desired"`
	Reason  string `json:"reason,omitempty"`
}

// Reported is the outcome of applying a generation.
type Reported struct {
	// RuntimeID is required by the control plane. Omitting it made every report a
	// 400 with "request does not name a runtime" -- the kind of cross-language
	// divergence that unit tests on one side cannot see, and that shared protocol
	// fixtures now pin.
	RuntimeID  string            `json:"runtime_id"`
	Generation int64             `json:"generation"`
	Outcome    string            `json:"outcome"`
	Reason     string            `json:"reason,omitempty"`
	Observed   map[string]string `json:"observed,omitempty"`
}

// Outcomes. These are a closed set: the control plane decides what to do based on
// them, so an unknown value must not be invented by the agent.
const (
	OutcomeApplied       = "applied"
	OutcomeFailed        = "failed"
	OutcomeRefusedFenced = "refused_fenced"
	OutcomeInProgress    = "in_progress"
)

// ControlPlane is the Cloud side, as the agent sees it. Narrow on purpose: the
// agent authenticates outbound and asks, so this is the whole surface it has.
type ControlPlane interface {
	// Desired long-polls for the current desired state.
	Desired(ctx context.Context, observed Observed) (*Desired, error)
	// Report is idempotent, keyed by (runtime_id, generation).
	Report(ctx context.Context, r Reported) error
	// Lease renews the agent's claim to be alive.
	Lease(ctx context.Context) error
}

// Observed is what the agent reports about the runtime it is managing. Cloud
// records these as observations, never as confirmation that a command executed,
// because under a pull model no command is sent.
type Observed struct {
	ReleaseDigest  string `json:"release_digest,omitempty"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
	Maintenance    string `json:"maintenance,omitempty"`
	Health         string `json:"health,omitempty"`
}

// Runtime is the local runtime API, as the agent sees it. Everything the agent
// does to the runtime goes through the API that already exists -- the agent is a
// client, not a second mutation path, so it inherits WP2's guarantees instead of
// having to re-establish them.
type Runtime interface {
	Observe(ctx context.Context) (Observed, error)
	// EnterMaintenance gates the runtime and waits until the active count
	// reaches zero, or the deadline passes.
	EnterMaintenance(ctx context.Context, reason string, deadline time.Duration) error
	// ExitMaintenance returns the runtime to serving.
	ExitMaintenance(ctx context.Context) error
	// Promote switches the active release to the given digest.
	Promote(ctx context.Context, digest string) error
	// Fetch downloads the release at the given digest, verifying it.
	Fetch(ctx context.Context, r Release) error
	// Validate checks the candidate without executing a customer integration.
	Validate(ctx context.Context, digest string) error
}

// DrainTimeout is how long a runtime is given to finish in-flight work before an
// upgrade aborts. The protocol is explicit that expiry aborts rather than kills:
// killing customer work to finish a deploy trades their data for our
// convenience.
const DrainTimeout = 5 * time.Minute

// ErrFenced means the generation the agent holds is not the one the control
// plane is on. The agent applies nothing and re-reports, so a stale agent cannot
// roll a runtime backwards.
var ErrFenced = errors.New("agent: generation is stale")

// ErrDrainTimeout means work did not finish inside the deadline. The upgrade is
// abandoned and the runtime is returned to serving on the old release.
var ErrDrainTimeout = errors.New("agent: drain deadline expired")

// Apply runs the protocol's apply sequence for one desired state.
//
// It is written as a straight line because the ORDER is the security property,
// and a sequence that is easy to reorder is a sequence that will be. Every exit
// path either leaves the runtime serving on the release it started with, or
// leaves it deliberately gated with a reason an operator can act on -- never
// gated by accident, and never in a state where in-flight work was discarded to
// make the deploy succeed.
func Apply(ctx context.Context, rt Runtime, want *Desired, haveGeneration int64, drain Drain, serving Observed) (Reported, error) {
	// A generation the agent has already moved past must not be applied: this is
	// the fence that stops a stale agent undoing a newer deployment.
	if want.Generation < haveGeneration {
		return Reported{Generation: want.Generation, Outcome: OutcomeRefusedFenced,
			Reason: fmt.Sprintf("desired generation %d is behind observed %d", want.Generation, haveGeneration)}, ErrFenced
	}

	// 1. verify + 2. prepare, before anything touches the running release.
	if want.Release.Digest == "" {
		return Reported{Generation: want.Generation, Outcome: OutcomeFailed, Reason: "desired state names no release"}, nil
	}

	// ALREADY SERVING IT IS THE NORMAL CASE, NOT A REASON TO WORK.
	//
	// A control plane publishes DESIRED STATE, not events: it keeps returning the
	// same generation until a deploy replaces it. So after a successful apply,
	// every subsequent cycle asks for the release the runtime is already serving.
	// Without this check the agent re-downloaded the release, gated the tenant,
	// re-activated identical content and leaked a staging directory -- measured on
	// a pooled tenant as a deploy re-applied every few seconds, each one leaving an
	// .install-* directory behind.
	//
	// Content-addressing is what makes the test exact rather than a guess: the same
	// digest IS the same release, whatever generation asked for it. This also
	// covers an agent restart, which starts with no memory of what it applied.
	if sameRelease(serving.ReleaseDigest, want.Release.Digest) {
		return Reported{
			Generation: want.Generation,
			Outcome:    OutcomeApplied,
			Observed:   map[string]string{"release_digest": serving.ReleaseDigest, "maintenance": serving.Maintenance},
		}, nil
	}
	if err := rt.Fetch(ctx, want.Release); err != nil {
		// A digest mismatch stops here. Content-addressing is what makes a
		// compromised control plane unable to substitute release content, so
		// fetching without verifying would give that up.
		return Reported{Generation: want.Generation, Outcome: OutcomeFailed,
			Reason: "fetch: " + err.Error()}, nil
	}

	// 3. drain. Entering maintenance is what makes the swap safe, and the
	// deadline is where we refuse to sacrifice running work.
	entered := false
	if err := rt.EnterMaintenance(ctx, reasonFor(want), drain.Timeout); err != nil {
		if errors.Is(err, ErrDrainTimeout) {
			// Abort: return to serving on the OLD release. Not left gated,
			// because gating on the agent's own authority would take the tenant
			// offline for a deploy that did not happen.
			_ = rt.ExitMaintenance(ctx)
			return Reported{Generation: want.Generation, Outcome: OutcomeFailed,
				Reason: "drain deadline expired; upgrade abandoned, runtime returned to serving"}, ErrDrainTimeout
		}
		return Reported{Generation: want.Generation, Outcome: OutcomeFailed, Reason: "drain: " + err.Error()}, nil
	}
	entered = true

	// From here the runtime is gated, so every subsequent exit must either
	// activate or restore -- there is no path that simply returns.
	defer func() {
		if entered {
			// Best effort: a failure to exit maintenance is reported by the
			// caller's outcome, and the gate is durable so an operator can see
			// and clear it. What must not happen is an unreported gate.
			_ = rt.ExitMaintenance(ctx)
		}
	}()

	// 4. swap.
	if err := rt.Promote(ctx, want.Release.Digest); err != nil {
		return Reported{Generation: want.Generation, Outcome: OutcomeFailed, Reason: "promote: " + err.Error()}, nil
	}

	// 5. validate, never by executing a customer integration: that would have
	// external side effects a rollback cannot undo.
	if err := rt.Validate(ctx, want.Release.Digest); err != nil {
		return Reported{Generation: want.Generation, Outcome: OutcomeFailed, Reason: "validate: " + err.Error()}, nil
	}

	// 6. activate, explicitly and before observing. The deferred exit is a
	// safety net for the failure paths; the success path must leave maintenance
	// itself, or the state reported below describes a runtime that is still
	// gated and Cloud would record the wrong thing.
	if err := rt.ExitMaintenance(ctx); err != nil {
		return Reported{Generation: want.Generation, Outcome: OutcomeFailed,
			Reason: "activate: " + err.Error()}, nil
	}
	entered = false

	// 7. report the state as it now is, after activation.
	observed, _ := rt.Observe(ctx)
	return Reported{
		Generation: want.Generation,
		Outcome:    OutcomeApplied,
		Observed:   map[string]string{"release_digest": observed.ReleaseDigest, "maintenance": observed.Maintenance},
	}, nil
}

// Drain carries the deadline for one apply. A struct rather than a bare duration
// so a future policy (per-tenant, or overridden by the control plane) has a place
// to live without changing every call site.
type Drain struct {
	Timeout time.Duration
}

func reasonFor(want *Desired) string {
	if want.Maintenance != nil && want.Maintenance.Reason != "" {
		return want.Maintenance.Reason
	}
	return fmt.Sprintf("deploy generation %d", want.Generation)
}

// sameRelease reports whether two digest spellings name the same content. Cloud
// stores `sha256:<hex>` while the runtime's own store names directories by bare
// hex, so comparing spellings instead of identities would call an already-applied
// release unapplied -- and re-install it on every cycle.
func sameRelease(a, b string) bool {
	na, err := normalizeDigest(a)
	if err != nil {
		return false
	}
	nb, err := normalizeDigest(b)
	if err != nil {
		return false
	}
	return na == nb
}
