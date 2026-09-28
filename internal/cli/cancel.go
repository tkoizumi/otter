package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/runs"
)

// cmdCancel stops a queued or running run.
//
// The API and the client have had the operation for a while; what was missing
// was the verb. It matters most right after `otter pause`: a pause stops new
// admission and deliberately leaves work in flight alone, so the immediate next
// question is how to end the run that is already going.
//
// A queued or retrying run is removed from the queue and finished at once. A
// running one is signalled and the worker records the final status, so this
// returns as soon as the cancellation is accepted, not when the child has
// exited.
func (a *App) cmdCancel(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(a.Stderr, "Usage: otter cancel <run-id>")
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "Stops a queued or running run. A cancelled run is never retried.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter cancel <run-id>")
		return 2
	}
	runID := fs.Arg(0)

	if err := g.client().CancelRun(ctx, runID); err != nil {
		return a.fail(err)
	}

	if g.jsonOut {
		return a.printJSON(api.CancelRunResponse{RunID: runID, Status: string(runs.StatusCancelled)})
	}
	fmt.Fprintf(a.Stdout, "cancelled    %s\n", runID)
	return 0
}
