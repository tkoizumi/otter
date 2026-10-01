package cli

import (
	"context"
	"flag"
	"fmt"
	"time"
)

// cmdSchedule reads or replaces when a job fires on its own.
//
// The schedule is runtime state rather than manifest state. A cadence changes
// far more often than a job's code -- load, cost or a vendor's rate limit moves
// it -- and a value that both a file and an API can write is a value that
// eventually disagrees with itself. A manifest's trigger.cron is imported once,
// for jobs that predate this command, and ignored from then on.
//
// With no subcommand the job in the working directory is shown, so inside a job
// directory `otter schedule` and `otter schedule show .` are the same command,
// exactly like `otter run`.
func (a *App) cmdSchedule(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		fmt.Fprint(a.Stderr, "Usage: otter schedule [show|set|clear] [job] [cron]\n\n"+
			"Reads or replaces when a job fires on its own.\n\n"+
			"  otter schedule                    show this job's schedule\n"+
			"  otter schedule show <job>         show one job's schedule\n"+
			"  otter schedule set '*/15 * * * *'  fire every fifteen minutes\n"+
			"  otter schedule set <job> '0 3 * * *'\n"+
			"  otter schedule clear [job]        never fire on its own\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rest := fs.Args()
	verb := "show"
	if len(rest) > 0 {
		switch rest[0] {
		case "show", "set", "clear":
			verb = rest[0]
			rest = rest[1:]
		}
	}

	var ref, cron string
	switch verb {
	case "show", "clear":
		switch len(rest) {
		case 0:
			ref = "."
		case 1:
			ref = rest[0]
		default:
			fmt.Fprintf(a.Stderr, "otter: usage: otter schedule %s [job]\n", verb)
			return 2
		}
	case "set":
		switch len(rest) {
		case 0:
			fmt.Fprint(a.Stderr, "otter: a cron expression is required\n")
			return 2
		case 1:
			ref, cron = ".", rest[0]
		case 2:
			ref, cron = rest[0], rest[1]
		default:
			fmt.Fprint(a.Stderr, "otter: usage: otter schedule set [job] <cron>\n")
			return 2
		}
	}

	id, err := resolveJobRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	client := g.client()

	if verb == "show" {
		view, err := client.GetJob(ctx, id)
		if err != nil {
			return a.fail(err)
		}
		if g.jsonOut {
			return a.printJSON(view)
		}
		a.printJobSchedule(view.Triggers.Cron, view.NextRunAt)
		return 0
	}

	view, err := client.SetSchedule(ctx, id, cron)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(view)
	}
	a.printJobSchedule(view.Cron, view.NextRunAt)
	return 0
}

// printJobSchedule states the cadence and, when one exists, the next time it will
// actually fire. An empty cron is reported as a deliberate state rather than as
// missing configuration, because clearing is a first-class request.
func (a *App) printJobSchedule(cron string, next *time.Time) {
	if cron == "" {
		fmt.Fprintln(a.Stdout, "no schedule: this job never fires on its own")
		fmt.Fprintln(a.Stdout, "otter run still runs it on demand")
		return
	}
	fmt.Fprintf(a.Stdout, "%s\n", cron)
	if next == nil {
		// A stored cadence with no next fire time means the job is paused.
		fmt.Fprintln(a.Stdout, "next run: none while the job is paused")
		return
	}
	fmt.Fprintf(a.Stdout, "next run: %s\n", next.Local().Format("2006-01-02 15:04:05"))
}
