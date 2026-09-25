package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/timeline"
)

// TestRunTimelineEndToEnd is the acceptance test for `otter trace`: after the
// same failed JSON POST the capture milestone uses, one merged read must show
// the lifecycle, the captured exchange with its call site, and the exception
// output — with no logging added to the integration.
func TestRunTimelineEndToEnd(t *testing.T) {
	requirePython(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cursor":
			writeJSON(t, w, http.StatusOK, map[string]any{"cursor": "cur-42", "password": "origin-secret-value"})
		case "/records":
			io.Copy(io.Discard, r.Body)
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"error": "cursor rejected"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()

	root := t.TempDir()
	writeIntegration(t, root, "trace-demo", fmt.Sprintf(`
version: 1
name: trace-demo
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
env:
  ORIGIN_URL: %s
`, origin.URL), captureFixtureSource)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "trace-demo",
		api.TriggerPayload{Type: api.TriggerManual, Body: json.RawMessage(`{"customer_id":123}`)},
		api.SubmitRunOptions{Capture: inspection.PolicyFull})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusFailed {
		t.Fatalf("run status = %s, want failed", view.Run.Status)
	}

	page := timelineOf(t, d, runID, timeline.Request{RunID: runID, IncludeHTTP: true, Limit: 100})

	// Context: status and error come from the run record, so they survive even a
	// pruned log. The trigger type is named without its payload.
	if page.Context.Status != string(runs.StatusFailed) {
		t.Errorf("status = %q, want failed", page.Context.Status)
	}
	if page.Context.IntegrationName != "trace-demo" {
		t.Errorf("integration name = %q, want trace-demo", page.Context.IntegrationName)
	}
	if page.Context.TriggerType != string(runs.TriggerManual) {
		t.Errorf("trigger = %q, want manual", page.Context.TriggerType)
	}
	if page.Context.Capture == nil || page.Context.Capture.Policy != inspection.PolicyFull {
		t.Errorf("capture policy = %+v, want full", page.Context.Capture)
	}
	// The trigger body is deliberately absent: it is stored unsanitized, so the
	// timeline must not carry it.
	if raw, err := json.Marshal(page); err != nil {
		t.Fatalf("marshal page: %v", err)
	} else if strings.Contains(string(raw), "customer_id") {
		t.Errorf("the timeline carried the raw trigger body: %s", raw)
	}

	// The merged order must contain the whole story: lifecycle, both exchanges,
	// and the exception on stderr.
	var (
		sawQueued, sawStarted, sawFailed bool
		sawStderr                        bool
		exchanges                        []timeline.Event
	)
	for _, event := range page.Events {
		switch {
		case event.Kind == timeline.KindHTTP:
			exchanges = append(exchanges, event)
		case event.Kind == timeline.KindLifecycle:
			switch {
			case strings.Contains(event.Message, "run queued"):
				sawQueued = true
			case strings.Contains(event.Message, "run started"):
				sawStarted = true
			case strings.Contains(event.Message, "run failed"):
				sawFailed = true
			}
		case event.Kind == timeline.KindLog && event.Stream == runs.StreamStderr:
			if strings.Contains(event.Message, "cursor rejected") {
				sawStderr = true
			}
		}
	}
	if !sawQueued || !sawStarted || !sawFailed {
		t.Errorf("lifecycle is incomplete: queued=%v started=%v failed=%v", sawQueued, sawStarted, sawFailed)
	}
	if !sawStderr {
		t.Errorf("the exception output is missing from the trace: %+v", page.Events)
	}
	if len(exchanges) != 2 {
		t.Fatalf("timeline shows %d exchanges, want 2", len(exchanges))
	}

	// The failing POST is identifiable and points at the code that sent it.
	var post *timeline.HTTPEvent
	for _, event := range exchanges {
		if event.HTTP.Method == http.MethodPost {
			post = event.HTTP
		}
	}
	if post == nil {
		t.Fatalf("the POST is missing from the trace")
	}
	if post.StatusCode == nil || *post.StatusCode != 400 {
		t.Errorf("POST status = %v, want 400", post.StatusCode)
	}
	if post.CallSite == "" {
		t.Errorf("the exchange carries no call site, so nothing points at the code that sent it")
	}
	if post.Payloads != "full" {
		t.Errorf("payloads = %q, want full", post.Payloads)
	}

	// The detail command the trace prints must actually retrieve the sanitized
	// bodies, which is the whole reason to reach for it.
	exchange, err := d.GetCaptureRequest(context.Background(), runID, post.RequestID)
	if err != nil {
		t.Fatalf("get captured request: %v", err)
	}
	if exchange.RequestBody == nil || exchange.RequestBody.State != inspection.BodyCaptured {
		t.Fatalf("request body was not captured: %+v", exchange.RequestBody)
	}
	if body := string(exchange.RequestBody.JSON); !strings.Contains(body, "cur-42") {
		t.Errorf("request body = %s, want the transformed payload", body)
	}
	if exchange.ResponseBody == nil || !strings.Contains(string(exchange.ResponseBody.JSON), "cursor rejected") {
		t.Errorf("response body = %+v, want the upstream error", exchange.ResponseBody)
	}
}

// TestRunTimelinePagingIsStableAcrossPageSizes walks the same trace with every
// page size and requires byte-identical event sequences. It is the property that
// makes a continuation safe to trust.
func TestRunTimelinePagingIsStableAcrossPageSizes(t *testing.T) {
	requirePython(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cursor":
			writeJSON(t, w, http.StatusOK, map[string]any{"cursor": "cur-42", "password": "x"})
		default:
			io.Copy(io.Discard, r.Body)
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"error": "cursor rejected"})
		}
	}))
	defer origin.Close()

	root := t.TempDir()
	writeIntegration(t, root, "trace-paging", fmt.Sprintf(`
version: 1
name: trace-paging
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
env:
  ORIGIN_URL: %s
`, origin.URL), captureFixtureSource)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "trace-paging",
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{Capture: inspection.PolicyFull})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	awaitTerminal(t, d, runID)

	// One page of everything is the reference sequence.
	reference := keysOf(timelineOf(t, d, runID, timeline.Request{RunID: runID, IncludeHTTP: true, Limit: 100}).Events)
	if len(reference) < 4 {
		t.Fatalf("the reference trace has only %d events; the scenario is too small to page", len(reference))
	}

	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			var (
				collected []string
				cursor    string
				pages     int
			)
			for {
				page := timelineOf(t, d, runID, timeline.Request{
					RunID: runID, IncludeHTTP: true, Limit: limit, After: cursor,
				})
				pages++
				if pages > 100 {
					t.Fatalf("pagination did not terminate")
				}
				collected = append(collected, keysOf(page.Events)...)
				if !page.HasMore {
					if page.NextCursor != "" {
						t.Errorf("a final page carried a cursor")
					}
					break
				}
				if page.NextCursor == "" {
					t.Fatalf("has_more with no cursor")
				}
				cursor = page.NextCursor
			}

			if len(collected) != len(reference) {
				t.Fatalf("paged %d events, want %d\npaged=%v\nref  =%v",
					len(collected), len(reference), collected, reference)
			}
			for i := range reference {
				if collected[i] != reference[i] {
					t.Fatalf("paged sequence differs at %d\npaged=%v\nref  =%v",
						i, collected, reference)
				}
			}
		})
	}
}

// TestRunTimelineRefusesNonTerminalAttempts checks the daemon-side rule that a
// trace is only defined for a finished attempt.
func TestRunTimelineRefusesNonTerminalAttempts(t *testing.T) {
	requirePython(t)

	// A run that blocks gives a window in which the attempt is nonterminal.
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeJSON(t, w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer origin.Close()
	// Unblock the handler on every exit path: a test failure while the request
	// is parked would otherwise leave the server shutdown waiting on it.
	defer unblock()

	root := t.TempDir()
	writeIntegration(t, root, "trace-blocking", fmt.Sprintf(`
version: 1
name: trace-blocking
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
env:
  ORIGIN_URL: %s
`, origin.URL), `
import os
import urllib.request

from otter import Context

ctx = Context.from_environment()
urllib.request.urlopen(os.environ["ORIGIN_URL"] + "/slow").read()
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "trace-blocking",
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{Capture: inspection.PolicyFull})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	// Wait until the attempt is visibly running, then demand a trace.
	awaitStatus(t, d, runID, runs.StatusRunning)

	_, err = d.TimelinePage(context.Background(), timeline.Request{RunID: runID, IncludeHTTP: true, Limit: 10})
	if !errors.Is(err, timeline.ErrRunNotTerminal) {
		t.Fatalf("timeline error = %v, want ErrRunNotTerminal", err)
	}

	unblock()
	awaitTerminal(t, d, runID)

	// Once finished, the same read succeeds.
	page := timelineOf(t, d, runID, timeline.Request{RunID: runID, IncludeHTTP: true, Limit: 100})
	if page.Context.Status == "" {
		t.Errorf("a finished attempt must be traceable")
	}
}

// TestRunTimelineWithoutCaptureStillTells the story covers a run that made no
// HTTP call: the failure must still be explainable.
func TestRunTimelineWithoutCaptureStillTellsTheStory(t *testing.T) {
	requirePython(t)

	root := t.TempDir()
	writeIntegration(t, root, "trace-nocall", `
version: 1
name: trace-nocall
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
`, `
raise RuntimeError("nothing to send, configuration is missing")
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "trace-nocall",
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{Capture: inspection.PolicyFull})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusFailed {
		t.Fatalf("run status = %s, want failed", view.Run.Status)
	}

	page := timelineOf(t, d, runID, timeline.Request{RunID: runID, IncludeHTTP: true, Limit: 100})
	if page.Context.Status != string(runs.StatusFailed) {
		t.Errorf("status = %q, want failed", page.Context.Status)
	}
	// Capture was enabled and observed nothing, which is a different statement
	// from "capture was never configured".
	if page.Context.Capture == nil || page.Context.Capture.State != inspection.CaptureComplete {
		t.Errorf("capture = %+v, want complete with no requests", page.Context.Capture)
	}
	if page.Context.Capture != nil && page.Context.Capture.RequestCount != 0 {
		t.Errorf("request count = %d, want 0", page.Context.Capture.RequestCount)
	}

	var sawFailure bool
	for _, event := range page.Events {
		if event.Kind == timeline.KindLifecycle && strings.Contains(event.Message, "run failed") {
			sawFailure = true
		}
		if event.Kind == timeline.KindHTTP {
			t.Errorf("a run that made no request must show no exchange: %+v", event)
		}
	}
	if !sawFailure {
		t.Errorf("the terminal lifecycle line is missing: %+v", page.Events)
	}
}

// TestRunTimelineSeparatesCtxLogFromNarration is the end-to-end form of the
// classification bug: an integration's ctx.log line and the runtime's narration
// share the `otter` stream, so a trace must still tell them apart.
func TestRunTimelineSeparatesCtxLogFromNarration(t *testing.T) {
	requirePython(t)

	root := t.TempDir()
	writeIntegration(t, root, "trace-ctxlog", `
version: 1
name: trace-ctxlog
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
`, `
from otter import Context

ctx = Context.from_environment()
ctx.log.info("sync starting", object="Contact", page_size=100)
raise RuntimeError("boom")
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "trace-ctxlog",
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{Capture: inspection.PolicyFull})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	awaitTerminal(t, d, runID)

	page := timelineOf(t, d, runID, timeline.Request{RunID: runID, IncludeHTTP: true, Limit: 100})

	var (
		sawCtxLogAsLog     bool
		sawNarration       bool
		ctxLogOnOtterStrem bool
	)
	for _, event := range page.Events {
		if event.Stream != runs.StreamOtter {
			continue
		}
		if strings.Contains(event.Message, "sync starting") {
			ctxLogOnOtterStrem = true
			if event.Kind != timeline.KindLog {
				t.Errorf("a ctx.log line was classified as %q, want %q", event.Kind, timeline.KindLog)
			}
			sawCtxLogAsLog = true
			// The stored line keeps the SDK's fields, including the marker that
			// says which writer produced it.
			if !strings.Contains(event.Message, `"logger":"otter"`) {
				t.Errorf("the ctx.log line lost its writer marker: %q", event.Message)
			}
		}
		if strings.Contains(event.Message, "run started") {
			sawNarration = true
			if event.Kind != timeline.KindLifecycle {
				t.Errorf("narration was classified as %q, want %q", event.Kind, timeline.KindLifecycle)
			}
		}
	}
	if !ctxLogOnOtterStrem {
		t.Fatalf("the ctx.log line never reached the otter stream: %+v", page.Events)
	}
	if !sawCtxLogAsLog {
		t.Errorf("the ctx.log line is missing from the trace")
	}
	if !sawNarration {
		t.Errorf("the runtime's own narration is missing from the trace")
	}
}

// TestRunTimelineOverHTTP goes through the running daemon's HTTP API rather than
// calling the backend method, so the route, the query contract, the client and
// the merge are all exercised together — the path `otter trace` actually uses.
func TestRunTimelineOverHTTP(t *testing.T) {
	requirePython(t)

	root := t.TempDir()
	writeIntegration(t, root, "trace-http", `
version: 1
name: trace-http
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
`, `
from otter import Context

ctx = Context.from_environment()
ctx.log.info("about to fail")
raise RuntimeError("boom")
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "trace-http",
		api.TriggerPayload{Type: api.TriggerManual}, api.SubmitRunOptions{Capture: inspection.PolicyFull})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	awaitTerminal(t, d, runID)

	client := api.NewClient("http://"+d.cfg.Listen, "")

	page, err := client.Timeline(context.Background(), timeline.Request{
		RunID: runID, IncludeHTTP: true, Limit: 2,
	})
	if err != nil {
		t.Fatalf("timeline over HTTP: %v", err)
	}
	if page.Context.RunID != runID || page.Context.Status != string(runs.StatusFailed) {
		t.Fatalf("context = %+v, want the failed run", page.Context)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want the requested 2", len(page.Events))
	}
	if !page.HasMore || page.NextCursor == "" {
		t.Fatalf("expected a continuation from a 2-event page: %+v", page)
	}

	// The cursor must work over HTTP too, and the context must be repeated.
	next, err := client.Timeline(context.Background(), timeline.Request{
		RunID: runID, IncludeHTTP: true, Limit: 2, After: page.NextCursor,
	})
	if err != nil {
		t.Fatalf("continuation over HTTP: %v", err)
	}
	if len(next.Events) == 0 {
		t.Fatalf("the continuation returned no events")
	}
	if next.Context.SchemaVersion != timeline.SchemaVersion {
		t.Errorf("schema version = %d, want %d", next.Context.SchemaVersion, timeline.SchemaVersion)
	}

	// A cursor that does not belong to this run is rejected by the API.
	if _, err := client.Timeline(context.Background(), timeline.Request{
		RunID: "not-this-run", IncludeHTTP: true, Limit: 2, After: page.NextCursor,
	}); err == nil {
		t.Errorf("a cursor for another run must be rejected")
	}

	// And a nonterminal attempt is a conflict over the wire as well.
	if _, err := client.Timeline(context.Background(), timeline.Request{
		RunID: "no-such-run", IncludeHTTP: true, Limit: 2,
	}); err == nil {
		t.Errorf("an unknown run must be rejected")
	}
}

// ------------------------------------------------------------------ helpers

// timelineOf reads one page and fails the test on error.
func timelineOf(t *testing.T, d *Daemon, runID string, req timeline.Request) *timeline.Page {
	t.Helper()
	req.RunID = runID
	page, err := d.TimelinePage(context.Background(), req)
	if err != nil {
		t.Fatalf("TimelinePage(%s): %v", runID, err)
	}
	return page
}

// keysOf renders just enough of each event to compare sequences.
func keysOf(events []timeline.Event) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		switch {
		case event.Kind == timeline.KindHTTP && event.HTTP != nil:
			out = append(out, "http:"+event.HTTP.RequestID)
		default:
			out = append(out, event.Kind+"@"+event.At.Format("15:04:05.000000000")+":"+event.Message)
		}
	}
	return out
}

// awaitStatus waits until a run reaches the given status, so a test can observe
// an attempt while it is still executing.
func awaitStatus(t *testing.T, d *Daemon, runID string, want runs.Status) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		view, err := d.GetRunDetail(context.Background(), runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if view.Run.Status == want {
			return
		}
		if view.Run.Status.Terminal() {
			t.Fatalf("run finished as %s before reaching %s", view.Run.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run never reached %s", want)
}
