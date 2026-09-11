// Package cli implements the `otter` command-line interface.
//
// The CLI is a thin HTTP client: it never touches the database directly, so
// it can run anywhere the API is reachable. The single exception is
// `otter validate`, which parses a manifest locally without a daemon.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/otter-runtime/otter/internal/api"
	"github.com/otter-runtime/otter/internal/config"
	"github.com/otter-runtime/otter/internal/runs"
)

// App is the CLI entry point.
type App struct {
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
}

// New creates a CLI app writing to the given streams.
func New(version string, stdout, stderr io.Writer) *App {
	return &App{Stdout: stdout, Stderr: stderr, Version: version}
}

type globals struct {
	api     string
	token   string
	jsonOut bool
	version bool
}

// Run executes the CLI and returns a process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	g, rest, err := parseGlobals(args)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	if g.version {
		fmt.Fprintf(a.Stdout, "otter %s\n", a.Version)
		return 0
	}
	if len(rest) == 0 {
		a.printUsage(a.Stderr)
		return 2
	}

	command, commandArgs := rest[0], rest[1:]

	switch command {
	case "help", "-h", "--help":
		a.printUsage(a.Stdout)
		return 0
	case "status":
		return a.cmdStatus(ctx, g)
	case "integrations":
		return a.cmdIntegrations(ctx, g, commandArgs)
	case "inspect":
		return a.cmdInspect(ctx, g, commandArgs)
	case "run":
		return a.cmdRun(ctx, g, commandArgs)
	case "runs":
		return a.cmdRuns(ctx, g, commandArgs)
	case "run-status":
		return a.cmdRunStatus(ctx, g, commandArgs)
	case "logs":
		return a.cmdLogs(ctx, g, commandArgs)
	case "state":
		return a.cmdState(ctx, g, commandArgs)
	case "validate":
		return a.cmdValidate(commandArgs)
	case "serve":
		// `otter serve` is the same daemon as `otterd`, in one process.
		return RunDaemon(ctx, a.Version, commandArgs, a.Stdout, a.Stderr)
	default:
		fmt.Fprintf(a.Stderr, "otter: unknown command %q\n\n", command)
		a.printUsage(a.Stderr)
		return 2
	}
}

// parseGlobals pulls the global flags out of anywhere in the argument list so
// that both `otter --api X runs` and `otter runs --api X` work.
func parseGlobals(args []string) (globals, []string, error) {
	var g globals
	rest := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--api":
			if i+1 >= len(args) {
				return g, nil, errors.New("--api requires a value")
			}
			i++
			g.api = args[i]
		case strings.HasPrefix(arg, "--api="):
			g.api = strings.TrimPrefix(arg, "--api=")

		case arg == "--token":
			if i+1 >= len(args) {
				return g, nil, errors.New("--token requires a value")
			}
			i++
			g.token = args[i]
		case strings.HasPrefix(arg, "--token="):
			g.token = strings.TrimPrefix(arg, "--token=")

		case arg == "--json":
			g.jsonOut = true

		case arg == "--version":
			g.version = true

		default:
			rest = append(rest, arg)
		}
	}

	return g, rest, nil
}

func (g globals) client() *api.Client {
	base := g.api
	if base == "" {
		base = os.Getenv("OTTER_API_URL")
	}
	token := g.token
	if token == "" {
		token = os.Getenv("OTTER_API_TOKEN")
	}
	return api.NewClient(base, token)
}

// Commands -------------------------------------------------------------------

func (a *App) cmdStatus(ctx context.Context, g globals) int {
	client := g.client()

	health, err := client.Health(ctx)
	if err != nil {
		return a.fail(err)
	}

	if g.jsonOut {
		return a.printJSON(health)
	}

	fmt.Fprintf(a.Stdout, "api:           %s\n", client.BaseURL)
	fmt.Fprintf(a.Stdout, "version:       %s\n", health.Version)
	fmt.Fprintf(a.Stdout, "status:        %s\n", health.Status)
	fmt.Fprintf(a.Stdout, "uptime:        %s\n",
		(time.Duration(health.UptimeSeconds * float64(time.Second))).Round(time.Second))

	// The daemon only discloses counters to an authenticated caller, so a
	// missing or wrong token shows up here instead of failing silently.
	if health.Integrations == nil {
		fmt.Fprintln(a.Stderr, "otter: run counts unavailable: the daemon requires an API token")
		fmt.Fprintln(a.Stderr, "hint: set OTTER_API_TOKEN or pass --token")
		return 0
	}

	queueDepth := 0
	if health.QueueDepth != nil {
		queueDepth = *health.QueueDepth
	}

	fmt.Fprintf(a.Stdout, "integrations:  %d total, %d valid, %d invalid\n",
		health.Integrations.Total, health.Integrations.Valid, health.Integrations.Invalid)
	fmt.Fprintf(a.Stdout, "queue depth:   %d\n", queueDepth)
	fmt.Fprintf(a.Stdout, "runs:          running=%d queued=%d retrying=%d\n",
		health.Runs[string(runs.StatusRunning)],
		health.Runs[string(runs.StatusQueued)],
		health.Runs[string(runs.StatusRetrying)])
	fmt.Fprintf(a.Stdout, "               succeeded=%d failed=%d timed_out=%d cancelled=%d\n",
		health.Runs[string(runs.StatusSucceeded)],
		health.Runs[string(runs.StatusFailed)],
		health.Runs[string(runs.StatusTimedOut)],
		health.Runs[string(runs.StatusCancelled)])
	return 0
}

func (a *App) cmdIntegrations(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("integrations", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	all := fs.Bool("all", false, "include invalid integrations")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	list, err := g.client().ListIntegrations(ctx)
	if err != nil {
		return a.fail(err)
	}

	if g.jsonOut {
		return a.printJSON(list)
	}

	printed := 0
	for _, it := range list {
		if !it.Valid {
			if !*all {
				continue
			}
			fmt.Fprintf(a.Stdout, "%s\t(invalid: %s)\n", it.ID, oneLine(it.Error))
			printed++
			continue
		}
		// The default output is exactly the integration names, one per line,
		// so it can be consumed by scripts.
		fmt.Fprintln(a.Stdout, it.ID)
		printed++
	}

	if printed == 0 {
		fmt.Fprintln(a.Stderr, "otter: no integrations found (use --all to include invalid ones)")
	}
	return 0
}

func (a *App) cmdInspect(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter inspect <integration>")
		return 2
	}
	id := fs.Arg(0)

	client := g.client()
	it, err := client.GetIntegration(ctx, id)
	if err != nil {
		return a.fail(err)
	}

	if g.jsonOut {
		return a.printJSON(it)
	}

	fmt.Fprintf(a.Stdout, "integration:   %s\n", it.ID)
	if it.Description != "" {
		fmt.Fprintf(a.Stdout, "description:   %s\n", it.Description)
	}
	fmt.Fprintf(a.Stdout, "path:          %s\n", it.Path)
	fmt.Fprintf(a.Stdout, "entrypoint:    %s\n", it.Entrypoint)
	fmt.Fprintf(a.Stdout, "python:        %s\n", it.PythonExecutable)
	fmt.Fprintf(a.Stdout, "timeout:       %ds\n", it.TimeoutSeconds)
	fmt.Fprintf(a.Stdout, "concurrency:   %d\n", it.Concurrency)
	fmt.Fprintf(a.Stdout, "retry:         attempts=%d (%d total) backoff=%s initial_delay=%s max_delay=%s\n",
		it.Retry.Attempts, it.Retry.MaxAttempts, it.Retry.Backoff, it.Retry.InitialDelay, it.Retry.MaxDelay)
	fmt.Fprintf(a.Stdout, "valid:         %t\n", it.Valid)
	if !it.Valid {
		fmt.Fprintf(a.Stdout, "error:         %s\n", oneLine(it.Error))
	}

	if it.Triggers.Cron != "" {
		fmt.Fprintf(a.Stdout, "cron:          %s\n", it.Triggers.Cron)
	}
	if it.NextRunAt != nil {
		fmt.Fprintf(a.Stdout, "next run:      %s\n", it.NextRunAt.Format(time.RFC3339))
	}
	if it.Triggers.WebhookEnabled {
		fmt.Fprintf(a.Stdout, "webhook:       enabled\n")
		fmt.Fprintf(a.Stdout, "webhook url:   POST %s%s\n", client.BaseURL, it.Triggers.WebhookURL)
		fmt.Fprintf(a.Stdout, "webhook token: %s\n", it.Triggers.WebhookToken)
		fmt.Fprintf(a.Stdout, "               curl -X POST %s%s -H 'X-Otter-Token: %s' -d '{}'\n",
			client.BaseURL, it.Triggers.WebhookURL, it.Triggers.WebhookToken)
	}

	if len(it.Env) > 0 {
		fmt.Fprintf(a.Stdout, "env:\n")
		for _, key := range sortedStringKeys(it.Env) {
			fmt.Fprintf(a.Stdout, "  %s=%s\n", key, it.Env[key])
		}
	}
	if len(it.Secrets) > 0 {
		fmt.Fprintf(a.Stdout, "secrets:       %s\n", strings.Join(it.Secrets, ", "))
	}

	recent, err := client.ListRuns(ctx, api.RunsQuery{IntegrationID: id, Limit: 5})
	if err == nil && len(recent) > 0 {
		fmt.Fprintf(a.Stdout, "\nrecent runs:\n")
		a.writeRunTable(recent)
	}

	return 0
}

func (a *App) cmdRun(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	body := fs.String("body", "", "JSON value recorded as the trigger body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter run <integration> [--body <json>]")
		return 2
	}

	var payload json.RawMessage
	if *body != "" {
		if !json.Valid([]byte(*body)) {
			fmt.Fprintf(a.Stderr, "otter: --body must be valid JSON, got %q\n", *body)
			return 2
		}
		payload = json.RawMessage(*body)
	}

	runID, err := g.client().SubmitRun(ctx, fs.Arg(0), payload)
	if err != nil {
		return a.fail(err)
	}

	// Only the run id goes to stdout so it can be captured by scripts.
	fmt.Fprintln(a.Stdout, runID)
	return 0
}

func (a *App) cmdRuns(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	integration := fs.String("integration", "", "filter by integration")
	status := fs.String("status", "", "filter by status")
	limit := fs.Int("limit", 20, "maximum number of runs")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *status != "" && !runs.Status(*status).Valid() {
		fmt.Fprintf(a.Stderr, "otter: unknown status %q (valid: %s)\n", *status, statusList())
		return 2
	}

	list, err := g.client().ListRuns(ctx, api.RunsQuery{
		IntegrationID: *integration,
		Status:        *status,
		Limit:         *limit,
	})
	if err != nil {
		return a.fail(err)
	}

	if g.jsonOut {
		return a.printJSON(list)
	}
	if len(list) == 0 {
		fmt.Fprintln(a.Stdout, "no runs")
		return 0
	}
	a.writeRunTable(list)
	return 0
}

func (a *App) writeRunTable(list []*runs.Run) {
	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN ID\tINTEGRATION\tTRIGGER\tSTATUS\tATTEMPT\tSTARTED\tDURATION")
	for _, r := range list {
		started := "-"
		if r.StartedAt != nil {
			started = r.StartedAt.Format("2006-01-02T15:04:05Z")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			r.ID, r.IntegrationID, r.TriggerType, r.Status, r.Attempt, started,
			r.Duration().Round(time.Millisecond))
	}
	_ = tw.Flush()
}

func (a *App) cmdRunStatus(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("run-status", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter run-status <run-id>")
		return 2
	}

	view, err := g.client().GetRun(ctx, fs.Arg(0))
	if err != nil {
		return a.fail(err)
	}

	if g.jsonOut {
		return a.printJSON(view)
	}

	r := view.Run
	fmt.Fprintf(a.Stdout, "run id:        %s\n", r.ID)
	fmt.Fprintf(a.Stdout, "integration:   %s\n", r.IntegrationID)
	fmt.Fprintf(a.Stdout, "trigger:       %s\n", r.TriggerType)
	fmt.Fprintf(a.Stdout, "status:        %s\n", r.Status)
	fmt.Fprintf(a.Stdout, "attempt:       %d\n", r.Attempt)
	if r.ParentRunID != nil {
		fmt.Fprintf(a.Stdout, "retry of:      %s\n", *r.ParentRunID)
	}
	if view.RootRunID != r.ID {
		fmt.Fprintf(a.Stdout, "root run:      %s\n", view.RootRunID)
		fmt.Fprintf(a.Stdout, "latest status: %s\n", view.LatestStatus)
	}
	fmt.Fprintf(a.Stdout, "created:       %s\n", r.CreatedAt.Format(time.RFC3339))
	if r.StartedAt != nil {
		fmt.Fprintf(a.Stdout, "started:       %s\n", r.StartedAt.Format(time.RFC3339))
	}
	if r.FinishedAt != nil {
		fmt.Fprintf(a.Stdout, "finished:      %s\n", r.FinishedAt.Format(time.RFC3339))
		fmt.Fprintf(a.Stdout, "duration:      %s\n", r.Duration().Round(time.Millisecond))
	}
	if r.ExitCode != nil {
		fmt.Fprintf(a.Stdout, "exit code:     %d\n", *r.ExitCode)
	}
	if r.Error != nil && *r.Error != "" {
		fmt.Fprintf(a.Stdout, "error:         %s\n", oneLine(*r.Error))
	}

	if len(view.Attempts) > 1 {
		fmt.Fprintf(a.Stdout, "\nattempts:\n")
		tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  RUN ID\tATTEMPT\tSTATUS\tSTARTED\tDURATION\tERROR")
		for _, attempt := range view.Attempts {
			started := "-"
			if attempt.StartedAt != nil {
				started = attempt.StartedAt.Format("15:04:05")
			}
			errText := "-"
			if attempt.Error != nil && *attempt.Error != "" {
				errText = oneLine(*attempt.Error)
			}
			fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\t%s\n",
				attempt.ID, attempt.Attempt, attempt.Status, started,
				attempt.Duration().Round(time.Millisecond), errText)
		}
		_ = tw.Flush()
	}
	return 0
}

func (a *App) cmdLogs(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	follow := fs.Bool("follow", false, "keep polling for new output")
	poll := fs.Duration("poll", 500*time.Millisecond, "poll interval used with --follow")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter logs <run-id> [--follow]")
		return 2
	}
	runID := fs.Arg(0)
	client := g.client()

	var afterID int64
	emptyPolls := 0

	for {
		entries, err := client.GetLogs(ctx, runID, afterID, 1000)
		if err != nil {
			return a.fail(err)
		}

		for _, entry := range entries {
			afterID = entry.ID
			switch entry.Stream {
			case runs.StreamStderr:
				// Integration stderr stays on the CLI's stderr, mirroring the
				// process it came from.
				fmt.Fprintln(a.Stderr, entry.Message)
			default:
				// stdout and otter (the SDK's ctx.log plus runtime lifecycle
				// events) both go to stdout so `otter logs <id>` shows the
				// complete output stream.
				fmt.Fprintln(a.Stdout, entry.Message)
			}
		}

		if !*follow {
			return 0
		}

		if len(entries) == 0 {
			emptyPolls++
		} else {
			emptyPolls = 0
		}

		// Stop once the run is finished and one poll has come back empty;
		// the second empty poll gives the worker time to write its final
		// lifecycle line.
		if emptyPolls >= 2 {
			view, err := client.GetRun(ctx, runID)
			if err == nil && view.Run.Status.Terminal() {
				return 0
			}
		}

		select {
		case <-ctx.Done():
			return 0
		case <-time.After(*poll):
		}
	}
}

func (a *App) cmdState(ctx context.Context, g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter state <get|set|delete> <integration> <key> [json]")
		return 2
	}

	client := g.client()
	sub, rest := args[0], args[1:]

	switch sub {
	case "get":
		if len(rest) != 2 {
			fmt.Fprintln(a.Stderr, "otter: usage: otter state get <integration> <key>")
			return 2
		}
		value, err := client.GetState(ctx, rest[0], rest[1])
		if err != nil {
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && apiErr.IsNotFound() {
				fmt.Fprintf(a.Stderr, "otter: %s/%s is not set\n", rest[0], rest[1])
				return 1
			}
			return a.fail(err)
		}
		// Print exactly the stored JSON value so it can be piped.
		fmt.Fprintln(a.Stdout, string(value))
		return 0

	case "set":
		if len(rest) != 3 {
			fmt.Fprintln(a.Stderr, "otter: usage: otter state set <integration> <key> <json>")
			return 2
		}
		if !json.Valid([]byte(rest[2])) {
			fmt.Fprintf(a.Stderr, "otter: value must be valid JSON, got %q (quote strings as '\"text\"')\n", rest[2])
			return 2
		}
		resp, err := client.SetState(ctx, rest[0], rest[1], json.RawMessage(rest[2]))
		if err != nil {
			return a.fail(err)
		}
		if g.jsonOut {
			return a.printJSON(resp)
		}
		fmt.Fprintf(a.Stderr, "set %s/%s = %s\n", rest[0], rest[1], string(resp.Value))
		return 0

	case "delete", "del", "rm":
		if len(rest) != 2 {
			fmt.Fprintln(a.Stderr, "otter: usage: otter state delete <integration> <key>")
			return 2
		}
		if err := client.DeleteState(ctx, rest[0], rest[1]); err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.Stderr, "deleted %s/%s\n", rest[0], rest[1])
		return 0

	default:
		fmt.Fprintf(a.Stderr, "otter: unknown state subcommand %q\n", sub)
		return 2
	}
}

// cmdValidate runs entirely locally: no daemon required.
func (a *App) cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter validate <otter.yaml|directory>")
		return 2
	}
	target := fs.Arg(0)

	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	if !info.IsDir() {
		m, err := config.LoadAndValidate(target)
		if err != nil {
			fmt.Fprintf(a.Stderr, "invalid: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Stdout, "ok: %s (%s)\n", m.Name, m.Path)
		return 0
	}

	items, err := config.Discover(target)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}
	if len(items) == 0 {
		fmt.Fprintf(a.Stderr, "otter: no %s found under %s\n", config.ManifestFileName, target)
		return 1
	}

	failures := 0
	for _, it := range items {
		if it.Valid {
			fmt.Fprintf(a.Stdout, "ok: %s (%s)\n", it.ID, it.ManifestPath)
			continue
		}
		failures++
		fmt.Fprintf(a.Stderr, "invalid: %s\n", it.ID)
		for _, line := range strings.Split(it.Error, "\n") {
			fmt.Fprintf(a.Stderr, "  %s\n", line)
		}
	}

	if failures > 0 {
		fmt.Fprintf(a.Stderr, "\n%d of %d integration(s) are invalid\n", failures, len(items))
		return 1
	}
	fmt.Fprintf(a.Stdout, "\n%d integration(s) valid\n", len(items))
	return 0
}

// Output helpers --------------------------------------------------------------

func (a *App) printJSON(v any) int {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: cannot encode output: %v\n", err)
		return 1
	}
	fmt.Fprintln(a.Stdout, string(encoded))
	return 0
}

// fail reports an error and returns a non-zero exit code, adding a hint when
// the daemon is simply not reachable.
func (a *App) fail(err error) int {
	fmt.Fprintf(a.Stderr, "otter: %v\n", err)

	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		if apiErr.IsUnauthorized() {
			fmt.Fprintln(a.Stderr, "hint: set OTTER_API_TOKEN or pass --token")
		}
		return 1
	}

	fmt.Fprintln(a.Stderr, "hint: is otterd running? start it with:")
	fmt.Fprintln(a.Stderr, "  otterd --integrations ./examples --data ./tmp")
	return 1
}

func (a *App) printUsage(w io.Writer) {
	fmt.Fprintf(w, `Otter is a lightweight runtime for running integration code anywhere.

Usage:
  otter [--api <url>] [--token <token>] [--json] <command> [arguments]

Runtime:
  status                          show daemon health and run counts
  integrations [--all]            list integration names
  inspect <integration>           show one integration in detail
  run <integration> [--body J]    queue a manual run and print its run id
  serve [flags]                   run the daemon (same as the otterd binary)

Runs:
  runs [--integration I] [--status S] [--limit N]
  run-status <run-id>             show a run and its retry attempts
  logs <run-id> [--follow]        print captured output

State:
  state get <integration> <key>
  state set <integration> <key> <json>
  state delete <integration> <key>

Manifests:
  validate <otter.yaml|directory> validate without a running daemon

Global flags:
  --api <url>    daemon URL (default %s, or OTTER_API_URL)
  --token <tok>  API token (or OTTER_API_TOKEN)
  --json         emit raw JSON instead of formatted text
  --version      print the version

Examples:
  otter integrations
  otter run counter
  otter logs $(otter run counter) --follow
  otter state get counter count
`, api.DefaultBaseURL)
}

// small helpers ---------------------------------------------------------------

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func statusList() string {
	names := make([]string, 0, len(runs.AllStatuses()))
	for _, s := range runs.AllStatuses() {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
