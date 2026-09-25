package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/timeline"
)

// traceAPI serves one timeline page and records the request the CLI made.
func traceAPI(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, req *timeline.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := &timeline.Request{
			RunID:       strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/timeline"),
			After:       r.URL.Query().Get("after"),
			IncludeHTTP: r.URL.Query().Get("include_http") != "false",
		}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				req.Limit = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		respond(w, r, req)
	}))
	t.Cleanup(server.Close)
	return server
}

func tracePage(runID string) *timeline.Page {
	status := 400
	duration := int64(9)
	return &timeline.Page{
		Context: timeline.Context{
			SchemaVersion:   timeline.SchemaVersion,
			RunID:           runID,
			IntegrationID:   "int-1",
			IntegrationName: "orders-sync",
			Status:          "failed",
			Attempt:         1,
			TriggerType:     "manual",
			ReleaseDigest:   "8c1d4f0a9b3e",
			IncludeHTTP:     true,
			Capture: &inspection.RunCapture{
				RunID: runID, State: inspection.CaptureComplete, Policy: inspection.PolicyFull,
				RequestCount: 2, CompletedCount: 2, Coverage: "urllib",
			},
		},
		Events: []timeline.Event{
			{
				Kind: timeline.KindLifecycle, At: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
				Source: timeline.SourceRunLogs, ID: 1, RunID: runID,
				Stream: "otter", Message: "run started (attempt 1 of 1)",
			},
			{
				Kind: timeline.KindHTTP, At: time.Date(2026, 1, 1, 12, 0, 0, 31000000, time.UTC),
				Source: timeline.SourceHTTPExchanges, ID: 1, RunID: runID,
				HTTP: &timeline.HTTPEvent{
					RequestID: "87603b35e60c4dae9f57040b15e24ab3", Method: "POST",
					URL:        "https://api.example.test/v2/records?access_token=REDACTED",
					StatusCode: &status, DurationMS: &duration, Phase: "completed", Complete: true,
					Payloads: "full", CallSite: "main.py:26",
				},
			},
			{
				Kind: timeline.KindLog, At: time.Date(2026, 1, 1, 12, 0, 0, 40000000, time.UTC),
				Source: timeline.SourceRunLogs, ID: 2, RunID: runID,
				Stream: "stderr", Message: "RuntimeError: upstream rejected the batch",
			},
		},
		SnapshotAt: time.Date(2026, 1, 1, 12, 0, 1, 0, time.UTC),
	}
}

// TestTraceTableShowsKindsNotStreamNames is a regression test for a rendering
// bug: the KIND column printed the raw stream name, so an integration's ctx.log
// line appeared as "otter" — the same stream the runtime narrates on. The kind is
// what the column is for, and the stream belongs in the detail.
func TestTraceTableShowsKindsNotStreamNames(t *testing.T) {
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		base := tracePage(runID)
		page := tracePage(runID)
		page.Events = []timeline.Event{
			{Kind: timeline.KindLifecycle, At: base.Events[0].At, Source: timeline.SourceRunLogs,
				ID: 1, RunID: runID, Stream: "otter", Message: "run queued (trigger cron)"},
			{Kind: timeline.KindLog, At: base.Events[1].At, Source: timeline.SourceRunLogs,
				ID: 2, RunID: runID, Stream: "otter",
				Message: `sync starting {"dry_run":false,"level":"info","logger":"otter"}`},
			{Kind: timeline.KindLog, At: base.Events[2].At, Source: timeline.SourceRunLogs,
				ID: 3, RunID: runID, Stream: "stderr", Message: "Traceback (most recent call last):"},
			{Kind: timeline.KindLog, At: base.Events[2].At, Source: timeline.SourceRunLogs,
				ID: 4, RunID: runID, Stream: "stdout", Message: "plain output"},
		}
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, "sync starting") && !strings.Contains(line, "plain output") {
			continue
		}
		// Only the KIND column is under test here: the message itself may
		// legitimately contain the word "otter" (the SDK's logger marker).
		if kind := strings.Fields(line)[1]; kind != timeline.KindLog {
			t.Errorf("KIND column = %q, want %q in: %q", kind, timeline.KindLog, line)
		}
	}
	if !strings.Contains(stdout, "lifecycle  run queued") {
		t.Errorf("narration must still render as lifecycle:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[stderr] Traceback") {
		t.Errorf("stderr should stay identifiable in the detail column:\n%s", stdout)
	}
}

func TestTraceHumanOutputShowsContextAndMergedEvents(t *testing.T) {
	runID := "3f2a91c4-7d18-4a6e-8b21-5c0d9e4a17bb"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		_ = json.NewEncoder(w).Encode(tracePage(runID))
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"run: " + runID,
		"integration: orders-sync",
		"status: failed",
		"release: 8c1d4f0a9b3e",
		"trigger: manual",
		"retry context: otter run-status " + runID,
		"capture: complete",
		"coverage: urllib",
		"run started",
		"POST https://api.example.test/v2/records",
		"400",
		"main.py:26",
		"stderr",
		"RuntimeError: upstream rejected the batch",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output is missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "otter request "+runID+" 87603b35e60c4dae9f57040b15e24ab3") {
		t.Errorf("the exchange must name the command that shows its payloads:\n%s", stdout)
	}
}

func TestTraceJSONLIsATypedStream(t *testing.T) {
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		_ = json.NewEncoder(w).Encode(tracePage(runID))
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 5 {
		t.Fatalf("want context + 3 events + page = 5 lines, got %d:\n%s", len(lines), stdout)
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("context line is not JSON: %v", err)
	}
	if first["type"] != "context" {
		t.Errorf("first record type = %v, want context", first["type"])
	}
	if first["schema_version"] == nil {
		t.Errorf("the context record must carry the schema version")
	}

	for i := 1; i <= 3; i++ {
		var record map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &record); err != nil {
			t.Fatalf("event line %d is not JSON: %v", i, err)
		}
		if record["type"] != "event" || record["event"] == nil {
			t.Errorf("line %d = %v, want an event record", i, record)
		}
	}

	var last map[string]any
	if err := json.Unmarshal([]byte(lines[4]), &last); err != nil {
		t.Fatalf("page line is not JSON: %v", err)
	}
	if last["type"] != "page" {
		t.Errorf("last record type = %v, want page", last["type"])
	}
	if _, ok := last["has_more"]; !ok {
		t.Errorf("the page record must say whether more events exist")
	}
}

func TestTraceEmptyPageStillReportsContext(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage("run-1")
		page.Events = []timeline.Event{}
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, `"type":"context"`) {
		t.Errorf("an empty trace must still emit its context:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"type":"page"`) {
		t.Errorf("an empty trace must still emit its page record:\n%s", stdout)
	}
	if strings.Contains(stdout, `"type":"event"`) {
		t.Errorf("an empty trace must emit no event records:\n%s", stdout)
	}
}

func TestTraceContinuationCommandPreservesMode(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage("run-1")
		page.HasMore = true
		page.NextCursor = "next-cursor"
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--no-http", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "--after next-cursor") {
		t.Errorf("the footer must offer the continuation:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--no-http") {
		t.Errorf("the continuation must repeat --no-http, or the cursor would be rejected:\n%s", stdout)
	}
}

// TestTraceRejectsFollow keeps --follow a usage error independent of the run.
func TestTraceRejectsFollow(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		t.Errorf("a rejected invocation must not reach the API")
	})

	code, _, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--follow")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--follow") {
		t.Errorf("stderr should explain that following is unsupported:\n%s", stderr)
	}
}

func TestTraceRejectsOutOfRangeLimit(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {})

	for _, raw := range []string{"0", "1001", "-3"} {
		code, _, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--limit", raw)
		if code != 2 {
			t.Fatalf("limit %s: exit = %d, want 2", raw, code)
		}
		if !strings.Contains(stderr, "--limit") {
			t.Errorf("limit %s: stderr should name the flag:\n%s", raw, stderr)
		}
	}
}

// TestTraceNonTerminalHintsAreStatusSpecific checks the three hints, because
// each status needs different advice and a retry has no output to follow.
func TestTraceNonTerminalHintsAreStatusSpecific(t *testing.T) {
	cases := []struct {
		status  string
		want    []string
		notWant string
	}{
		{
			status: "running",
			want:   []string{"--follow", "otter requests"},
		},
		{
			status: "retrying",
			want:   []string{"waiting to execute", "run-status", "parent"},
			// Following a retry's logs would show nothing: retry creation writes
			// its scheduling line to the previous attempt.
			notWant: "--follow",
		},
		{
			status: "queued",
			want:   []string{"waiting for execution", "otter status", "run-status"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"run has not finished: status is ` +
					tc.status + `"}}`))
			})

			code, _, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--pretty")
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("hint is missing %q:\n%s", want, stderr)
				}
			}
			if tc.notWant != "" && strings.Contains(stderr, tc.notWant) {
				t.Errorf("hint should not suggest %q for a %s attempt:\n%s", tc.notWant, tc.status, stderr)
			}
		})
	}
}

// TestTraceStaleCursorTellsTheOperatorToRestart keeps a changed-evidence
// continuation actionable.
func TestTraceStaleCursorTellsTheOperatorToRestart(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"timeline evidence changed since the first page"}}`))
	})

	code, _, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--json", "--after", "stale")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "start over") {
		t.Errorf("stderr should tell the operator to restart the trace:\n%s", stderr)
	}
	if !strings.Contains(stderr, "otter trace run-1") {
		t.Errorf("stderr should name the restart command:\n%s", stderr)
	}
}

// TestTraceHostileStringsAreEscapedAndShellSafe covers both halves: output must
// not be able to drive the terminal, and a copied command must not be able to
// become a second command.
func TestTraceHostileStringsAreEscapedAndShellSafe(t *testing.T) {
	status := 400
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage("run-1")
		page.Events[0].Message = "boom\x1b[2Jcleared"
		page.Events[1].HTTP.RequestID = "req; rm -rf /"
		page.Events[1].HTTP.URL = "https://x.test/a\nb"
		page.Events[1].HTTP.StatusCode = &status
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(stdout, "\x1b") {
		t.Errorf("a control character reached the terminal:\n%q", stdout)
	}
	if !strings.Contains(stdout, `\x1b[2J`) {
		t.Errorf("the escape should be shown escaped, not dropped:\n%s", stdout)
	}
	if strings.Contains(stdout, "otter request run-1 req; rm") {
		t.Errorf("the detail command must quote a hostile request id:\n%s", stdout)
	}
	if !strings.Contains(stdout, `'req; rm -rf /'`) {
		t.Errorf("the hostile request id should be quoted:\n%s", stdout)
	}
	if strings.Contains(stdout, "\nb\n") {
		t.Errorf("a newline in a URL must not forge a line:\n%s", stdout)
	}
}

// TestTracePipedWithoutJSONRequiresJSONBeforeContinuing is the one place the
// output mode matters: a cursor is only valid for the mode it was issued under.
func TestTracePipedWithoutJSONRequiresJSONBeforeContinuing(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		t.Errorf("a rejected invocation must not reach the API")
	})

	// runCLI writes to a buffer, so stdout is not a terminal and the mode
	// resolves to JSONL even without --json.
	code, _, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--after", "some-cursor")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--json") {
		t.Errorf("stderr should say that continuing a piped trace needs --json:\n%s", stderr)
	}
}

// TestTraceExcludedHTTPIsStated keeps --no-http from looking like a recording
// that observed nothing.
func TestTraceExcludedHTTPIsStated(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage("run-1")
		page.Context.IncludeHTTP = false
		page.Events = []timeline.Event{}
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--no-http", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "--no-http") {
		t.Errorf("the trace must say that HTTP events were excluded by the operator:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capture: complete") {
		t.Errorf("the capture state must still be reported:\n%s", stdout)
	}
}

// TestTraceExpiredCaptureKeepsLossFacts covers the collapse deriveState causes:
// expired wins over incomplete, so the loss counters must be reported alongside.
func TestTraceExpiredCaptureKeepsLossFacts(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage("run-1")
		page.Context.Capture = &inspection.RunCapture{
			RunID: "run-1", State: inspection.CaptureExpired, Policy: inspection.PolicyFull,
			RequestCount: 4, IncompleteCount: 2, DroppedEvents: 3, PayloadsExpired: true,
		}
		page.Events = []timeline.Event{}
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"expired", "never completed", "dropped"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expired capture output is missing %q:\n%s", want, stdout)
		}
	}
}

// TestTracePendingCaptureIsNotDescribedAsRunning covers the wording: an attempt
// can be finished while its recording never finalized.
func TestTracePendingCaptureIsNotDescribedAsRunning(t *testing.T) {
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage("run-1")
		page.Context.Capture = &inspection.RunCapture{
			RunID: "run-1", State: inspection.CapturePending, Policy: inspection.PolicyFull,
			Finalization: inspection.FinalizationPending,
		}
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", "run-1", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "not finalized") {
		t.Errorf("a pending recording on a finished run must say so:\n%s", stdout)
	}
	if strings.Contains(stdout, "still executing") {
		t.Errorf("a finished attempt must not be described as executing:\n%s", stdout)
	}
}
