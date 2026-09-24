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
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
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

	// Resolve the daemon before any command needs it. This is what lets a bare
	// `otter run hello` reach a project runtime with no --api flag and no
	// wrapper to remember. The boolean reports whether a workspace was found
	// at all, which the daemon commands below require.
	inProject := resolveAPI(&g)

	if g.version {
		fmt.Fprintf(a.Stdout, "otter %s\n", a.Version)
		return 0
	}
	if len(rest) == 0 {
		a.printUsage(a.Stderr)
		return 2
	}

	command, commandArgs := rest[0], rest[1:]

	// Commands that act on a runtime need one to act on. There is deliberately
	// no ambient default: falling back to a fixed port meant a command could
	// silently answer for a different workspace, which is worse than refusing.
	if !inProject && g.api == "" && needsDaemon(command) {
		fmt.Fprintf(a.Stderr, "otter: no workspace here (no .otter in this directory or above)\n")
		fmt.Fprintf(a.Stderr, "otter: cd into a workspace, start one with otter start, or pass --api <url>\n")
		return 2
	}

	switch command {
	case "help", "-h", "--help":
		a.printUsage(a.Stdout)
		return 0
	case "status":
		return a.cmdStatus(ctx, g)
	case "integrations":
		return a.cmdIntegrations(ctx, g, commandArgs)
	case "reload":
		return a.cmdReload(ctx, g, commandArgs)
	case "inspect":
		return a.cmdInspect(ctx, g, commandArgs)
	case "register":
		return a.cmdRegister(ctx, g, commandArgs)
	case "reset":
		return a.cmdReset(ctx, g, commandArgs)
	case "delete":
		return a.cmdDelete(ctx, g, commandArgs)
	case "move":
		return a.cmdMove(ctx, g, commandArgs)
	case "run":
		return a.cmdRun(ctx, g, commandArgs)
	case "runs":
		return a.cmdRuns(ctx, g, commandArgs)
	case "run-status":
		return a.cmdRunStatus(ctx, g, commandArgs)
	case "logs":
		return a.cmdLogs(ctx, g, commandArgs)
	case "requests":
		return a.cmdRequests(ctx, g, commandArgs)
	case "request":
		return a.cmdRequest(ctx, g, commandArgs)
	case "state":
		return a.cmdState(ctx, g, commandArgs)
	case "validate":
		return a.cmdValidate(commandArgs)
	case "identity":
		return a.cmdIdentity(ctx, commandArgs)
	case "init":
		// Scaffolds a workspace and one integration. Local: no daemon, no
		// network, nothing but files the developer is expected to edit.
		return a.cmdInit(commandArgs)
	case "start":
		// Project-aware: finds the project, picks a free port, loads the
		// project's environment files, records where it listens.
		return a.cmdStart(ctx, commandArgs)
	case "stop":
		return a.cmdStop(ctx, commandArgs)
	case "serve":
		// `otter serve` is the daemon itself, with the daemon's own flag
		// defaults. It stays for systemd units and scripts that already pass
		// --integrations, --data and --listen explicitly.
		return RunDaemon(ctx, a.Version, commandArgs, a.Stdout, a.Stderr)
	case "deploy":
		return a.cmdDeploy(ctx, g, commandArgs)
	case "release":
		return a.cmdRelease(ctx, g, commandArgs)
	case "prepare":
		return a.cmdPrepare(ctx, commandArgs)
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
	if token == "" {
		token = localHostToken(base)
	}
	return api.NewClient(base, token)
}

// EnvDir is where `otter deploy` writes one environment file per integration,
// mode 0600. The API token lives in one of them.
const EnvDir = "/etc/otter"

// envDirForTest lets a test point the lookup at a temporary directory. It is
// the deploy location in every real build.
var envDirForTest = EnvDir

// localHostToken reads the API token from the daemon's own environment file.
//
// On the host that runs otterd, the token is already on disk -- systemd reads
// it out of /etc/otter/<integration>.env for the daemon. An operator's shell is
// a different process and inherits none of it, so every command on the host
// otherwise starts with a 401 and a copy-and-paste. Reading it here removes
// that without weakening anything: the file is 0600 and only root can read it.
//
// It only applies to a loopback API. A tunnel forwards a remote daemon to
// 127.0.0.1 on a machine that may have its own /etc/otter, and silently
// presenting the wrong token would be more confusing than a clear 401.
func localHostToken(base string) string {
	if !isLoopbackBase(base) {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(envDirForTest, "*.env"))
	if err != nil {
		return ""
	}
	sort.Strings(matches)
	for _, path := range matches {
		if token := readEnvValue(path, "OTTER_API_TOKEN"); token != "" {
			return token
		}
	}
	return ""
}

// isLoopbackBase reports whether a base URL points at this machine, including
// the empty value that means "use the default".
func isLoopbackBase(base string) bool {
	if strings.TrimSpace(base) == "" {
		return true
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// readEnvValue returns one KEY=value from a systemd environment file. Values
// are taken verbatim: these files are written by `otter deploy` and never
// contain shell syntax.
func readEnvValue(path, key string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, prefix))
	}
	return ""
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
	schedule := fs.Bool("schedule", false, "show each integration's cron, next run and last outcome")
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

	if *schedule {
		return a.printSchedule(ctx, g, list, *all)
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

// printSchedule answers the question "is this actually running on a schedule?":
// what each integration's cron is, when it next fires, and what happened last
// time it did.
//
// A cron integration that has never run is the common case after a first
// start, so "no runs yet" is reported rather than an empty column.
func (a *App) printSchedule(ctx context.Context, g globals, list []api.IntegrationView, includeInvalid bool) int {
	client := g.client()
	now := time.Now().UTC()

	fmt.Fprintf(a.Stdout, "%-20s %-16s %-21s %-10s %s\n",
		"INTEGRATION", "CRON", "NEXT RUN", "IN", "LAST RUN")
	fmt.Fprintln(a.Stdout, strings.Repeat("-", 92))

	shown := 0
	for _, it := range list {
		if !it.Valid {
			if !includeInvalid {
				continue
			}
			fmt.Fprintf(a.Stdout, "%-20s %-16s %-21s %-10s %s\n",
				it.ID, "-", "-", "-", "(invalid: "+oneLine(it.Error)+")")
			shown++
			continue
		}

		cron := it.Triggers.Cron
		if cron == "" {
			continue // not scheduled; this view is about schedules
		}
		shown++

		next, in := "-", "-"
		if it.NextRunAt != nil {
			next = it.NextRunAt.Local().Format("2006-01-02 15:04:05")
			in = it.NextRunAt.Sub(now).Round(time.Second).String()
		}

		last := "no runs yet"
		// A small limit keeps this a status view rather than a history dump.
		if recent, err := client.ListRuns(ctx, api.RunsQuery{IntegrationID: it.ID, Limit: 1}); err == nil && len(recent) > 0 {
			r := recent[0]
			last = fmt.Sprintf("%s at %s", r.Status, r.CreatedAt.Local().Format("15:04:05"))
			if r.Error != nil && *r.Error != "" && r.Status != runs.StatusSucceeded {
				last += " (" + oneLine(*r.Error) + ")"
			}
		}
		fmt.Fprintf(a.Stdout, "%-20s %-16s %-21s %-10s %s\n", it.ID, cron, next, in, last)
	}

	if shown == 0 {
		fmt.Fprintln(a.Stderr, "otter: no integrations with a cron trigger")
		return 0
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
	// Accept a directory or manifest too, so `otter inspect .` shows the
	// integration the working directory holds.
	id, err := resolveIntegrationRef(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

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
	if it.PythonMode == "managed" {
		fmt.Fprintf(a.Stdout, "python:        managed (prepared environment)\n")
	} else {
		fmt.Fprintf(a.Stdout, "python:        %s\n", it.PythonExecutable)
	}
	fmt.Fprintf(a.Stdout, "timeout:       %ds\n", it.TimeoutSeconds)
	fmt.Fprintf(a.Stdout, "concurrency:   %d\n", it.Concurrency)
	fmt.Fprintf(a.Stdout, "capture:       %s\n", captureSummary(it.Capture))
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

// captureSummary spells out what a capture policy means for an integration, so
// `otter inspect` answers "are payloads being stored for this one?" without the
// reader having to know the policy names. Full is the default, which makes the
// question worth answering explicitly.
func captureSummary(policy string) string {
	switch inspection.Policy(policy) {
	case inspection.PolicyFull:
		return "full (request and response headers and JSON bodies are stored)"
	case inspection.PolicyMetadata:
		return "metadata (request summaries only; no headers or bodies)"
	case inspection.PolicyOff:
		return "off (nothing is recorded)"
	default:
		return policy
	}
}

// interactiveOutput decides whether `otter run` waits and prints a summary.
// It is a variable so a test can exercise the waiting path without a terminal,
// which is otherwise the one branch that cannot be driven in CI.
var interactiveOutput = func(w io.Writer) bool { return isTerminal(w) }

// normalizeLongFlags rewrites `--name value` to `-name value`, which is the
// spelling the standard flag package understands. Without it, `otter run x
// --no-wait` fails with "flag provided but not defined" while `--no-wait=true`
// works, which is a difference nobody should have to know.
func normalizeLongFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			return append(out, args[i:]...)
		}
		if strings.HasPrefix(arg, "--") && len(arg) > 2 && !strings.Contains(arg, "=") {
			out = append(out, "-"+arg[2:])
			continue
		}
		out = append(out, arg)
	}
	return out
}

// flagsFirst moves flag arguments ahead of positional ones.
//
// The standard flag package stops parsing at the first non-flag argument, so
// `otter run counter --no-wait` would leave --no-wait as a second positional
// and fail with a usage error. Developers write flags last; reordering before
// parsing is what makes the obvious spelling work.
func flagsFirst(args []string, takesValue func(string) bool) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if takesValue(arg) && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func (a *App) cmdRun(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	body := fs.String("body", "", "JSON value recorded as the trigger body")
	noWait := fs.Bool("no-wait", false, "queue the run and print its id without waiting")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the run and any retries to finish")
	capture := fs.String("capture", "", "override this run's HTTP capture policy: off, metadata or full (default: the integration's policy)")
	// Only --body, --timeout and --capture take a value; the rest are booleans,
	// so the reorderer needs no reflection over the flag set.
	takesValue := func(arg string) bool {
		name := strings.TrimLeft(arg, "-")
		if i := strings.Index(name, "="); i >= 0 {
			name = name[:i]
		}
		return name == "body" || name == "timeout" || name == "poll" || name == "capture"
	}
	args = flagsFirst(normalizeLongFlags(args), takesValue)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter run [integration] [--body <json>] [--no-wait] [--capture <policy>]")
		return 2
	}

	// Validate before submitting: a mistyped policy should not queue a run. An
	// omitted flag is not a policy, though: it leaves the choice to the
	// integration's manifest and then the deployment default, which only the
	// daemon can resolve.
	capturePolicy, err := inspection.ParsePolicyOverride(*capture)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
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

	// No argument means "the integration I am standing in", so `otter run` and
	// `otter run .` are the same command.
	ref := "."
	if fs.NArg() == 1 {
		ref = fs.Arg(0)
	}
	integration, err := resolveIntegrationRef(ref)
	if err != nil {
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		return 2
	}

	client := g.client()
	rootID, err := client.SubmitRunWithOptions(ctx, integration, payload, capturePolicy.String())
	if err != nil {
		return a.fail(err)
	}

	// A non-terminal stdout means the caller is a script or a pipe, where the
	// bare id is the useful answer -- the same rule `otter logs` uses. An
	// explicit --json or --no-wait says the same thing out loud.
	waiting := !*noWait && !g.jsonOut && interactiveOutput(a.Stdout)
	if !waiting {
		fmt.Fprintln(a.Stdout, rootID)
		return 0
	}

	// The id goes out before the wait begins: it is the handle for everything
	// that follows, and it must exist even if the wait is interrupted, the
	// daemon restarts, or this process is killed.
	fmt.Fprintf(a.Stdout, "run: %s\n", rootID)

	view, err := a.waitForRun(ctx, client, rootID, *timeout)
	if err != nil {
		// The run exists; say how to follow it rather than losing it behind
		// the failure to watch it.
		fmt.Fprintf(a.Stderr, "otter: %v\n", err)
		fmt.Fprintf(a.Stderr, "otter: otter logs %s --follow\n", rootID)
		return 1
	}

	// The id is already out; what remains is the outcome and the run's own
	// output. It is printed in full above because the daemon does not resolve
	// prefixes, so an abbreviated id would be unusable in the next command.
	a.printRunOutcome(a.Stdout, view)
	a.printRunOutput(ctx, client, view.ID)
	return runExitCode(view)
}

// printRunOutcome is one line, in the shape `make sync-run` used: a status
// with a colon, so a human reads it and a script can grep it.
func (a *App) printRunOutcome(w io.Writer, view *api.RunView) {
	fmt.Fprintf(w, "status: %s\n", view.LatestStatus)
}

// printRunOutput prints the run's captured records exactly as the daemon holds
// them -- the message with any structured fields as a trailing JSON object --
// so the output of `otter run` is the same text `otter logs` shows. No
// prefixing, no indenting, and no summary of somebody else's log format.
func (a *App) printRunOutput(ctx context.Context, client *api.Client, runID string) {
	entries, err := client.GetLogs(ctx, runID, 0, 1000)
	if err != nil {
		// The run is queued and its id is on the first line; losing the log
		// fetch must not lose the run.
		fmt.Fprintf(a.Stderr, "otter: could not read the run's output: %v\n", err)
		fmt.Fprintf(a.Stderr, "otter: otter logs %s --follow\n", runID)
		return
	}
	for _, entry := range entries {
		fmt.Fprintln(a.Stdout, rawLogLine(entry.Message))
	}
}

// rawLogLine is the log record as stored: `message {"field":...}`.
func rawLogLine(message string) string {
	text, fields := splitStructured(message)
	if len(fields) == 0 {
		return text
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return text
	}
	if text == "" {
		return string(encoded)
	}
	return text + " " + string(encoded)
}

// waitForRun polls until the whole retry chain reaches a terminal status.
//
// A failed attempt is retried as a new run, so the attempt that was submitted
// goes terminal while the work is still going. The view carries the root run
// and the latest attempt's status, which is what has to settle before the
// answer is known.
func (a *App) waitForRun(ctx context.Context, client *api.Client, rootID string, timeout time.Duration) (*api.RunView, error) {
	deadline := time.Now().Add(timeout)
	var lastAttempt string
	var lastProgress time.Time
	for {
		view, err := client.GetRun(ctx, rootID)
		if err != nil {
			return nil, err
		}
		if settled(view) {
			return view, nil
		}

		// Track movement through the chain: a new attempt, or a different
		// status, is progress. A failure that is not followed by another
		// attempt is the end of the chain rather than a pause in it.
		attempt := latestAttemptID(view)
		if attempt != lastAttempt {
			lastAttempt = attempt
			lastProgress = time.Now()
		}
		if view.LatestStatus == runs.StatusFailed && time.Since(lastProgress) > retryGrace {
			return view, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the run did not finish within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// retryGrace bounds how long a failed chain is watched for a scheduled retry.
// The retry is queued before its delay elapses, so a short grace is enough to
// see that one exists; for a backoff longer than this, `otter run-status` and
// the run history remain the way to follow it, rather than leaving the command
// blocked on someone else's retry policy.
const retryGrace = 15 * time.Second

// settled reports whether a retry chain has reached its final answer. A failed
// latest attempt is deliberately not settled: the daemon queues the next
// attempt before the previous one's failure is visible, so `failed` can be a
// pause rather than an end.
func settled(view *api.RunView) bool {
	switch view.LatestStatus {
	case runs.StatusSucceeded, runs.StatusCancelled, runs.StatusTimedOut:
		return true
	}
	return false
}

// latestAttemptID identifies the newest attempt in the chain, which is how the
// wait notices that a retry has begun.
func latestAttemptID(view *api.RunView) string {
	if len(view.Attempts) > 0 {
		return view.Attempts[len(view.Attempts)-1].ID
	}
	return view.ID
}

// runExitCode makes `otter run` usable in a shell conditional: a failed,
// timed-out or cancelled run is a non-zero exit.
func runExitCode(view *api.RunView) int {
	if view.LatestStatus == runs.StatusSucceeded {
		return 0
	}
	return 1
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
	if r.PythonMode != "" {
		fmt.Fprintf(a.Stdout, "python mode:   %s\n", r.PythonMode)
	}
	if r.PythonVersion != "" {
		fmt.Fprintf(a.Stdout, "python:        %s\n", r.PythonVersion)
	}
	if r.EnvironmentDigest != "" {
		fmt.Fprintf(a.Stdout, "environment:   %s\n", r.EnvironmentDigest)
	}
	if r.ReleaseDigest != "" {
		fmt.Fprintf(a.Stdout, "release:       %s\n", r.ReleaseDigest)
	}
	if r.SDKVersion != "" {
		fmt.Fprintf(a.Stdout, "sdk:           %s\n", r.SDKVersion)
	}
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
	pretty := fs.Bool("pretty", false, "human-readable output even when piped (overrides --json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter logs <run-id> [--follow]")
		return 2
	}
	runID := fs.Arg(0)
	client := g.client()

	// Machine-readable when asked for, or whenever stdout is not a terminal.
	//
	// This is the difference between `otter logs <id> | jq` working and the
	// operator having to redirect to a file and strip the human prefix before
	// jq can parse a line at all. A pipe gets JSONL; a terminal keeps the
	// readable form. No flag, no temp file.
	//
	// --pretty is the escape hatch for the one case auto-detection gets wrong:
	// `otter logs <id> | less` is a pipe, but the reader wants prose.
	structured := !*pretty && (g.jsonOut || !isTerminal(a.Stdout))

	var afterID int64
	emptyPolls := 0

	for {
		entries, err := client.GetLogs(ctx, runID, afterID, 1000)
		if err != nil {
			return a.fail(err)
		}

		for _, entry := range entries {
			afterID = entry.ID

			if structured {
				// Every stream goes to stdout here. A caller asking for a
				// machine-readable stream means to pipe it, and splitting it
				// across two file descriptors would silently drop half the
				// output. Which stream a line came from is a field instead.
				text, fields := splitStructured(entry.Message)
				record := logRecord{
					RunID:     entry.RunID,
					Timestamp: entry.Timestamp,
					Stream:    entry.Stream,
					Text:      text,
					Fields:    fields,
				}
				encoded, err := json.Marshal(record)
				if err != nil {
					fmt.Fprintf(a.Stderr, "otter: encode log line: %v\n", err)
					return 1
				}
				fmt.Fprintln(a.Stdout, string(encoded))
				continue
			}

			rendered := renderLogLine(entry.Message)
			switch entry.Stream {
			case runs.StreamStderr:
				// Integration stderr stays on the CLI's stderr, mirroring the
				// process it came from.
				fmt.Fprintln(a.Stderr, rendered)
			default:
				// stdout and otter (the SDK's ctx.log plus runtime lifecycle
				// events) both go to stdout so `otter logs <id>` shows the
				// complete output stream.
				fmt.Fprintln(a.Stdout, rendered)
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
		fmt.Fprintln(a.Stderr, "otter: usage: otter validate <otter.yaml|directory|integration>")
		return 2
	}
	target := fs.Arg(0)

	info, err := os.Stat(target)
	if err != nil {
		// Not a path. `otter init` tells a developer to run `otter validate
		// <name>`, and a name is the one thing a path-only command cannot take,
		// so a bare word is looked up in the workspace first.
		resolved, ok := manifestByName(target)
		if !ok {
			fmt.Fprintf(a.Stderr, "otter: %v\n", err)
			return 2
		}
		m, err := config.LoadAndValidate(resolved)
		if err != nil {
			fmt.Fprintf(a.Stderr, "invalid: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Stdout, "ok: %s (%s)\n", m.Name, m.Path)
		return 0
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
			// Each workspace has its own daemon and its own token, so naming the
			// directory would send an operator to a pile of files and leave them
			// guessing which one this daemon reads.
			fmt.Fprintln(a.Stderr, "hint: on the Otter host, each workspace's token is in "+
				EnvDir+"/workspaces/<workspace>.env")
		}
		// The daemon answered, so it is running: the reachability hint would
		// only misdirect. Its own message is the actionable one.
		return 1
	}

	fmt.Fprintln(a.Stderr, "hint: is otterd running? start it with:")
	fmt.Fprintln(a.Stderr, "  otterd --integrations ./integrations --data ./tmp")
	return 1
}

func (a *App) printUsage(w io.Writer) {
	fmt.Fprintf(w, `Otter is a lightweight runtime for running integration code anywhere.

Usage:
  otter [--api <url>] [--token <token>] [--json] <command> [arguments]

Getting started:
  init [--force] [name]           scaffold a workspace and one integration
  release [<integration>|.]       stage and activate an immutable release

Runtime:
  start [--detach]                run this project's runtime, free port, env loaded
  stop                            stop the runtime serving this project
  status                          show daemon health and run counts
  integrations [--all]            list integration names
  integrations --schedule         cron, next run and last outcome per integration
  reload                          re-read the integrations directory; no restart
  inspect <integration>           show one integration in detail
  run [<integration>] [--no-wait] run it, wait, print the outcome and its output
  run --capture off|metadata|full override this run's HTTP capture policy
  serve [flags]                   run the daemon with the daemon's own defaults

Identity:
  register [<path>|.]             give a source directory a durable identity
  reset <integration>             retire its identity, mint a fresh one at the same path
  delete <integration>            purge its state, history, tokens and releases
  move <integration> <dest>       preserve its identity across a directory rename
  identity migrate [--apply]      move a name-keyed workspace onto the identity registry

Runs:
  runs [--integration I] [--status S] [--limit N]
  run-status <run-id>             show a run and its retry attempts
  logs <run-id> [--follow]        print captured output (JSONL when piped; --pretty to force prose)
  requests <run-id>               list the outgoing HTTP a run recorded
  request <request-id>            show one recorded exchange with its payloads
  request <run-id> <request-id>   the same, when the id needs disambiguating

State:
  state get <integration> <key>
  state set <integration> <key> <json>
  state delete <integration> <key>

Manifests:
  validate <otter.yaml|dir|name>  validate without a running daemon

Python:
  prepare [--integrations DIR] [--data DIR] [<integration>|.]
                                  prepare opt-in managed Python environments
  release [--all] [<integration>|.]
                                  stage and activate an immutable release
  release --list <integration>    list staged releases
  release --list --all            every integration that has a release, and every one that does not
  release --list --all --prune    remove release directories with no registered identity (--apply to act)
  release --activate <digest> <integration>
                                  roll back to a staged release

Deployment:
  deploy --host <user@host>       install or update a remote runtime over SSH
  deploy --status                 show the last deploy from this checkout
  deploy --host <host> --destroy  stop and remove it

Global flags:
  --api <url>    daemon URL (default %s, or OTTER_API_URL)
  --token <tok>  API token (or OTTER_API_TOKEN)
  --json         emit raw JSON instead of formatted text
  --version      print the version

Without --api or OTTER_API_URL, the daemon URL is read from the nearest recorded
address below .otter/, walking up from the working directory. A running daemon
records its address there when it starts, so commands in a project reach the
daemon serving that project -- including one on a non-default port.

An integration is addressed by the label in its manifest, by the filesystem path
that holds that manifest, or by its durable identity as id:<id>: otter run .
runs the integration in the working directory, otter run inside one does the
same, and otter inspect . looks at it. A path is resolved through the registry,
so the directory name does not matter. Labels need not be unique; when two
integrations share one, the bare label is refused and the candidates are listed.

State, run history, webhook tokens, releases and environments belong to the
durable identity the runtime mints, not to the label, so renaming an
integration keeps them and copying a directory does not inherit them. See
docs/identity.md. The "otter identity migrate" command moves an older,
name-keyed workspace onto the registry.

A run executes the integration's active release rather than its source tree, so
an edit is not live until otter release stages and activates a new one. Every
integration needs a release before its first run, whether or not it uses managed
Python; otter release --all covers a whole workspace.

Examples:
  otter init acme                 # scaffold a workspace and an integration
  otter start                     # in a project: free port, env files loaded
  otter start --detach            # same, in the background
  otter run counter
  otter run                       # the integration in the working directory
  otter release                   # release the integration in this directory
  otter release --all             # release every integration in the workspace
  otter stop
  otter integrations
  otter reload                    # after adding an integration: no restart
  otter logs $(otter run counter) --follow
  otter state get counter count
  otter prepare shopify-to-salesforce
  otter deploy --host droplet
`, api.DefaultBaseURL)
}

// small helpers ---------------------------------------------------------------

// needsDaemon reports whether a command acts on a running runtime rather than
// on files in the working tree. The local commands -- init, validate, serve,
// release, prepare, deploy -- read and write files directly and need no daemon.
// The workspace-scoped ones among them resolve their directories from the
// workspace when there is one and from explicit flags when there is not, which
// is how `otter deploy` operates on a host that has no checkout.
func needsDaemon(command string) bool {
	switch command {
	case "status", "integrations", "reload", "inspect", "run", "runs", "run-status", "logs", "state",
		"register", "reset", "delete", "move", "requests", "request":
		return true
	default:
		return false
	}
}

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
