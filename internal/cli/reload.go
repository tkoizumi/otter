package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/tkoizumi/otter/internal/api"
)

// cmdReload asks the running daemon to re-read its integrations directory.
//
// This is the alternative to `otter stop && otter start` after adding or
// editing an integration. Nothing is stopped: the daemon keeps serving, runs
// that are already executing keep running, and a cron schedule whose
// expression did not change keeps its next fire time. Only the set of
// integrations the daemon knows about is replaced.
func (a *App) cmdReload(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(a.Stderr, "Usage: otter reload")
		fmt.Fprintln(a.Stderr)
		fmt.Fprintln(a.Stderr, "Re-reads the integrations directory against the running daemon.")
		fmt.Fprintln(a.Stderr, "The daemon is not restarted: executing runs and unchanged cron")
		fmt.Fprintln(a.Stderr, "schedules are left alone. New integrations still need `otter release`")
		fmt.Fprintln(a.Stderr, "before they can run, because a run executes the active release.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter reload")
		return 2
	}

	result, err := g.client().Reload(ctx)
	if err != nil {
		return a.fail(err)
	}

	if g.jsonOut {
		return a.printJSON(result)
	}

	a.printReloadResult(result)
	return 0
}

// printReloadResult names every integration the reload added, removed or
// changed, because those are the answers to "did my edit land?" and "what did
// that just do?".
func (a *App) printReloadResult(result *api.ReloadResult) {
	if len(result.Added)+len(result.Removed)+len(result.Changed) == 0 {
		fmt.Fprintf(a.Stdout, "no changes   %d integration(s)\n", result.Total)
	} else {
		for _, id := range result.Added {
			fmt.Fprintf(a.Stdout, "added        %s\n", id)
		}
		for _, id := range result.Removed {
			fmt.Fprintf(a.Stdout, "removed      %s\n", id)
		}
		for _, id := range result.Changed {
			fmt.Fprintf(a.Stdout, "changed      %s\n", id)
		}
	}

	for _, id := range result.Invalid {
		fmt.Fprintf(a.Stdout, "invalid      %s\n", id)
	}
	if result.RunsCancelled > 0 {
		fmt.Fprintf(a.Stdout, "cancelled    %d queued run(s) of removed integrations\n", result.RunsCancelled)
	}

	if len(result.Added) > 0 {
		fmt.Fprintf(a.Stdout, "\nnext: otter release <integration>   (a run executes the active release,\n")
		fmt.Fprintf(a.Stdout, "      so a newly discovered integration must be released before it can run)\n")
	}
}
