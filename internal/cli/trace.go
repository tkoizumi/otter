package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/timeline"
)

// cmdTrace prints one page of a finished attempt's merged timeline: its
// lifecycle lines, its captured output and its HTTP exchanges in one order.
//
// The operator-facing point is that these three used to require holding two
// command outputs side by side. The command is read-only and never follows a
// run: a timeline is a record of a finished attempt, so asking for one that is
// still executing is refused with a hint rather than a partial answer.
func (a *App) cmdTrace(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	limit := fs.Int("limit", timeline.DefaultLimit, "maximum number of events to print")
	after := fs.String("after", "", "continue from the cursor a previous page printed")
	noHTTP := fs.Bool("no-http", false, "omit captured HTTP exchanges (the capture state is still reported)")
	pretty := fs.Bool("pretty", false, "human-readable output even when piped (overrides --json)")
	follow := fs.Bool("follow", false, "not supported: a trace covers a finished attempt")
	takesValue := func(arg string) bool {
		name := strings.TrimLeft(arg, "-")
		if i := strings.Index(name, "="); i >= 0 {
			name = name[:i]
		}
		return name == "limit" || name == "after"
	}
	if err := fs.Parse(flagsFirst(normalizeLongFlags(args), takesValue)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter trace <run-id> [--limit N] [--after <cursor>] [--no-http] [--json]")
		return 2
	}
	// --follow is refused on its own terms, not because the run happens to be
	// running: a trace is only well defined for a finished attempt.
	if *follow {
		fmt.Fprintln(a.Stderr, "otter: otter trace does not follow a run; it shows a finished attempt")
		fmt.Fprintf(a.Stderr, "otter: follow live output with 'otter logs %s --follow'\n", fs.Arg(0))
		return 2
	}
	if *limit < 1 || *limit > timeline.MaxLimit {
		fmt.Fprintf(a.Stderr, "otter: --limit must be between 1 and %d\n", timeline.MaxLimit)
		return 2
	}

	// Machine-readable when asked for, or whenever stdout is not a terminal,
	// matching `otter logs`. The resolved mode is what a continuation must
	// repeat, so it is checked before the first request rather than after.
	structured := !*pretty && (g.jsonOut || !isTerminal(a.Stdout))
	if *after != "" && structured && !g.jsonOut {
		fmt.Fprintln(a.Stderr, "otter: continuing a piped trace requires --json, so the output mode cannot change mid-trace")
		fmt.Fprintf(a.Stderr, "otter: re-run with: otter trace %s --json --after %s\n", fs.Arg(0), *after)
		return 2
	}

	runID := fs.Arg(0)
	page, err := g.client().Timeline(ctx, timeline.Request{
		RunID:       runID,
		IncludeHTTP: !*noHTTP,
		After:       *after,
		Limit:       *limit,
	})
	if err != nil {
		return a.failTrace(runID, err)
	}

	if structured {
		return a.printTraceJSONL(page, runID, *noHTTP)
	}
	a.printTrace(page, runID, *noHTTP, *after != "")
	return 0
}

// failTrace explains the two failures an operator is most likely to hit and can
// act on: an attempt that has not finished, and evidence that moved under a
// continuation. Every other failure keeps its usual handling.
func (a *App) failTrace(runID string, err error) int {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 409 && strings.Contains(apiErr.Message, timeline.ErrRunNotTerminal.Error()):
			a.explainNonTerminal(runID, apiErr.Message)
			return 1
		case apiErr.StatusCode == 409 && strings.Contains(apiErr.Message, timeline.ErrEvidenceChanged.Error()):
			fmt.Fprintf(a.Stderr, "otter: %s\n", oneLine(apiErr.Message))
			fmt.Fprintf(a.Stderr, "otter: the run's recorded evidence changed since that cursor; "+
				"start over with 'otter trace %s' (discard any pages already collected)\n", runID)
			return 1
		}
	}
	return a.fail(err)
}

// explainNonTerminal turns a nonterminal status into the next command to run.
// The three statuses need different advice: a running attempt has output to
// follow, a retrying attempt has none of its own yet, and a queued one has not
// started at all.
func (a *App) explainNonTerminal(runID, message string) {
	fmt.Fprintf(a.Stderr, "otter: %s\n", oneLine(message))

	status := ""
	if i := strings.LastIndex(message, "status is "); i >= 0 {
		status = strings.TrimSpace(message[i+len("status is "):])
	}

	switch status {
	case "running":
		fmt.Fprintf(a.Stderr, "otter: follow it with 'otter logs %s --follow' or list 'otter requests %s'\n",
			runID, runID)
	case "retrying":
		// A retry row is created by the scheduler and has no logs of its own
		// until a worker claims it, so pointing at its logs would be a dead end.
		fmt.Fprintf(a.Stderr, "otter: this attempt is waiting to execute; it has no output yet\n")
		fmt.Fprintf(a.Stderr, "otter: check the attempt it retries with 'otter run-status %s', "+
			"then trace the finished parent it names\n", runID)
	case "queued":
		fmt.Fprintf(a.Stderr, "otter: this attempt is waiting for execution; it has no execution output yet\n")
		fmt.Fprintf(a.Stderr, "otter: see 'otter run-status %s' for the attempt and 'otter status' for the queue\n", runID)
	default:
		fmt.Fprintf(a.Stderr, "otter: it has not finished, so there is no timeline yet; "+
			"see 'otter run-status %s'\n", runID)
	}
}

// ---------------------------------------------------------------- human form

// printTrace renders a page for a human.
//
// A continuation re-prints the header: a reader resuming a long trace has
// scrolled the context off the screen, and the header is where the status and
// error that explain the events live.
func (a *App) printTrace(page *timeline.Page, runID string, noHTTP bool, resumed bool) {
	if resumed {
		fmt.Fprintln(a.Stdout, "(continuing; the header is repeated)")
	}
	a.printTraceHeader(page)

	if len(page.Events) == 0 {
		a.printTraceEmpty(page, noHTTP)
	} else {
		fmt.Fprintln(a.Stdout)
		a.printTraceTable(page)
	}

	a.printTraceFooter(page, runID, noHTTP)
}

func (a *App) printTraceHeader(page *timeline.Page) {
	ctx := page.Context
	name := ctx.IntegrationName
	if name == "" {
		name = ctx.IntegrationID
	}

	attempt := strconv.Itoa(ctx.Attempt)
	if ctx.Attempt > 0 {
		attempt = fmt.Sprintf("%d", ctx.Attempt)
	}
	fmt.Fprintf(a.Stdout, "run: %s   integration: %s   status: %s   attempt: %s\n",
		escapeTerminal(ctx.RunID), escapeTerminal(name), escapeTerminal(ctx.Status), attempt)

	parent := "-"
	if ctx.ParentRunID != "" {
		parent = escapeTerminal(ctx.ParentRunID)
	}
	fmt.Fprintf(a.Stdout, "release: %s   trigger: %s   parent: %s\n",
		orDash(ctx.ReleaseDigest), orDash(ctx.TriggerType), parent)

	if ctx.Error != "" {
		fmt.Fprintf(a.Stdout, "error: %s\n", escapeTerminal(ctx.Error))
	}
	fmt.Fprintf(a.Stdout, "retry context: otter run-status %s\n", escapeTerminal(ctx.RunID))

	fmt.Fprintf(a.Stdout, "capture: %s\n", traceCaptureState(ctx.Capture, ctx.IncludeHTTP))
}

// traceCaptureState explains the recording in one line. It never lets "no HTTP
// events below" stand in for "nothing was recorded": expiry is called out, and an
// expired recording still reports the incompleteness it had before retention,
// which the derived state on its own would hide.
func traceCaptureState(capture *inspection.RunCapture, includeHTTP bool) string {
	if capture == nil {
		return "unknown - the daemon returned no capture state"
	}

	var line string
	switch capture.State {
	case inspection.CaptureUnavailable:
		line = "unavailable - this run has no recording. It may still have made requests."
	case inspection.CaptureOff:
		line = "off - capture was disabled for this run, so outgoing HTTP was not recorded"
	case inspection.CaptureExpired:
		line = fmt.Sprintf("expired - the recorded requests (%d) were removed by retention; the summary was kept",
			capture.RequestCount)
	case inspection.CapturePending:
		// The attempt is finished but its recording is not: a run killed before
		// it could finalize leaves this. Saying "the run is still executing"
		// here would be wrong.
		line = "not finalized - the recording never completed, so it may be missing requests"
	case inspection.CaptureIncomplete:
		line = "incomplete - some capture was lost; the list below is not the whole story"
	default:
		if capture.RequestCount == 0 {
			line = "complete - capture was enabled and observed no outgoing HTTP requests"
		} else {
			line = fmt.Sprintf("complete - %d recorded request(s)", capture.RequestCount)
		}
	}

	if capture.Coverage != "" {
		line += "; coverage: " + escapeTerminal(capture.Coverage)
	}
	// Expiry supersedes the derived state, so the loss counters have to be
	// reported alongside it or a reader would take "expired" for "was complete".
	if capture.State == inspection.CaptureExpired && (capture.IncompleteCount > 0 || capture.DroppedEvents > 0) {
		line += fmt.Sprintf("; before expiry: %d request(s) never completed, %d event(s) dropped",
			capture.IncompleteCount, capture.DroppedEvents)
	}
	if !includeHTTP {
		line += "\n           HTTP events were excluded by --no-http"
	}
	return line
}

// printTraceEmpty explains an empty page instead of printing an empty table.
func (a *App) printTraceEmpty(page *timeline.Page, noHTTP bool) {
	switch {
	case noHTTP:
		fmt.Fprintln(a.Stdout, "\nNo events to show; HTTP events were excluded by --no-http.")
	case page.Context.Capture != nil && page.Context.Capture.State == inspection.CaptureUnavailable,
		page.Context.Capture != nil && page.Context.Capture.State == inspection.CaptureOff:
		fmt.Fprintln(a.Stdout, "\nNo events were retained for this run.")
	default:
		fmt.Fprintln(a.Stdout, "\nNo events were retained for this run (the run recorded no output and no requests).")
	}
}

func (a *App) printTraceTable(page *timeline.Page) {
	fmt.Fprintln(a.Stdout, "TIME          KIND       DETAIL")

	// A trace that spans more than one calendar day would be ambiguous with a
	// time-only column, so the date is added before the time.
	first := page.Events[0].At
	last := page.Events[len(page.Events)-1].At
	withDate := first.Year() != last.Year() || first.YearDay() != last.YearDay() ||
		(first.Hour() == 0 && first.Minute() == 0 && first.Second() == 0)

	for _, event := range page.Events {
		stamp := event.At.UTC().Format("15:04:05.000")
		if withDate {
			stamp = event.At.UTC().Format("2006-01-02 15:04:05.000")
		}
		switch event.Kind {
		case timeline.KindHTTP:
			a.printTraceHTTP(stamp, event, page.Context.RunID)
		case timeline.KindLifecycle:
			fmt.Fprintf(a.Stdout, "%-13s %-10s %s\n", stamp, timeline.KindLifecycle, escapeTerminal(event.Message))
		default:
			// The KIND column shows the event kind, never the stream name. The
			// `otter` stream carries both the runtime's narration and an
			// integration's ctx.log output, so printing it there would present
			// the integration's own line as if it were the runtime talking.
			// Which stream a line came from is useful, so stderr keeps its
			// marker in the detail column where stdout is the unmarked default.
			message := escapeTerminal(event.Message)
			if event.Stream == runs.StreamStderr {
				message = "[stderr] " + message
			}
			fmt.Fprintf(a.Stdout, "%-13s %-10s %s\n", stamp, timeline.KindLog, message)
		}
	}
}

func (a *App) printTraceHTTP(stamp string, event timeline.Event, runID string) {
	http := event.HTTP
	if http == nil {
		return
	}

	summary := fmt.Sprintf("%s %s", orDash(http.Method), escapeTerminal(http.URL))
	switch {
	case http.ErrorClass != "":
		summary += " -> error:" + escapeTerminal(http.ErrorClass)
	case http.StatusCode != nil:
		summary += " -> " + strconv.Itoa(*http.StatusCode)
	}
	if http.DurationMS != nil {
		summary += ", " + (time.Duration(*http.DurationMS) * time.Millisecond).String()
	}
	if !http.Complete {
		summary += " (incomplete)"
	}
	// Late means the daemon recorded the exchange after the producer stamped it,
	// so its neighbours on this line are not evidence of causal order.
	if http.Late {
		summary += " (recorded after the fact)"
	}
	fmt.Fprintf(a.Stdout, "%-13s %-10s %s\n", stamp, "http", summary)

	detail := fmt.Sprintf("%s; %s; payloads: %s",
		orDash(http.CallSite), orDash(http.Phase), orDash(http.Payloads))
	fmt.Fprintf(a.Stdout, "%-13s %-10s   %s\n", "", "", detail)
	if http.Payloads != "" && http.Payloads != "metadata" {
		fmt.Fprintf(a.Stdout, "%-13s %-10s   %s\n", "", "",
			shellCommand("otter", "request", runID, http.RequestID))
	}
}

func (a *App) printTraceFooter(page *timeline.Page, runID string, noHTTP bool) {
	if !page.HasMore || page.NextCursor == "" {
		return
	}
	// The next command repeats the output mode and --no-http, because a cursor
	// issued under one setting is rejected under the other.
	fmt.Fprintf(a.Stdout, "\nmore events may exist; continue with:\n  %s\n",
		shellCommand("otter", traceArgs(runID, page.NextCursor, noHTTP)...))
}

// traceArgs builds the continuation command's arguments. Every one of them is
// echoed through shellCommand, so a hostile run id or cursor cannot inject a
// second command.
func traceArgs(runID, cursor string, noHTTP bool) []string {
	args := []string{"trace", runID, "--after", cursor}
	if noHTTP {
		args = append(args, "--no-http")
	}
	return args
}

// ---------------------------------------------------------------- JSONL form

// traceRecord is one line of machine-readable `otter trace` output.
//
// JSONL is a typed stream rather than a bare event list: the first record is the
// context and the last is the page framing, so a consumer that receives an empty
// page still learns the run's status, whether capture was on, and how to ask for
// more. Human warnings never appear here.
type traceRecord struct {
	Type          string `json:"type"`
	SchemaVersion int    `json:"schema_version,omitempty"`

	Context *timeline.Context `json:"context,omitempty"`
	Event   *timeline.Event   `json:"event,omitempty"`

	HasMore    *bool  `json:"has_more,omitempty"`
	NextCursor string `json:"next_cursor,omitempty"`
	SnapshotAt string `json:"snapshot_at,omitempty"`
}

func (a *App) printTraceJSONL(page *timeline.Page, runID string, noHTTP bool) int {
	ctx := page.Context
	if err := a.writeTraceRecord(traceRecord{
		Type:          "context",
		SchemaVersion: timeline.SchemaVersion,
		Context:       &ctx,
	}); err != nil {
		return a.fail(err)
	}

	for i := range page.Events {
		event := page.Events[i]
		if err := a.writeTraceRecord(traceRecord{Type: "event", Event: &event}); err != nil {
			return a.fail(err)
		}
	}

	hasMore := page.HasMore
	record := traceRecord{
		Type:       "page",
		HasMore:    &hasMore,
		NextCursor: page.NextCursor,
		SnapshotAt: page.SnapshotAt.Format(time.RFC3339Nano),
	}
	if err := a.writeTraceRecord(record); err != nil {
		return a.fail(err)
	}
	return 0
}

func (a *App) writeTraceRecord(record traceRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode trace record: %w", err)
	}
	if _, err := fmt.Fprintln(a.Stdout, string(encoded)); err != nil {
		return fmt.Errorf("write trace record: %w", err)
	}
	return nil
}

// shellCommand renders a command so it can be copied and pasted. Every argument
// is quoted when it needs it: terminal escaping makes a value safe to *print*,
// but a request id or cursor with a shell metacharacter would still be a second
// command if it were pasted unquoted.
func shellCommand(name string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, name)
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes an argument for a POSIX shell when it is not already safe.
func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`!&|;<>()*?[]{}#~=%") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
