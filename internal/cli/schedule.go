package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"

	"github.com/tkoizumi/otter/internal/api"
)

// cmdSchedule manages when a job fires on its own.
//
// A schedule is runtime state rather than manifest state: a cadence changes far
// more often than a job's code, and a value that both a file and an API can
// write is a value that eventually disagrees with itself. A manifest's
// trigger.cron is reconciled as a manifest-owned row, which the API refuses to
// change; everything under `add` is API-owned and no reload touches it.
//
// With no subcommand the job in the working directory is listed, so inside a job
// directory `otter schedule` and `otter schedule list .` are the same command,
// exactly like `otter run`.
func (a *App) cmdSchedule(ctx context.Context, g globals, args []string) int {
	if len(args) == 0 {
		return a.scheduleList(ctx, g, nil)
	}

	verb, rest := args[0], args[1:]
	switch verb {
	case "list", "ls":
		return a.scheduleList(ctx, g, rest)
	case "show":
		return a.scheduleShow(ctx, g, rest)
	case "add", "create":
		return a.scheduleAdd(ctx, g, rest)
	case "update", "edit":
		return a.scheduleUpdate(ctx, g, rest)
	case "remove", "rm", "delete":
		return a.scheduleRemove(ctx, g, rest)
	case "pause":
		return a.schedulePauseResume(ctx, g, rest, true)
	case "resume":
		return a.schedulePauseResume(ctx, g, rest, false)
	case "set":
		return a.scheduleLegacySet(ctx, g, rest)
	case "clear":
		return a.scheduleLegacyClear(ctx, g, rest)
	default:
		// The v0.3.0 spelling `otter schedule <job>` showed a job's cadence.
		// Keep that meaning rather than failing on the older form.
		return a.scheduleShow(ctx, g, args)
	}
}

func scheduleUsage(a *App) {
	fmt.Fprint(a.Stderr, "Usage: otter schedule <command> [args]\n\n"+
		"Manage when a job fires on its own.\n\n"+
		"  otter schedule list [job]              list a job's schedules (default: this job)\n"+
		"  otter schedule add [job] <cron>        add a schedule\n"+
		"        --timezone <zone>                IANA zone (default UTC)\n"+
		"        --payload <json>                 per-occurrence ctx.trigger.body\n"+
		"        --missed-policy <policy>         skip | coalesce | catch_up\n"+
		"        --idempotency-key <key>          retry-safe key (default: derived from the request)\n"+
		"  otter schedule show <schedule-id|job>  show one schedule, or a job's\n"+
		"  otter schedule update <schedule-id>    change a schedule\n"+
		"        --cron <expr>  --timezone <zone>  --payload <json>  --missed-policy <policy>\n"+
		"  otter schedule remove <schedule-id>    delete a schedule\n"+
		"  otter schedule pause <schedule-id>     hold one schedule back\n"+
		"  otter schedule resume <schedule-id>    release it\n\n"+
		"Deprecated, for a job's single cadence:\n"+
		"  otter schedule set [job] '<cron>'      replace the cadence\n"+
		"  otter schedule clear [job]             never fire on its own\n")
}

// scheduleList prints every schedule a job holds.
func (a *App) scheduleList(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("schedule list", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { scheduleUsage(a) }
	if err := fs.Parse(splitFlags(args)); err != nil {
		return 2
	}
	ref, ok := singleRef(a, fs.Args())
	if !ok {
		return 2
	}
	id, err := resolveJobRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	views, err := g.client().ListSchedules(ctx, id)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(api.ScheduleList{Schedules: views})
	}
	if len(views) == 0 {
		fmt.Fprintln(a.Stdout, "no schedules: this job never fires on its own")
		fmt.Fprintln(a.Stdout, "otter run still runs it on demand")
		return 0
	}
	for i, view := range views {
		if i > 0 {
			fmt.Fprintln(a.Stdout)
		}
		a.printScheduleView(view)
	}
	return 0
}

// scheduleShow prints one schedule by id, or every schedule of a job when the
// argument names a job.
func (a *App) scheduleShow(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("schedule show", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { scheduleUsage(a) }
	if err := fs.Parse(splitFlags(args)); err != nil {
		return 2
	}
	ref, ok := singleRef(a, fs.Args())
	if !ok {
		return 2
	}

	client := g.client()
	if view, err := client.GetSchedule(ctx, ref); err == nil {
		if g.jsonOut {
			return a.printJSON(view)
		}
		a.printScheduleView(*view)
		return 0
	} else if !isNotFound(err) {
		return a.fail(err)
	}

	id, err := resolveJobRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	views, err := client.ListSchedules(ctx, id)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(api.ScheduleList{Schedules: views})
	}
	if len(views) == 0 {
		fmt.Fprintln(a.Stdout, "no schedules: this job never fires on its own")
		return 0
	}
	for i, view := range views {
		if i > 0 {
			fmt.Fprintln(a.Stdout)
		}
		a.printScheduleView(view)
	}
	return 0
}

// scheduleAdd creates an API-owned schedule.
func (a *App) scheduleAdd(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("schedule add", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { scheduleUsage(a) }
	timezone := fs.String("timezone", "", "IANA time zone (default UTC)")
	payload := fs.String("payload", "", "per-occurrence payload as a JSON object")
	policy := fs.String("missed-policy", "", "skip | coalesce | catch_up")
	key := fs.String("idempotency-key", "", "retry-safe key")
	if err := fs.Parse(splitFlags(args)); err != nil {
		return 2
	}

	rest := fs.Args()
	var ref, cron string
	switch len(rest) {
	case 1:
		ref, cron = ".", rest[0]
	case 2:
		ref, cron = rest[0], rest[1]
	default:
		fmt.Fprintln(a.Stderr, "otter: usage: otter schedule add [job] <cron>")
		return 2
	}
	if strings.TrimSpace(cron) == "" {
		fmt.Fprintln(a.Stderr, "otter: a cron expression is required")
		return 2
	}
	id, err := resolveJobRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	req := api.ScheduleCreateRequest{
		Cron:         strings.TrimSpace(cron),
		Timezone:     strings.TrimSpace(*timezone),
		MissedPolicy: strings.TrimSpace(*policy),
	}
	if strings.TrimSpace(*payload) != "" {
		raw := json.RawMessage(*payload)
		if !json.Valid(raw) || !strings.HasPrefix(strings.TrimSpace(*payload), "{") {
			fmt.Fprintln(a.Stderr, "otter: --payload must be a JSON object")
			return 2
		}
		req.Payload = raw
	}
	if *key == "" {
		*key = derivedIdempotencyKey(id, req)
	}

	view, err := g.client().CreateSchedule(ctx, id, req, *key)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(view)
	}
	a.printScheduleView(*view)
	return 0
}

// scheduleUpdate changes an API-owned schedule.
func (a *App) scheduleUpdate(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("schedule update", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { scheduleUsage(a) }
	cron := fs.String("cron", "", "new cron expression")
	timezone := fs.String("timezone", "", "new IANA time zone")
	payload := fs.String("payload", "", "new payload as a JSON object")
	policy := fs.String("missed-policy", "", "new missed-occurrence policy")
	if err := fs.Parse(splitFlags(args)); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter schedule update <schedule-id> [flags]")
		return 2
	}

	var req api.ScheduleUpdateRequest
	any := false
	if isFlagSet(fs, "cron") {
		req.Cron = cron
		any = true
	}
	if isFlagSet(fs, "timezone") {
		req.Timezone = timezone
		any = true
	}
	if isFlagSet(fs, "payload") {
		raw := json.RawMessage(*payload)
		if !json.Valid(raw) || !strings.HasPrefix(strings.TrimSpace(*payload), "{") {
			fmt.Fprintln(a.Stderr, "otter: --payload must be a JSON object")
			return 2
		}
		req.Payload = &raw
		any = true
	}
	if isFlagSet(fs, "missed-policy") {
		req.MissedPolicy = policy
		any = true
	}
	if !any {
		fmt.Fprintln(a.Stderr, "otter: nothing to update; pass at least one of --cron, --timezone, --payload, --missed-policy")
		return 2
	}

	view, err := g.client().UpdateSchedule(ctx, rest[0], req)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(view)
	}
	a.printScheduleView(*view)
	return 0
}

// scheduleRemove deletes an API-owned schedule.
func (a *App) scheduleRemove(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("schedule remove", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { scheduleUsage(a) }
	if err := fs.Parse(splitFlags(args)); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter schedule remove <schedule-id>")
		return 2
	}
	if err := g.client().DeleteSchedule(ctx, rest[0]); err != nil {
		return a.fail(err)
	}
	if !g.jsonOut {
		fmt.Fprintf(a.Stdout, "removed %s\n", rest[0])
	}
	return 0
}

// schedulePauseResume holds one schedule back or releases it.
func (a *App) schedulePauseResume(ctx context.Context, g globals, args []string, paused bool) int {
	name := "resume"
	if paused {
		name = "pause"
	}
	fs := flag.NewFlagSet("schedule "+name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { scheduleUsage(a) }
	if err := fs.Parse(splitFlags(args)); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintf(a.Stderr, "otter: usage: otter schedule %s <schedule-id>\n", name)
		return 2
	}
	view, err := g.client().SetSchedulePaused(ctx, rest[0], paused)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(view)
	}
	a.printScheduleView(*view)
	return 0
}

// scheduleLegacySet is the deprecated single-cadence setter.
func (a *App) scheduleLegacySet(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("schedule set", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { scheduleUsage(a) }
	if err := fs.Parse(splitFlags(args)); err != nil {
		return 2
	}
	rest := fs.Args()
	var ref, cron string
	switch len(rest) {
	case 0:
		fmt.Fprintln(a.Stderr, "otter: a cron expression is required")
		return 2
	case 1:
		ref, cron = ".", rest[0]
	case 2:
		ref, cron = rest[0], rest[1]
	default:
		fmt.Fprintln(a.Stderr, "otter: usage: otter schedule set [job] <cron>")
		return 2
	}
	return a.setJobSchedule(ctx, g, ref, cron)
}

// scheduleLegacyClear is the deprecated single-cadence clearer.
func (a *App) scheduleLegacyClear(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("schedule clear", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() { scheduleUsage(a) }
	if err := fs.Parse(splitFlags(args)); err != nil {
		return 2
	}
	ref, ok := singleRef(a, fs.Args())
	if !ok {
		return 2
	}
	return a.setJobSchedule(ctx, g, ref, "")
}

func (a *App) setJobSchedule(ctx context.Context, g globals, ref, cron string) int {
	id, err := resolveJobRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	view, err := g.client().SetSchedule(ctx, id, cron)
	if err != nil {
		return a.fail(err)
	}
	if g.jsonOut {
		return a.printJSON(view)
	}
	a.printScheduleView(*view)
	return 0
}

// splitFlags moves flag pairs ahead of positional arguments, so a command
// accepts `otter schedule update <id> --cron ...` as well as the reverse order.
// The standard flag package stops at the first positional argument; every flag
// in this command family takes a value, so a flag without "=" consumes its
// neighbour.
func splitFlags(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return append(flags, positional...)
}

// singleRef returns the one positional reference a command accepts, defaulting
// to the job in the working directory.
func singleRef(a *App, rest []string) (string, bool) {
	switch len(rest) {
	case 0:
		return ".", true
	case 1:
		return rest[0], true
	default:
		fmt.Fprintln(a.Stderr, "otter: too many arguments")
		return "", false
	}
}

// isFlagSet reports whether a flag was present on the command line.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// isNotFound reports whether an API error is a 404, which is how show falls
// back from a schedule id to a job reference.
func isNotFound(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// derivedIdempotencyKey makes a repeated identical `schedule add` a no-op while
// leaving a deliberate second, differing schedule possible. The key is a digest
// of exactly what the command asked for, so a script that re-runs the same line
// converges instead of accumulating duplicates.
func derivedIdempotencyKey(jobID string, req api.ScheduleCreateRequest) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		jobID, req.Cron, req.Timezone, req.MissedPolicy, string(req.Payload),
	}, "\x00")))
	return "cli:" + hex.EncodeToString(sum[:])
}

// printScheduleView states one schedule in the shape an operator reads.
func (a *App) printScheduleView(view api.ScheduleView) {
	when := view.Cron
	if when == "" {
		when = "no schedule: this job never fires on its own"
	}
	if view.Timezone != "" && view.Cron != "" {
		when += " (" + view.Timezone + ")"
	}
	fmt.Fprintf(a.Stdout, "%s\n", when)
	if view.ID != "" {
		fmt.Fprintf(a.Stdout, "id: %s\n", view.ID)
	}
	if view.Origin != "" {
		owner := view.Origin
		if view.Origin == "manifest" {
			owner = "manifest (change trigger.cron and reload)"
		}
		fmt.Fprintf(a.Stdout, "owner: %s\n", owner)
	}
	if len(view.Payload) > 0 && string(view.Payload) != "{}" {
		fmt.Fprintf(a.Stdout, "payload: %s\n", string(view.Payload))
	}
	if view.Paused {
		fmt.Fprintln(a.Stdout, "paused: yes")
	}
	switch {
	case view.NextRunAt != nil:
		fmt.Fprintf(a.Stdout, "next run: %s\n", view.NextRunAt.Local().Format("2006-01-02 15:04:05"))
	case view.Paused || view.Cron == "":
		// No next run is the honest answer; nothing more to say.
	default:
		fmt.Fprintln(a.Stdout, "next run: none")
	}
}
