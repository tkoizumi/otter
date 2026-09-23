package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/inspection"
)

// cmdRequests lists the HTTP exchanges captured for one run.
//
// The list never loads bodies, and it always prints the capture state as well:
// an empty list from a run that was never recorded must not look the same as an
// empty list from a run that genuinely made no calls.
func (a *App) cmdRequests(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("requests", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	limit := fs.Int("limit", 100, "maximum number of requests to list")
	afterID := fs.Int64("after-id", 0, "list only requests ingested after this cursor")
	pretty := fs.Bool("pretty", false, "human-readable output even when piped (overrides --json)")
	// Flags may appear after the run id; the reorderer is what makes that work,
	// since flag.Parse stops at the first positional argument.
	takesValue := func(arg string) bool {
		name := strings.TrimLeft(arg, "-")
		if i := strings.Index(name, "="); i >= 0 {
			name = name[:i]
		}
		return name == "limit" || name == "after-id"
	}
	if err := fs.Parse(flagsFirst(normalizeLongFlags(args), takesValue)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Stderr, "otter: usage: otter requests <run-id> [--limit N] [--after-id ID]")
		return 2
	}
	runID := fs.Arg(0)

	response, err := g.client().ListCaptureRequests(ctx, runID, *afterID, *limit)
	if err != nil {
		return a.fail(err)
	}

	if !*pretty && (g.jsonOut || !isTerminal(a.Stdout)) {
		return a.printJSON(response)
	}

	a.printCaptureState(response.Capture)

	if len(response.Requests) == 0 {
		// printCaptureState already explained why there is nothing to show.
		return 0
	}

	writer := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "REQUEST ID\tMETHOD\tSTATUS\tDURATION\tPAYLOADS\tURL")
	for _, request := range response.Requests {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			request.RequestID,
			orDash(request.Method),
			requestStatus(request),
			formatMillis(request.DurationTotalMS),
			request.Completeness(),
			escapeTerminal(request.URL),
		)
	}
	if err := writer.Flush(); err != nil {
		return a.fail(err)
	}

	if next := response.Requests[len(response.Requests)-1].ID; len(response.Requests) == *limit {
		fmt.Fprintf(a.Stderr, "otter: more requests may exist; continue with --after-id %d\n", next)
	}
	return 0
}

// cmdRequest prints one captured exchange, including its sanitized payloads.
//
// The request id may be given on its own or with its run. The short form is the
// common case; the run-and-request form stays because a request id is only
// unique within a run, so naming the run is the only way to resolve an id that
// more than one run recorded.
func (a *App) cmdRequest(ctx context.Context, g globals, args []string) int {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	pretty := fs.Bool("pretty", false, "human-readable output even when piped (overrides --json)")
	// Only --pretty is accepted, and it takes no value; reordering lets it follow
	// the positional arguments.
	if err := fs.Parse(flagsFirst(normalizeLongFlags(args), func(string) bool { return false })); err != nil {
		return 2
	}
	switch fs.NArg() {
	case 1, 2:
	default:
		fmt.Fprintln(a.Stderr, "otter: usage: otter request <request-id>")
		fmt.Fprintln(a.Stderr, "       otter request <run-id> <request-id>")
		return 2
	}

	var (
		response *api.CaptureRequestResponse
		err      error
	)
	if fs.NArg() == 1 {
		requestID := fs.Arg(0)
		response, err = g.client().GetCaptureRequestByID(ctx, requestID)
		if err != nil {
			return a.failRequestLookup(requestID, err)
		}
	} else {
		response, err = g.client().GetCaptureRequest(ctx, fs.Arg(0), fs.Arg(1))
		if err != nil {
			return a.fail(err)
		}
	}

	if !*pretty && (g.jsonOut || !isTerminal(a.Stdout)) {
		return a.printJSON(response)
	}

	request := response.Request
	if request == nil {
		fmt.Fprintln(a.Stderr, "otter: the daemon returned no request")
		return 1
	}

	fmt.Fprintf(a.Stdout, "%s %s\n", orDash(request.Method), escapeTerminal(request.URL))
	if request.FinalURL != "" && request.FinalURL != request.URL {
		fmt.Fprintf(a.Stdout, "  final url: %s\n", escapeTerminal(request.FinalURL))
	}
	fmt.Fprintf(a.Stdout, "  request:   %s\n", request.RequestID)
	fmt.Fprintf(a.Stdout, "  phase:     %s\n", request.Phase)
	if request.StatusCode != nil {
		fmt.Fprintf(a.Stdout, "  status:    %d\n", *request.StatusCode)
	}
	if request.TransportError != "" {
		fmt.Fprintf(a.Stdout, "  error:     %s (%s)\n",
			escapeTerminal(request.TransportError), orDash(request.TransportClass))
	}
	fmt.Fprintf(a.Stdout, "  duration:  %s (headers %s, body %s)\n",
		formatMillis(request.DurationTotalMS),
		formatMillis(request.DurationToHeadersMS),
		formatMillis(request.DurationBodyMS))
	if !request.OccurredAt.IsZero() {
		fmt.Fprintf(a.Stdout, "  occurred:  %s\n", request.OccurredAt.Format(time.RFC3339Nano))
	}
	if request.CallSite != "" {
		fmt.Fprintf(a.Stdout, "  call site: %s\n", escapeTerminal(request.CallSite))
	}
	fmt.Fprintf(a.Stdout, "  payloads:  %s\n", request.Completeness())

	a.printHeaderBlock("request headers", request.RequestHeaders)
	a.printBody("request body", request.RequestBody)
	a.printHeaderBlock("response headers", request.ResponseHeaders)
	a.printBody("response body", request.ResponseBody)

	if response.Capture != nil && response.Capture.State != inspection.CaptureComplete {
		fmt.Fprintf(a.Stdout, "\ncapture: %s\n", captureExplanation(response.Capture))
	}
	return 0
}

// failRequestLookup explains a request-id-only lookup that found nothing.
//
// An unknown single id is most often a run id whose request id was left off, so
// the run's list is offered as the next step rather than a bare "not found".
// Every other failure keeps its usual handling.
func (a *App) failRequestLookup(requestID string, err error) int {
	code := a.fail(err)

	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.IsNotFound() {
		fmt.Fprintf(a.Stderr, "hint: %q is not a recorded request id; if it is a run id, list its requests with 'otter requests %q'\n",
			requestID, requestID)
	}
	return code
}

// ---------------------------------------------------------------- rendering

func (a *App) printCaptureState(capture *inspection.RunCapture) {
	if capture == nil {
		fmt.Fprintln(a.Stderr, "otter: the daemon returned no capture state for this run")
		return
	}
	// The explanation is the point: an empty list is ambiguous without it.
	fmt.Fprintf(a.Stdout, "capture: %s\n", captureExplanation(capture))
	if capture.RequestCount > 0 {
		fmt.Fprintf(a.Stdout, "  requests: %d (%d completed, %d failed, %d incomplete)\n",
			capture.RequestCount, capture.CompletedCount, capture.FailedCount, capture.IncompleteCount)
	}
	if capture.DroppedEvents > 0 {
		fmt.Fprintf(a.Stdout, "  dropped:  %d events (%d bytes) were not recorded\n",
			capture.DroppedEvents, capture.DroppedBytes)
	}
	if capture.RedactionCount > 0 {
		fmt.Fprintf(a.Stdout, "  redacted: %d values were removed before storage\n", capture.RedactionCount)
	}
	if capture.Coverage != "" {
		fmt.Fprintf(a.Stdout, "  coverage: %s only; other clients and raw sockets are not captured\n",
			capture.Coverage)
	}
}

// captureExplanation turns a capture state into a sentence an operator can act
// on. The distinctions matter: "no requests" and "not recorded" are different
// answers, and a zero count must never imply the run made no network calls.
func captureExplanation(capture *inspection.RunCapture) string {
	switch capture.State {
	case inspection.CaptureUnavailable:
		return "unavailable - this run has no recording (it predates HTTP capture, or was submitted with capture off before that was recorded). The run may still have made requests."
	case inspection.CaptureOff:
		return "off - capture was disabled for this run, so outgoing HTTP was not recorded"
	case inspection.CaptureExpired:
		return fmt.Sprintf("expired - the payloads of %d recorded request(s) were removed by retention",
			capture.RequestCount)
	case inspection.CapturePending:
		return "in progress - the run is still executing"
	case inspection.CaptureIncomplete:
		reason := "some capture was lost"
		if capture.DroppedEvents > 0 {
			reason = fmt.Sprintf("%d event(s) were dropped", capture.DroppedEvents)
		}
		if capture.IncompleteCount > 0 {
			reason = fmt.Sprintf("%d request(s) never completed", capture.IncompleteCount)
		}
		return "incomplete - " + reason + "; the list below is not the whole story"
	default:
		if capture.RequestCount == 0 {
			return "complete - capture was enabled and observed no outgoing HTTP requests"
		}
		return "complete"
	}
}

func (a *App) printHeaderBlock(title string, pairs []inspection.HeaderPair) {
	if len(pairs) == 0 {
		return
	}
	fmt.Fprintf(a.Stdout, "\n%s\n", title)
	for _, pair := range pairs {
		fmt.Fprintf(a.Stdout, "  %s: %s\n", escapeTerminal(pair.Name), escapeTerminal(pair.Value))
	}
}

func (a *App) printBody(title string, body *inspection.BodyDescriptor) {
	if body == nil {
		return
	}
	fmt.Fprintf(a.Stdout, "\n%s\n", title)
	switch body.State {
	case inspection.BodyEmpty:
		fmt.Fprintln(a.Stdout, "  (empty)")
	case inspection.BodyCaptured:
		fmt.Fprintf(a.Stdout, "  %s\n", indentJSON(body.JSON))
		if body.Redacted {
			fmt.Fprintf(a.Stdout, "  (%d value(s) redacted before storage)\n", body.RedactedCount)
		}
	default:
		fmt.Fprintf(a.Stdout, "  omitted: %s\n", omissionExplanation(body.Reason, body.ContentType))
	}
}

// omissionExplanation says why a body is not shown, in operator terms.
func omissionExplanation(reason, contentType string) string {
	switch reason {
	case inspection.ReasonUnsupportedContent:
		if contentType != "" {
			return fmt.Sprintf("v1 captures JSON only; this body is %s", escapeTerminal(contentType))
		}
		return "v1 captures JSON only; this body has an unsupported content type"
	case inspection.ReasonStreamUnsupported:
		return "the request body was a stream and could not be inspected without consuming it"
	case inspection.ReasonOversized:
		return "the body exceeded the capture size limit"
	case inspection.ReasonIncomplete:
		return "the application did not read the body to the end, so there is no complete value"
	case inspection.ReasonEncoded:
		return "the body was content-encoded and could not be decoded"
	case inspection.ReasonUnparseable:
		return "the body was not valid JSON"
	case inspection.ReasonRedactionFailed:
		return "the body could not be safely sanitized, so it was not stored"
	case inspection.ReasonQuotaExceeded:
		return "the run's capture quota was reached"
	case inspection.ReasonDropped:
		return "the event was dropped under capture pressure"
	case inspection.ReasonExpired:
		return "the payload was removed by retention"
	default:
		return "not captured"
	}
}

func requestStatus(request inspection.ExchangeSummary) string {
	switch {
	case request.TransportClass != "":
		return "error:" + request.TransportClass
	case request.StatusCode != nil:
		return strconv.Itoa(*request.StatusCode)
	default:
		return "-"
	}
}

func formatMillis(value *int64) string {
	if value == nil {
		return "-"
	}
	return (time.Duration(*value) * time.Millisecond).String()
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return escapeTerminal(value)
}

// indentJSON pretty-prints a captured body. A body that will not indent is
// printed as-is rather than dropped: it came from valid JSON, so this only
// happens if the daemon changed shape.
func indentJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "  ", "  "); err != nil {
		return escapeTerminal(string(raw))
	}
	return buf.String()
}

// escapeTerminal makes a remote string safe to print. Header values, URLs and
// error text are attacker-influenced: without this, a crafted header could move
// the cursor, clear the screen or forge a line of output.
func escapeTerminal(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
