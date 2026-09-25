package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	width := fs.Int("width", 0, "column width to render for (default: a fixed width)")
	pretty := fs.Bool("pretty", false, "human-readable output even when piped (overrides --json)")
	noColor := fs.Bool("no-color", false, "never color the output (also honours NO_COLOR)")
	follow := fs.Bool("follow", false, "not supported: a trace covers a finished attempt")
	takesValue := func(arg string) bool {
		name := strings.TrimLeft(arg, "-")
		if i := strings.Index(name, "="); i >= 0 {
			name = name[:i]
		}
		return name == "limit" || name == "after" || name == "width"
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

	// The width and color only affect the human form; JSONL is never elided or
	// colored, because its consumer is a program.
	a.traceWidthOverride = *width
	a.traceNoColor = *noColor

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
		refs := a.printTraceTable(page)
		a.printTraceFootnotes(refs, page.Context.RunID)
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

func (a *App) printTraceTable(page *timeline.Page) []httpRef {
	// A trace that spans more than one calendar day would be ambiguous with a
	// time-only column, so the date is added before the time.
	first := page.Events[0].At
	last := page.Events[len(page.Events)-1].At
	withDate := first.Year() != last.Year() || first.YearDay() != last.YearDay() ||
		(first.Hour() == 0 && first.Minute() == 0 && first.Second() == 0)
	timeWidth := traceTimeWidth
	if withDate {
		timeWidth = dateWidth + 1 + traceTimeWidth
	}

	t := table{timeWidth: timeWidth, color: a.colorEnabled(), cols: a.traceWidth()}
	t.line(a.Stdout, "TIME", "", "KIND", "DETAIL")

	// Every line after the first indents to where the detail text begins, which is
	// what makes a wrapped URL or a stack trace read as one cell.
	cont := strings.Repeat(" ", t.detailCol())
	available := a.traceWidth() - t.detailCol()

	var refs []httpRef
	for _, event := range page.Events {
		stamp := event.At.UTC().Format("15:04:05")
		if withDate {
			stamp = event.At.UTC().Format("2006-01-02 15:04:05")
		}
		switch event.Kind {
		case timeline.KindHTTP:
			if ref, ok := a.printTraceHTTP(t, stamp, event, cont, available, len(refs)+1); ok {
				refs = append(refs, ref)
			}
		case timeline.KindLifecycle:
			t.block(a.Stdout, stamp, traceGlyph(event), timeline.KindLifecycle, event.Message, cont)
		default:
			// The KIND column shows the event kind, never the stream name. The
			// `otter` stream carries both the runtime's narration and an
			// integration's ctx.log output, so printing it there would present
			// the integration's own line as if it were the runtime talking.
			//
			// The stored line is shown as it was written, one row per event. A
			// ctx.log line carries its structured fields as a JSON suffix, and
			// splitting that out read worse: it stranded a short field alone on a
			// line above a wrapped object. Keeping the line intact keeps one event
			// on one row, which is what the timeline is for.
			message := event.Message
			if event.Stream == runs.StreamStderr {
				message = "[stderr] " + message
			}
			t.block(a.Stdout, stamp, traceGlyph(event), timeline.KindLog, message, cont)
		}
	}
	return refs
}

// Trace table geometry. Every row, continuation line and the header come from
// these through table, rather than from literal widths repeated per call site:
// the earlier version built columns in two places and they disagreed.
const (
	// traceTimeWidth is hh:mm:ss. The date widens the column only for a trace
	// that spans more than one day.
	traceTimeWidth = len("15:04:05")
	dateWidth      = len("2006-01-02")
	// traceGlyphWidth is one symbol plus the gap after it.
	traceGlyphWidth = 2
	// traceKindWidth is the widest kind, "lifecycle".
	traceKindWidth = len("lifecycle")
	// gap separates the fixed fields from each other and from the detail.
	gap = 2
)

// table owns column geometry so a header, a row and a continuation line cannot
// disagree about where the detail column is.
//
// color is decided once, before any row is written, so a page cannot be half
// colored. Color is applied to the glyph after the field is padded, never before:
// an escape sequence counts toward byte length but occupies no column, and
// measuring a colored string is how a table drifts out of alignment.
type table struct {
	timeWidth int
	color     bool
	// cols is the column width the table is rendered for, so a value that is
	// allowed to wrap can be broken at the right place.
	cols int
}

// rowDetail renders the fixed columns for one row, ending just before the detail
// text. The time and kind fields are padded and truncated to a fixed width, so an
// unexpectedly long value cannot shift the detail column for one row only.
func rowDetail(timeWidth int, time, glyph, kind string) string {
	return padEnd(time, timeWidth) +
		strings.Repeat(" ", gap) +
		padEnd(glyph, traceGlyphWidth) +
		strings.Repeat(" ", gap) +
		padEnd(kind, traceKindWidth)
}

// detailCol is the zero-based column where detail text starts.
func (t table) detailCol() int {
	return t.timeWidth + gap + traceGlyphWidth + gap + traceKindWidth + gap
}

// fields renders the fixed columns. The time and kind fields are padded and
// truncated to a fixed width, so an unexpectedly long kind cannot shift the
// detail column for one row and not the others.
func (t table) fields(time, glyph, kind string) string {
	return padEnd(time, t.timeWidth) +
		strings.Repeat(" ", gap) +
		t.colorGlyph(glyph) +
		strings.Repeat(" ", gap) +
		padEnd(kind, traceKindWidth) +
		strings.Repeat(" ", gap)
}

// colorGlyph pads the glyph to its column and then colors it, so the escape
// sequences are never part of what the padding measures.
func (t table) colorGlyph(glyph string) string {
	padded := padEnd(glyph, traceGlyphWidth)
	if !t.color {
		return padded
	}
	switch glyph {
	case glyphOK:
		return ansiGreen + glyph + ansiReset + strings.Repeat(" ", traceGlyphWidth-1)
	case glyphBroken:
		return ansiRed + glyph + ansiReset + strings.Repeat(" ", traceGlyphWidth-1)
	case glyphWarn:
		return ansiYellow + glyph + ansiReset + strings.Repeat(" ", traceGlyphWidth-1)
	default:
		return ansiDim + padded + ansiReset
	}
}

// line writes one row whose detail is a single value.
func (t table) line(w io.Writer, time, glyph, kind, detail string) {
	fmt.Fprintln(w, t.fields(time, glyph, kind)+detail)
}

// Glyphs. They are named because the color keys off them, and a typo in a string
// literal would silently lose the color rather than fail.
const (
	glyphOK     = "✓"
	glyphBroken = "×"
	glyphWarn   = "!"
	glyphInfo   = "·"
)

// ANSI SGR sequences, used only when the output is a terminal that can show them.
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// colorOutput is the terminal check, as a variable so a test can exercise the
// colored path without a real terminal. It matches the seam `interactiveOutput`
// already uses for the same reason.
var colorOutput = func(w io.Writer) bool { return isTerminal(w) }

// colorEnabled reports whether colored output is wanted. Three conditions have
// to hold, and each exists for a different reason:
//
//   - stdout must be a terminal, or the escapes would end up inside a file or a
//     pipeline that asked for `otter trace | less`.
//   - NO_COLOR must be empty, which is the cross-tool convention for opting out.
//   - TERM must not be "dumb", which is how an environment says it cannot render
//     escapes at all.
func (a *App) colorEnabled() bool {
	if a.traceNoColor {
		return false
	}
	if !colorOutput(a.Stdout) {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return true
}

// block writes a row whose detail may span lines, indenting the remainder to the
// detail column so multi-line output keeps the table's shape.
//
// Only the row's own text is wrapped here. A ctx.log line's structured fields are
// deliberately left inside that text: splitting them out read worse, because a
// short field ended up stranded on a line of its own above a wrapped object.
func (t table) block(w io.Writer, time, glyph, kind, text, cont string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		// A map entry with no prefix is the first line; the rest are indented so
		// the whole event reads as one cell.
		if first := strings.TrimRight(text, "\n"); line == strings.Split(first, "\n")[0] {
			fmt.Fprintln(w, rowDetail(t.timeWidth, time, glyph, kind)+
				strings.Repeat(" ", gap)+escapeTerminal(line))
			continue
		}
		fmt.Fprintln(w, cont+escapeTerminal(line))
	}
}

// padEnd pads a field to an exact column width.
//
// It counts runes, not bytes: the glyph column holds characters like "·" that are
// two bytes but occupy one column, and padding by byte length shifted every
// column after the glyph. A rune run wider than its field is truncated rather
// than allowed to push the columns, which is why the field width is authoritative.
func padEnd(value string, width int) string {
	runes := []rune(value)
	if len(runes) >= width {
		return string(runes[:width])
	}
	return value + strings.Repeat(" ", width-len(runes))
}

// traceGlyph marks an event's own outcome. It is deliberately not a claim about
// the run: a handled 400 inside a successful run is still marked, because the
// column describes what the call did, not what the run concluded.
func traceGlyph(e timeline.Event) string {
	switch e.Kind {
	case timeline.KindHTTP:
		http := e.HTTP
		if http == nil {
			return glyphInfo
		}
		switch {
		case http.ErrorClass != "":
			// A transport failure never reached a status.
			return glyphBroken
		case !http.Complete || http.Phase == "in_progress":
			// We do not know how it ended: unresolved, but not proven broken.
			return glyphWarn
		case http.StatusCode == nil:
			return glyphInfo
		case *http.StatusCode < 300:
			return glyphOK
		case *http.StatusCode < 400:
			return glyphInfo
		case *http.StatusCode < 500:
			// Rejected, not broken. The service answered.
			return glyphWarn
		default:
			return glyphBroken
		}
	case timeline.KindLifecycle:
		message := strings.ToLower(e.Message)
		switch {
		case strings.Contains(message, "run succeeded"):
			return glyphOK
		case strings.Contains(message, "run failed"),
			strings.Contains(message, "run cancelled"),
			strings.Contains(message, "timed_out"),
			strings.Contains(message, "timed out"),
			strings.Contains(message, "marked failed"):
			return glyphBroken
		default:
			// Queued and started have not succeeded at anything yet.
			return glyphInfo
		}
	default:
		return glyphInfo
	}
}

// httpRef is one exchange's reference into the footnotes. The trace table stays
// one line per event; everything that would have been a second and third line
// under a row is collected here and printed once, at the end, in the order the
// exchanges appear.
type httpRef struct {
	number    int
	requestID string
	callSite  string
	// phase and payloads are carried so the footer can state the norm once
	// instead of repeating it on every entry.
	phase    string
	payloads string
	// errorCode and errorText let a footnote restate the reason, which is what a
	// reader wants next to the command that opens the payloads.
	errorCode string
	errorText string
}

// printTraceHTTP writes one exchange as a single row and returns its footnote
// reference.
//
// The row carries what is needed to find the exchange; the call site and the
// payload command move to the footnotes. `completed` and `payloads: partial` are
// not repeated per exchange either: they are the common case, and the footer
// states them once so that an exception stands out.
func (a *App) printTraceHTTP(t table, stamp string, event timeline.Event, cont string, available, number int) (httpRef, bool) {
	http := event.HTTP
	if http == nil {
		return httpRef{}, false
	}

	summary := fmt.Sprintf("%s %s", orDash(http.Method), shortURL(http.URL))
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

	ref := httpRef{
		number:    number,
		requestID: http.RequestID,
		callSite:  orDash(http.CallSite),
		phase:     orDash(http.Phase),
		payloads:  orDash(http.Payloads),
		errorCode: http.ErrorCode,
		errorText: http.ErrorMessage,
	}
	t.line(a.Stdout, stamp, traceGlyph(event), "http",
		elide(summary, available)+"  ["+strconv.Itoa(number)+"]")

	// The reason a failed exchange failed, on the line under it. The status code
	// says a call was rejected; only this says why, and reaching for another
	// command to find out is the thing the trace exists to avoid.
	if reason := errorReason(http); reason != "" {
		for _, line := range wrapAt(reason, t.cols-t.detailCol()) {
			fmt.Fprintln(a.Stdout, cont+line)
		}
	}
	return ref, true
}

// errorReason renders an exchange's failure reason, or nothing when there is
// none to show.
//
// A failed exchange with no summary says so. That is not the same as a response
// with no error: under `capture: metadata` there is no body to read, and a
// silent omission would read as "the server explained nothing".
func errorReason(http *timeline.HTTPEvent) string {
	if http.StatusCode != nil && *http.StatusCode < 400 && http.ErrorClass == "" {
		return ""
	}
	code := escapeTerminal(http.ErrorCode)
	message := escapeTerminal(http.ErrorMessage)
	switch {
	case code != "" && message != "":
		return code + ": " + message
	case message != "":
		return message
	case code != "":
		return code
	case http.StatusCode != nil && *http.StatusCode >= 400:
		return "no error message captured"
	default:
		return ""
	}
}

// wrapAt breaks a value into lines no longer than width, at spaces, so a
// sentence can span lines without being elided. A non-positive width leaves it on
// one line. Only spaces are folded: collapsing them on the first line would
// change the text, and the caller has already flattened and bounded whatever it
// is wrapping.
func wrapAt(value string, width int) []string {
	if width <= 0 || len([]rune(value)) <= width {
		return []string{value}
	}
	var (
		lines   []string
		current string
	)
	for _, word := range strings.Split(value, " ") {
		switch {
		case current == "":
			current = word
		case len([]rune(current))+1+len([]rune(word)) <= width:
			current += " " + word
		default:
			lines = append(lines, current)
			current = word
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	if len(lines) == 0 {
		return []string{value}
	}
	return lines
}

// printTraceFootnotes writes the collected references.
//
// Everything is indented to the detail column, so a footnote entry starts in the
// same column as a row's detail text and the eye can run straight down.
//
// When every exchange agrees on phase and payload coverage, that is stated once
// and each entry carries only what is specific to it. Every entry prints a
// complete command even though the entries differ only in the request id: a
// command a reader is meant to run has to be runnable, so it is written out in
// full rather than abbreviated, and the ids are padded so the varying part lines
// up and the repetition is at least scannable.
func (a *App) printTraceFootnotes(refs []httpRef, runID string) {
	if len(refs) == 0 {
		return
	}
	prefix := strings.Repeat(" ", detailCol(traceTimeWidth))

	// Align the ids when they are all the same length, which is the common case.
	// Padding is applied to the quoted form, so alignment never costs safety: a
	// request id is SDK-supplied and goes through the same shell quoting as any
	// other argument.
	quotedIDs := make([]string, len(refs))
	for i, ref := range refs {
		quotedIDs[i] = shellQuote(ref.requestID)
	}
	idWidth := len(quotedIDs[0])
	for _, id := range quotedIDs {
		if len(id) != idWidth {
			idWidth = 0
			break
		}
	}

	var b strings.Builder
	b.WriteString("\n")
	if refsAgree(refs) {
		fmt.Fprintf(&b, "%sall %d exchanges: %s, payloads %s\n",
			prefix, len(refs), refs[0].phase, refs[0].payloads)
	}

	for i, ref := range refs {
		b.WriteString(refMarker(i+1, ref.number) + " " + shortCallSite(ref.callSite))
		if !refsAgree(refs) {
			fmt.Fprintf(&b, "; %s; payloads %s", ref.phase, ref.payloads)
		}
		b.WriteString("\n")

		if idWidth > 0 {
			fmt.Fprintf(&b, "%sotter request %s %-*s\n", prefix, shellQuote(runID), idWidth, quotedIDs[i])
			continue
		}
		b.WriteString(prefix + shellCommand("otter", "request", runID, ref.requestID) + "\n")
	}
	fmt.Fprint(a.Stdout, b.String())
}

// refMarker renders a reference so that its number aligns whether it has one
// digit or several: "1. " and "10." both put the call site in the same column.
func refMarker(position, number int) string {
	digits := len(strconv.Itoa(number))
	return "└" + padEnd(strconv.Itoa(number)+".", digits+2)
}

// refsAgree reports whether every exchange shares one phase and payload state.
func refsAgree(refs []httpRef) bool {
	for _, ref := range refs[1:] {
		if ref.phase != refs[0].phase || ref.payloads != refs[0].payloads {
			return false
		}
	}
	return true
}

// footnoteReason restates a failure reason for a footnote entry, or nothing when
// there is none. The code alone is enough here: the row above already carries the
// full text, and repeating a sentence in every footnote is the noise the
// footnotes exist to remove.
func footnoteReason(ref httpRef) string {
	if ref.errorCode != "" {
		return "(" + escapeTerminal(ref.errorCode) + ")"
	}
	if ref.errorText != "" {
		return "(" + escapeTerminal(ref.errorText) + ")"
	}
	return ""
}

// shortCallSite drops the " in function" tail the capture records after a
// location. "salesforce.py:105" is what a reader matches against a file;
// the function name rides along only as noise in a footnote.
func shortCallSite(site string) string {
	if i := strings.Index(site, " in "); i > 0 {
		return site[:i]
	}
	return site
}

// shortURL renders a URL without the scheme a reader already assumes.
//
// https:// is dropped because it is the overwhelming case and it costs eight
// characters of column on every row. http:// is kept: plaintext HTTP is the one
// case where the scheme carries information, and hiding it would hide exactly the
// thing worth noticing.
func shortURL(raw string) string {
	escaped := escapeTerminal(raw)
	if rest, ok := strings.CutPrefix(escaped, "https://"); ok {
		return rest
	}
	return escaped
}

// detailCol is the zero-based column where detail text starts. It is the shared
// answer for the table and for anything printed beneath it, so a footnote prefix
// and a row's detail column cannot disagree.
func detailCol(timeWidth int) int {
	return timeWidth + gap + traceGlyphWidth + gap + traceKindWidth + gap
}

// elide shortens a value that will not fit the detail column, keeping both ends:
// the host and the path's leaf are what a reader recognises, and the middle of a
// long URL is the part they do not need. A non-positive width disables eliding.
func elide(value string, width int) string {
	const marker = "…"
	if width <= 0 || len(value) <= width {
		return value
	}
	keep := width - len(marker)
	if keep < 1 {
		return marker
	}
	head := keep / 2
	tail := keep - head
	return value[:head] + marker + value[len(value)-tail:]
}

// traceWidth is how many columns the table may occupy.
//
// It is a fixed width rather than the terminal's: measuring a terminal needs a
// platform syscall or a new dependency, and a predictable default beats a wrong
// guess. --width is the escape hatch for a terminal that differs.
func (a *App) traceWidth() int {
	if a.traceWidthOverride > 0 {
		return a.traceWidthOverride
	}
	return defaultTraceWidth
}

// defaultTraceWidth fits a typical terminal without wrapping.
const defaultTraceWidth = 100

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
