package deploy

import (
	"fmt"
	"strings"
)

// The stages of a converge that change the host, in the order Run performs
// them. They are named once, here, because the report of a failed deploy is
// only honest if "not reached" is computed from the same plan the run walks.
const (
	stagePrepare         = "prepare"
	stageSyncSources     = "sync-sources"
	stageSyncBinaries    = "sync-binaries"
	stageSyncUV          = "sync-uv"
	stageOwnership       = "ownership"
	stageWorkspaceRecord = "workspace-record"
	stageDaemonEnv       = "daemon-env"
	stageSharedEnv       = "shared-env"
	stageRelease         = "release"
	stageInstallRestart  = "install-restart"
	stageHealth          = "health"

	// stageLocalState is deliberately not a host mutation and is absent from
	// hostStages: it is this checkout's state file, written after the host has
	// already converged. It exists so that failure, too, can name what it left
	// behind -- a fully deployed host and a stale local record.
	stageLocalState = "local-state"
)

// hostStages is the ordered plan of host mutations for one run, built once
// from the config so that "not reached" means what it says. Steps this config
// will skip are absent rather than reported as not reached: uv is only pushed
// when some job needs preparation, and nothing is released on a host with no
// jobs.
func hostStages(cfg Config, withUV bool) []string {
	stages := []string{stagePrepare, stageSyncSources, stageSyncBinaries}
	if withUV {
		stages = append(stages, stageSyncUV)
	}
	stages = append(stages,
		stageOwnership,
		stageWorkspaceRecord,
		stageDaemonEnv,
		stageSharedEnv,
	)
	if len(cfg.Jobs) > 0 {
		stages = append(stages, stageRelease)
	}
	return append(stages, stageInstallRestart, stageHealth)
}

// stageStatus is what happened to one stage. A stage only counts as reached
// once it has started, which is what keeps a skipped stage distinguishable
// from a failed one.
type stageStatus int

const (
	stagePending stageStatus = iota
	stageStarted
	stageDone
	stageFailed
)

// progress records how far a converge got: for every host mutation, whether it
// started, finished, or failed. It is the only memory of the run that survives
// an error return, and it is what makes a partial deploy able to describe
// itself instead of returning a bare cause.
type progress struct {
	order  []string
	status map[string]stageStatus
}

// newProgress starts a run against the ordered plan from hostStages.
func newProgress(stages []string) *progress {
	return &progress{order: stages, status: map[string]stageStatus{}}
}

// begin records that a stage is about to change the host.
func (p *progress) begin(stage string) { p.mark(stage, stageStarted) }

// done records that a stage finished. Every stage is marked before it
// succeeds, so a failure can never be mistaken for a stage that never ran.
func (p *progress) done(stage string) { p.mark(stage, stageDone) }

func (p *progress) mark(stage string, status stageStatus) {
	if p.status == nil {
		p.status = map[string]stageStatus{}
	}
	// Never hide a mutation because the plan and the code drifted: an
	// unplanned stage is recorded and reported rather than dropped.
	if !p.planned(stage) {
		p.order = append(p.order, stage)
	}
	p.status[stage] = status
}

func (p *progress) planned(stage string) bool {
	for _, planned := range p.order {
		if planned == stage {
			return true
		}
	}
	return false
}

// completed lists the stages that finished, in the order they ran.
func (p *progress) completed() []string {
	var out []string
	for _, stage := range p.order {
		if p.status[stage] == stageDone {
			out = append(out, stage)
		}
	}
	return out
}

// notReached lists the planned stages that never started. A stage that has
// only started is the one that failed, and it is reported as Failed, not here.
func (p *progress) notReached() []string {
	var out []string
	for _, stage := range p.order {
		if p.status[stage] == stagePending {
			out = append(out, stage)
		}
	}
	return out
}

// fail closes a stage and turns its error into a report when the deploy had
// already changed the host. Before the first mutation the bare error is the
// whole truth: nothing landed, so there is nothing to describe.
func (p *progress) fail(stage string, err error) error {
	p.mark(stage, stageFailed)
	completed := p.completed()
	if len(completed) == 0 {
		return err
	}
	return &PartialDeployError{
		Stage:       stage,
		Cause:       err,
		Completed:   completed,
		NotReached:  p.notReached(),
		Consequence: consequenceFor(stage),
	}
}

// converged builds the report for a failure that happened after every host
// stage succeeded: the deploy is live, only this checkout's record of it is
// missing.
func (p *progress) converged(err error) error {
	return &PartialDeployError{
		Stage:       stageLocalState,
		Cause:       err,
		Completed:   p.completed(),
		NotReached:  p.notReached(),
		Consequence: consequenceFor(stageLocalState),
	}
}

// consequenceFor is the one line that says what the host is doing now. It is
// chosen from the stage that failed, not from how the failure felt: the
// restart is the point after which the previous release can no longer be
// assumed to be serving.
func consequenceFor(stage string) string {
	switch stage {
	case stageInstallRestart:
		return "the new unit was written; the previous release may no longer be serving"
	case stageHealth:
		return "the new release is installed but did not become healthy"
	case stageLocalState:
		return "the host is deployed and serving the new release; the local state file was not saved"
	default:
		return "the previous release is still serving; nothing was restarted"
	}
}

// PartialDeployError is returned when a deploy failed after it had already
// changed the host. Its message is the report the operator needs: which
// mutations landed, which step failed, what was never attempted, and what the
// host is serving now.
//
// It wraps the cause, so errors.Is and errors.As still reach the failure that
// stopped the run -- including any hint the deployer added to it.
type PartialDeployError struct {
	// Stage is the step that failed: one of the ordered host stages, or
	// "local-state" when every host stage succeeded and only the local state
	// file was not written.
	Stage string
	// Cause is what that step reported.
	Cause error
	// Completed lists the stages that finished, in the order they ran.
	Completed []string
	// NotReached lists the planned stages that were never started.
	NotReached []string
	// Consequence describes, in one line, the state the host is left in.
	Consequence string
}

// Error renders the report. The CLI prints it verbatim after "otter: ", so the
// first line has to stand on its own.
func (e *PartialDeployError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "deploy stopped at %s: %v\n", e.Stage, e.Cause)
	fmt.Fprintf(&b, "Mutations completed: %d\n", len(e.Completed))
	for _, stage := range e.Completed {
		fmt.Fprintf(&b, "  %s\n", stage)
	}
	fmt.Fprintf(&b, "Failed: %s\n", e.Stage)
	b.WriteString("Not reached:\n")
	if len(e.NotReached) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, stage := range e.NotReached {
		fmt.Fprintf(&b, "  %s\n", stage)
	}
	fmt.Fprintf(&b, "Consequence: %s", e.Consequence)
	return b.String()
}

// Unwrap exposes the cause so errors.Is matches the injected failure through
// the report, exactly as it would without it.
func (e *PartialDeployError) Unwrap() error { return e.Cause }
