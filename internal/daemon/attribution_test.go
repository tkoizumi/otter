package daemon

import (
	"errors"
	"testing"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/executor"
	"github.com/tkoizumi/otter/internal/runs"
)

// A run's status says it ended badly. It does not say whether the RUNTIME ended
// it, and the gate's Reliability row is written in exactly those terms: 30
// consecutive days "without a runtime-caused incident requiring manual repair",
// and "track runtime failures separately from job bugs and upstream failures".
//
// classifyOutcome already decided this in order to pick the retry policy, and then
// discarded it. These are the cases the recorded answer has to get right, and the
// wrong direction is expensive in both: calling a job's bug a runtime incident
// restarts the reliability observation, and calling a runtime failure the job's
// hides the one thing the window is measuring.
func TestClassifyOutcomeAttributesTheEnding(t *testing.T) {
	code := func(n int) *int { return &n }

	cases := []struct {
		name   string
		res    *executor.Result
		reason cancelReason
		want   string
	}{
		{
			name: "a job that exited non-zero is the job's",
			res:  &executor.Result{ExitCode: code(1)},
			want: runs.AttributionJob,
		},
		{
			name: "a job killed by a signal is the job's",
			res:  &executor.Result{Signal: "SIGKILL"},
			want: runs.AttributionJob,
		},
		{
			name: "a job that overran its timeout is the job's",
			res:  &executor.Result{TimedOut: true},
			want: runs.AttributionJob,
		},
		{
			// The runtime could not put the job on the machine: a missing
			// interpreter, an unprepared environment, a failed preflight.
			name: "a job the runtime could not start is the runtime's",
			res:  &executor.Result{StartError: errors.New("no interpreter")},
			want: runs.AttributionRuntime,
		},
		{
			// The job ran and the runtime failed to observe how it ended.
			name: "a run the runtime failed to reap is the runtime's",
			res:  &executor.Result{WaitError: errors.New("wait: no child processes")},
			want: runs.AttributionRuntime,
		},
		{
			// No exit code, no signal, no wait error. Something in the runtime lost
			// the outcome, and a lost outcome is not the job's fault.
			name: "a run with no ending recorded is the runtime's",
			res:  &executor.Result{},
			want: runs.AttributionRuntime,
		},
		{
			name:   "the daemon shutting down mid-run is the runtime's",
			res:    &executor.Result{ExitCode: code(1)},
			reason: reasonShutdown,
			want:   runs.AttributionRuntime,
		},
		{
			name:   "an operator cancelling is the operator's",
			res:    &executor.Result{Cancelled: true},
			reason: reasonUser,
			want:   runs.AttributionOperator,
		},
		{
			// Not a user cancellation, or the case above would have taken it: a
			// maintenance drain or an operator command.
			name: "a drain cancelling is the operator's",
			res:  &executor.Result{Cancelled: true},
			want: runs.AttributionOperator,
		},
		{
			// A success is nobody's failure, and saying so with an empty field is
			// better than inventing a fourth value for it.
			name: "a successful run is attributed to nobody",
			res:  &executor.Result{ExitCode: code(0)},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, got := classifyOutcome(tc.res, tc.reason, &config.Manifest{})
			if got != tc.want {
				t.Errorf("attribution = %q, want %q", got, tc.want)
			}
		})
	}
}
