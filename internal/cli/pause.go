package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/tkoizumi/otter/internal/api"
)

// cmdPauseResume suspends or re-arms a job's autonomous triggers.
//
// Pausing stops cron and webhook admission. It does not retire the identity,
// touch the state, cancel a queued run or remove a release, and it does not
// block a manual run: "stop firing on its own" and "disable this job"
// are different requests, and only the first one is what an operator pausing a
// misbehaving job at 3am means.
//
// With no argument the job in the working directory is used, so inside
// a job directory `otter pause` and `otter pause .` are the same
// command, exactly like `otter run`.
func (a *App) cmdPauseResume(ctx context.Context, g globals, args []string, paused bool) int {
	verb := "resume"
	if paused {
		verb = "pause"
	}

	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		if paused {
			fmt.Fprint(a.Stderr, "Usage: otter pause [job]\n\n"+
				"Suspends cron and webhook triggers for one job. State, history,\n"+
				"tokens and releases are untouched, and otter run still works.\n")
			return
		}
		fmt.Fprint(a.Stderr, "Usage: otter resume [job]\n\n"+
			"Re-arms the triggers a pause suspended.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(a.Stderr, "otter: usage: otter %s [job]\n", verb)
		return 2
	}

	ref := "."
	if fs.NArg() == 1 {
		ref = fs.Arg(0)
	}
	id, err := resolveJobRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	client := g.client()
	var view *api.PauseView
	if paused {
		view, err = client.PauseJob(ctx, id)
	} else {
		view, err = client.ResumeJob(ctx, id)
	}
	if err != nil {
		return a.fail(err)
	}

	if g.jsonOut {
		return a.printJSON(view)
	}
	a.printPauseResult(view)
	return 0
}

// printPauseResult states the transition as well as the state. A deploy script
// calls pause unconditionally, so it needs to tell "this call paused it" from
// "it was already paused", which is the difference between changed true and
// false.
func (a *App) printPauseResult(v *api.PauseView) {
	switch {
	case v.Paused && v.Changed:
		fmt.Fprintf(a.Stdout, "paused: %s\n", v.Name)
	case v.Paused:
		fmt.Fprintf(a.Stdout, "already paused: %s\n", v.Name)
	case v.Changed:
		fmt.Fprintf(a.Stdout, "resumed: %s\n", v.Name)
	default:
		fmt.Fprintf(a.Stdout, "already enabled: %s (was not paused)\n", v.Name)
	}

	if v.Paused {
		fmt.Fprintf(a.Stdout, "cron and webhook will not fire; otter run still runs it on demand\n")
		return
	}
	fmt.Fprintf(a.Stdout, "cron and webhook fire again\n")
}

// pausedSummary renders a paused trigger state on one line: since when. It is
// what turns a missing next run in `otter inspect` into a deliberate state.
func pausedSummary(t api.TriggerView) string {
	if t.PausedAt == nil {
		return "yes"
	}
	return "since " + t.PausedAt.Local().Format("2006-01-02 15:04:05")
}
