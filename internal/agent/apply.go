package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The types of the pull protocol. They mirror the wire shape in
// hosting/docs/agent-protocol.md; keeping them here rather than generating from
// the spec means a protocol change is a visible edit to a Go file and a test,
// not a silent regeneration.

// CapabilityDesiredSnapshot is what an agent declares when it can converge a
// per-job SNAPSHOT rather than one release.
//
// It is the whole version-skew mechanism. Cloud and agents deploy
// independently, so Cloud serves the older single-release shape to an agent that
// does not declare this, and an agent that receives no snapshot (because Cloud
// is older) reconciles the single release and removes nothing. Neither direction
// can make an old participant act on a shape it does not know.
const CapabilityDesiredSnapshot = "desired.snapshot.v1"

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
	// Capabilities names the protocol shapes this agent understands. Cloud
	// answers in the shape the agent declares, so a fleet can be rebuilt at its
	// own pace.
	Capabilities []string `json:"capabilities,omitempty"`
}

// DesiredJob is one job's entry in the desired snapshot.
type DesiredJob struct {
	// Job is the runtime's durable job id. It is what the release store is keyed
	// by, so it is what a removal names.
	Job    string `json:"job"`
	Digest string `json:"digest"`
	URL    string `json:"url"`
	Size   int64  `json:"size,omitempty"`
}

func (j DesiredJob) release() Release {
	return Release{Digest: j.Digest, URL: j.URL, Size: j.Size}
}

// Desired is what the control plane answers with: which release and
// configuration this runtime should be running, and its generation.
type Desired struct {
	// RuntimeID is echoed by the control plane and MUST be sent back on every
	// request: the endpoints refuse a request that does not name a runtime,
	// because a credential's binding cannot be checked without one.
	RuntimeID  string `json:"runtime_id"`
	Generation int64  `json:"generation"`
	// Snapshot marks the per-job answer. It is an explicit flag rather than
	// "jobs is non-empty" because a snapshot with NO jobs is meaningful -- it is
	// the state every managed job has been deleted -- and reading it as "no
	// answer" would leave deleted jobs serving forever.
	Snapshot bool         `json:"snapshot,omitempty"`
	Jobs     []DesiredJob `json:"jobs,omitempty"`
	// Release is the single-release shape, still served to an agent that has not
	// declared the snapshot capability, and still understood here so a new agent
	// works against an older Cloud.
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
	// Releases is the full active set after the cycle. It is the EVIDENCE a
	// per-job operation closes on: a deploy on its target job's presence, a
	// removal on its target job's absence.
	//
	// It is deliberately NOT omitempty, and nil is kept DISTINCT from empty:
	//
	//   - a NON-NIL, EMPTY slice marshals as `[]`: "I am reporting, and the
	//     runtime serves nothing". This is the only shape that can confirm the
	//     LAST removal on a runtime, because a removal is confirmed by the job's
	//     absence -- and a report that never mentioned the set cannot be read as
	//     evidence that the job is gone;
	//   - a nil slice marshals as `null`: "this report carries no set at all",
	//     which is what an agent that only speaks the single-release shape sends
	//     (its evidence is the digest above). Cloud reads that as no evidence.
	//
	// `omitempty` collapsed both of those into an absent field, so the last
	// removal on a runtime could never be confirmed: its operation stayed
	// pending forever and refused every later deploy with `operation_open`. The
	// field must be present-with-`[]`, not omitted and not `null`.
	Releases []ObservedRelease `json:"releases"`
	// ManagedJobs is the released-only set the runtime manages. Kept present for
	// the same reason as Releases: `[]` is "reported, and the runtime manages
	// none", a list is "reported, here they are", and nil (`null`) is "not
	// reported". Emptiness is meaningful here -- it is what makes a stale
	// managed-set record clearable -- so it must not be omitted.
	ManagedJobs []string `json:"managed_jobs"`
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
//
// `omitempty` on the two collections below is DELIBERATE, and it is the one
// place on this path where it is correct: `Releases`/`ManagedJobs` here travel
// on the DESIRED request, and that route reads them with `?? []` -- an absent
// set and an empty one are answered identically. The question that must not be
// conflated -- "did the runtime actually answer, or did the read fail?" -- is
// carried by ManagedReconciliation, which is false (and so omitted) on a failed
// read and gates the only consumer of the set (`declareSnapshot`). The REPORT,
// by contrast, closes operations on the set itself, so its `Reported.Releases`
// must NOT be omitted; see the comment there.
type Observed struct {
	ReleaseDigest string `json:"release_digest,omitempty"`
	// Releases is every release the runtime serves, per job. A SET because a
	// runtime manages several jobs, and a single digest cannot say which is
	// which -- guessing would be worse than saying nothing, because Cloud treats
	// a reported release as evidence.
	Releases []ObservedRelease `json:"releases,omitempty"`
	// ManagedJobs is the released-only set: the jobs this runtime holds with no
	// source in the jobs directory, which is what a control plane owns here.
	ManagedJobs []string `json:"managed_jobs,omitempty"`
	// ManagedReconciliation is true when the runtime can identify that set. An
	// agent will not remove a managed job from a runtime that cannot, because it
	// could not tell a released-only job from a workspace job.
	ManagedReconciliation bool   `json:"managed_reconciliation,omitempty"`
	RuntimeVersion        string `json:"runtime_version,omitempty"`
	Maintenance           string `json:"maintenance,omitempty"`
	Health                string `json:"health,omitempty"`
}

// ObservedRelease is one release a runtime serves, addressed by job.
type ObservedRelease struct {
	Job    string `json:"job"`
	Digest string `json:"digest"`
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
	// DeleteJob removes a Cloud-managed job through the runtime's own delete
	// path, which is where the release store is actually cleaned. It is how a
	// desired-state removal is EXECUTED; the runtime refuses a job that has a
	// source in the jobs directory, so a workspace job can never be removed by a
	// control plane that did not name it.
	DeleteJob(ctx context.Context, job string) (string, error)
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
// It is the single entry point for both shapes, because the ORDER is the
// security property and a shape that took a different path would be a shape that
// reorders it. The generation fence is applied once here, then the answer is
// reconciled either as a per-job snapshot or as the single release an older
// control plane still sends.
func Apply(ctx context.Context, rt Runtime, want *Desired, haveGeneration int64, drain Drain, serving Observed) (Reported, error) {
	// A generation the agent has already moved past must not be applied: this is
	// the fence that stops a stale agent undoing a newer deployment.
	if want.Generation < haveGeneration {
		return Reported{Generation: want.Generation, Outcome: OutcomeRefusedFenced,
			Reason: fmt.Sprintf("desired generation %d is behind observed %d", want.Generation, haveGeneration)}, ErrFenced
	}
	if want.Snapshot {
		return applySnapshot(ctx, rt, want, drain, serving)
	}
	return applySingle(ctx, rt, want, drain, serving)
}

// applySingle is the original single-release sequence, kept for the shape an
// agent that has not declared the snapshot capability still receives.
func applySingle(ctx context.Context, rt Runtime, want *Desired, drain Drain, serving Observed) (Reported, error) {
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

// applySnapshot converges a runtime onto a full per-job desired state.
//
// THE DIFFERENCE FROM applySingle IS NOT "more than one job" -- it is that the
// desired state is the COMPLETE set, so a job the runtime serves and the snapshot
// does not name has been deleted and must go. That is what makes a delete stick:
// the release directory is what discovery derives a job from, so leaving it
// behind resurrects the job on the next scan however loudly the control plane
// stopped desiring it.
//
// Only MANAGED jobs are considered for removal, and the runtime decides which
// those are. A job whose source lives in the jobs directory is not the control
// plane's to remove, and the runtime refuses it -- so an agent never has to guess
// and can never delete work Cloud never owned.
func applySnapshot(ctx context.Context, rt Runtime, want *Desired, drain Drain, serving Observed) (Reported, error) {
	desired := make(map[string]DesiredJob, len(want.Jobs))
	for _, job := range want.Jobs {
		if job.Job == "" {
			return failedReport(want, serving, "desired state names a job with no id"), nil
		}
		if job.Digest == "" {
			return failedReport(want, serving, "desired state names no release for job "+job.Job), nil
		}
		desired[job.Job] = job
	}

	current := make(map[string]string, len(serving.Releases))
	for _, rel := range serving.Releases {
		current[rel.Job] = rel.Digest
	}

	// Jobs whose release has to change. Deterministic order so a log lines up
	// with the last one and a test can assert on it.
	changes := make([]DesiredJob, 0, len(want.Jobs))
	for _, job := range want.Jobs {
		if !sameRelease(current[job.Job], job.Digest) {
			changes = append(changes, job)
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Job < changes[j].Job })

	// Managed jobs the snapshot no longer names. Refusing to guess at the set is
	// the whole reason ManagedReconciliation travels: a runtime that cannot say
	// which jobs it manages is one where a removal would be a guess.
	removals := []string{}
	if serving.ManagedReconciliation {
		for _, job := range serving.ManagedJobs {
			if _, keep := desired[job]; !keep {
				removals = append(removals, job)
			}
		}
		sort.Strings(removals)
	}

	// Already converged is the NORMAL case: the control plane publishes desired
	// state, so every poll after a successful apply asks for what is already
	// running. Returning here is what stops a redeploy loop.
	if len(changes) == 0 && len(removals) == 0 {
		return convergedReport(want, serving), nil
	}

	// Everything needed is fetched BEFORE the runtime is gated, exactly as the
	// single-release path does: a failed download must not take a serving
	// runtime offline for a deploy that then does not happen.
	for _, job := range changes {
		if err := rt.Fetch(ctx, job.release()); err != nil {
			return failedReport(want, serving, "fetch "+job.Job+": "+err.Error()), nil
		}
	}

	// Removals happen WITHOUT gating. Deleting one job does not touch another
	// job's work, and the runtime's own in-flight refusal is the guard -- gating
	// the whole runtime to remove an idle job would stop every other tenant job
	// for no safety at all.
	problems := []string{}
	for _, job := range removals {
		note, err := rt.DeleteJob(ctx, job)
		if err != nil {
			problems = append(problems, fmt.Sprintf("remove %s: %v", job, err))
			continue
		}
		_ = note
	}

	if len(changes) > 0 {
		entered := false
		if err := rt.EnterMaintenance(ctx, reasonFor(want), drain.Timeout); err != nil {
			if errors.Is(err, ErrDrainTimeout) {
				// Abort: return to serving on the OLD release. Not left gated,
				// because gating on the agent's own authority would take the
				// tenant offline for a deploy that did not happen.
				_ = rt.ExitMaintenance(ctx)
				return Reported{Generation: want.Generation, Outcome: OutcomeFailed,
					Reason: "drain deadline expired; upgrade abandoned, runtime returned to serving"}, ErrDrainTimeout
			}
			return failedReport(want, serving, "drain: "+err.Error()), nil
		}
		entered = true
		defer func() {
			if entered {
				_ = rt.ExitMaintenance(ctx)
			}
		}()

		for _, job := range changes {
			if err := rt.Promote(ctx, job.Digest); err != nil {
				problems = append(problems, fmt.Sprintf("promote %s: %v", job.Job, err))
			}
		}
		for _, job := range changes {
			if err := rt.Validate(ctx, job.Digest); err != nil {
				problems = append(problems, fmt.Sprintf("validate %s: %v", job.Job, err))
			}
		}
		if err := rt.ExitMaintenance(ctx); err != nil {
			problems = append(problems, "activate: "+err.Error())
		}
		entered = false
	}

	if len(problems) > 0 {
		// One outcome for the whole snapshot, because it IS one desired state.
		// Reporting applied while a removal was refused would close an operation
		// Cloud asked for and leave the release store holding a deleted job.
		return failedReport(want, serving, strings.Join(problems, "; ")), nil
	}

	// Report the state as it now is, after activation.
	observed, _ := rt.Observe(ctx)
	return convergedReport(want, observed), nil
}

// convergedReport is the applied outcome, carrying the full observed set so a
// control plane can confirm per job rather than on a claim.
func convergedReport(want *Desired, observed Observed) Reported {
	return Reported{
		Generation:  want.Generation,
		Outcome:     OutcomeApplied,
		Observed:    map[string]string{"release_digest": observed.ReleaseDigest, "maintenance": observed.Maintenance},
		Releases:    observed.Releases,
		ManagedJobs: observed.ManagedJobs,
	}
}

// failedReport is a failure that still carries the release set the agent last
// observed. The set is what a control plane confirms a removal against, and an
// empty one would read as "the runtime serves nothing" -- which could confirm a
// deletion that did not happen. The pre-apply observation is the safe value: it
// still contains every job that was serving when the cycle started.
func failedReport(want *Desired, serving Observed, reason string) Reported {
	return Reported{
		Generation:  want.Generation,
		Outcome:     OutcomeFailed,
		Reason:      reason,
		Releases:    serving.Releases,
		ManagedJobs: serving.ManagedJobs,
	}
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
