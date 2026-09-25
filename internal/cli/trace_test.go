package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
		// legitimately contain the word "otter" (the SDK's logger marker). The
		// columns are time, glyph, kind, so the kind is the third field.
		if kind := strings.Fields(line)[2]; kind != timeline.KindLog {
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
		"POST api.example.test/v2/records",
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

// TestTraceTableColumnsAlign is the regression test for a table whose header,
// rows and continuation lines disagreed about where the detail column was, and
// whose padding counted bytes rather than display columns. "·" is one column and
// two bytes, so a byte-based pad shifted every column after the glyph.
//
// It asserts display columns, because that is what a reader sees.
func TestTraceTableColumnsAlign(t *testing.T) {
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		_ = json.NewEncoder(w).Encode(tracePage(runID))
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty", "--width", "200")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	detailCol := -1
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "TIME") {
			detailCol = len([]rune(line[:strings.Index(line, "DETAIL")]))
			continue
		}
		if detailCol < 0 || strings.TrimSpace(line) == "" {
			continue
		}
		runes := []rune(line)
		isMainRow := len(line) >= 8 && line[2] == ':' && line[5] == ':'
		switch {
		case isMainRow:
			if len(runes) <= detailCol {
				t.Errorf("row is shorter than the detail column %d: %q", detailCol, line)
				continue
			}
			// The detail text must begin exactly at the header's column, and the
			// two columns before it are the separator.
			if strings.TrimSpace(string(runes[detailCol])) == "" {
				t.Errorf("row has no detail text at column %d: %q", detailCol+1, line)
			}
			if string(runes[detailCol-2:detailCol]) != "  " {
				t.Errorf("row is not separated from the detail column %d: %q", detailCol+1, line)
			}
		case strings.HasPrefix(line, " "):
			indent := 0
			for indent < len(runes) && runes[indent] == ' ' {
				indent++
			}
			if indent != detailCol {
				t.Errorf("continuation text starts at column %d, want %d: %q", indent+1, detailCol+1, line)
			}
		}
	}
	if detailCol < 0 {
		t.Fatalf("no DETAIL header found in:\n%s", stdout)
	}
}

// TestTraceGlyphMarksEventOutcomesPinsTheDistinctions covers the four cases the
// glyph column exists to separate, especially 4xx (rejected) from 5xx (broken).
func TestTraceGlyphMarksEventOutcomesPinsTheDistinctions(t *testing.T) {
	status := func(code int) *int { return &code }
	duration := int64(9)

	cases := []struct {
		name  string
		event timeline.Event
		want  string
	}{
		{
			name: "2xx succeeds",
			event: timeline.Event{Kind: timeline.KindHTTP, HTTP: &timeline.HTTPEvent{
				StatusCode: status(200), Complete: true, Phase: "completed"}},
			want: "✓",
		},
		{
			name: "4xx is rejected, not broken",
			event: timeline.Event{Kind: timeline.KindHTTP, HTTP: &timeline.HTTPEvent{
				StatusCode: status(400), Complete: true, Phase: "completed"}},
			want: "!",
		},
		{
			name: "5xx is broken",
			event: timeline.Event{Kind: timeline.KindHTTP, HTTP: &timeline.HTTPEvent{
				StatusCode: status(500), Complete: true, Phase: "completed"}},
			want: "×",
		},
		{
			name: "a transport error never reached a status",
			event: timeline.Event{Kind: timeline.KindHTTP, HTTP: &timeline.HTTPEvent{
				ErrorClass: "timeout", Complete: true, Phase: "failed"}},
			want: "×",
		},
		{
			name: "an unfinished exchange is unresolved",
			event: timeline.Event{Kind: timeline.KindHTTP, HTTP: &timeline.HTTPEvent{
				StatusCode: status(200), Complete: false, Phase: "in_progress", DurationMS: &duration}},
			want: "!",
		},
		{
			name:  "a redirect is informational",
			event: timeline.Event{Kind: timeline.KindHTTP, HTTP: &timeline.HTTPEvent{StatusCode: status(302), Complete: true, Phase: "completed"}},
			want:  "·",
		},
		{
			name:  "narration has not succeeded at anything",
			event: timeline.Event{Kind: timeline.KindLifecycle, Message: "run queued (trigger cron)"},
			want:  "·",
		},
		{
			name:  "a successful run is marked",
			event: timeline.Event{Kind: timeline.KindLifecycle, Message: "run succeeded (attempt 1, 1.668s)"},
			want:  "✓",
		},
		{
			name:  "a failed run is marked",
			event: timeline.Event{Kind: timeline.KindLifecycle, Message: "run failed (attempt 1, 41ms)"},
			want:  "×",
		},
		{
			name:  "an informational log is neutral",
			event: timeline.Event{Kind: timeline.KindLog, Message: "sync starting"},
			want:  "·",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := traceGlyph(tc.event); got != tc.want {
				t.Errorf("glyph = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPadEndCountsColumnsNotBytes is a regression test for a misalignment whose
// cause was invisible in review: "·" is one column but two bytes, so padding by
// byte length pushed every column after the glyph one place right. Each of these
// must occupy exactly two columns.
func TestPadEndCountsColumnsNotBytes(t *testing.T) {
	for _, value := range []string{"·", "✓", "×", "!", "ab", "", "abcd"} {
		got := padEnd(value, 2)
		if columns := len([]rune(got)); columns != 2 {
			t.Errorf("padEnd(%q, 2) = %q occupies %d columns, want 2", value, got, columns)
		}
	}
}

// TestGlyphColorIsTerminalOnly pins the rule that matters most: a pipe or a
// redirect must never receive escape sequences, because `otter trace | less`,
// `> file`, and `--json` all expect plain text.
func TestGlyphColorIsTerminalOnly(t *testing.T) {
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		_ = json.NewEncoder(w).Encode(tracePage(runID))
	})

	// runCLI writes to a buffer, so the terminal check is false and the color
	// path is not taken. No escape may appear.
	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("a non-terminal must receive no escape sequences:\n%q", stdout)
	}
}

// TestGlyphColorAppliesToEachOutcome checks the colors themselves, and that
// coloring never changes the columns: an escape occupies bytes but no width.
func TestGlyphColorAppliesToEachOutcome(t *testing.T) {
	restore := colorOutput
	colorOutput = func(io.Writer) bool { return true }
	t.Cleanup(func() { colorOutput = restore })
	// A real terminal sets TERM; the "dumb" check is what declines otherwise.
	t.Setenv("TERM", "xterm-256color")
	os.Unsetenv("NO_COLOR")
	t.Cleanup(func() { os.Unsetenv("NO_COLOR") })

	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		_ = json.NewEncoder(w).Encode(tracePage(runID))
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty", "--width", "200")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// The fixture contains a 400 and no success, so the colored success glyph is
	// asserted against the color function directly rather than the page.
	for _, tc := range []struct {
		name  string
		glyph string
		color string
	}{
		{"a success is green", glyphOK, ansiGreen},
		{"a warning is yellow", glyphWarn, ansiYellow},
		{"a break is red", glyphBroken, ansiRed},
		{"informational text is dim", glyphInfo, ansiDim},
	} {
		got := (table{timeWidth: traceTimeWidth, color: true}).colorGlyph(tc.glyph)
		if !strings.HasPrefix(got, tc.color) {
			t.Errorf("%s: colorGlyph(%q) = %q, want it to start with %q", tc.name, tc.glyph, got, tc.color)
		}
		// The escape occupies bytes but no column, so the field must still be
		// exactly the glyph column wide once escapes are stripped.
		if width := len([]rune(stripANSI(got))); width != traceGlyphWidth {
			t.Errorf("%s: colored glyph occupies %d columns, want %d", tc.name, width, traceGlyphWidth)
		}
	}
	if !strings.Contains(stdout, ansiYellow+glyphWarn+ansiReset) {
		t.Errorf("the 400 in the fixture should be yellow: %q", stdout)
	}

	// --no-color wins over a terminal.
	code, plain, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty", "--no-color")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("--no-color must produce no escapes:\n%q", plain)
	}

	// Colored and plain output must align identically once escapes are stripped.
	colored := stripANSI(stdout)
	if colored != plain {
		t.Errorf("color changed the layout:\ncolored=%q\nplain  =%q", colored, plain)
	}
}

// TestNoColorEnvironment disables color the way other tools ask for it.
func TestNoColorEnvironment(t *testing.T) {
	restore := colorOutput
	colorOutput = func(io.Writer) bool { return true }
	t.Cleanup(func() { colorOutput = restore })

	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		_ = json.NewEncoder(w).Encode(tracePage(runID))
	})
	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("NO_COLOR must be honoured:\n%q", stdout)
	}
}

// stripANSI removes SGR escape sequences so a test can compare layout.
func stripANSI(s string) string {
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "m")
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+1:]
	}
}

// TestTraceHTTPDetailIsFootnoted pins the shape: an exchange is one table row
// with a bracketed reference, and the call site plus the payload command live in
// the footnotes rather than as extra lines under the row.
func TestTraceHTTPDetailIsFootnoted(t *testing.T) {
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		_ = json.NewEncoder(w).Encode(tracePage(runID))
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty", "--width", "200")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	lines := strings.Split(stdout, "\n")

	// The exchange row ends with its reference and carries no call site.
	var rowLine string
	for _, line := range lines {
		if strings.Contains(line, "POST ") && strings.Contains(line, "http") {
			rowLine = line
		}
	}
	if rowLine == "" {
		t.Fatalf("no exchange row found in:\n%s", stdout)
	}
	if !strings.Contains(rowLine, "[1]") {
		t.Errorf("the exchange row should carry its footnote reference: %q", rowLine)
	}
	if strings.Contains(rowLine, "main.py:26") {
		t.Errorf("the call site belongs in the footnotes, not the row: %q", rowLine)
	}
	if strings.Contains(rowLine, "otter request") {
		t.Errorf("the payload command belongs in the footnotes, not the row: %q", rowLine)
	}

	// The footnotes carry the call site and a runnable command, indented to the
	// detail column.
	var sawCallSite, sawCommand bool
	for _, line := range lines {
		if strings.Contains(line, "└1.") {
			sawCallSite = strings.Contains(line, "main.py:26")
		}
		if strings.Contains(line, "otter request "+runID) {
			sawCommand = true
			if indent := len(line) - len(strings.TrimLeft(line, " ")); indent != 25 {
				t.Errorf("a footnote command starts at column %d, want 25: %q", indent+1, line)
			}
		}
	}
	if !sawCallSite {
		t.Errorf("the footnote does not carry the call site:\n%s", stdout)
	}
	if !sawCommand {
		t.Errorf("the footnote does not carry a runnable payload command:\n%s", stdout)
	}
}

// TestTraceFootnotesStateTheCommonCaseOnce checks that a uniform phase and payload
// state is summarized rather than repeated on every entry, and that a differing
// one is shown per entry where it matters.
func TestTraceFootnotesStateTheCommonCaseOnce(t *testing.T) {
	status := 400
	runID := "run-1"

	page := tracePage(runID)
	page.Events = []timeline.Event{{
		Kind: timeline.KindHTTP, At: page.Events[1].At, Source: timeline.SourceHTTPExchanges,
		ID: 1, RunID: runID,
		HTTP: &timeline.HTTPEvent{
			RequestID: "req-1", Method: "POST", URL: "https://a.test/x",
			StatusCode: &status, Phase: "completed", Complete: true,
			Payloads: "partial", CallSite: "main.py:26 in push",
		},
	}}

	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		_ = json.NewEncoder(w).Encode(page)
	})
	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty", "--width", "200")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	// One exchange: the state is stated as a summary, not on the entry.
	if !strings.Contains(stdout, "all 1 exchanges: completed, payloads partial") {
		t.Errorf("a uniform state should be summarized:\n%s", stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "└1.") {
			if strings.Contains(line, "payloads") || strings.Contains(line, "completed") {
				t.Errorf("a uniform state should not be repeated on the entry: %q", line)
			}
			// The " in push" tail is noise; the file and line are what a reader
			// matches against the code.
			if !strings.Contains(line, "main.py:26") || strings.Contains(line, "in push") {
				t.Errorf("the call site should be shortened to its location: %q", line)
			}
		}
	}
}

// TestTraceShowsHTTPErrorReason pins the point of the error summary: a rejected
// call states why in the trace, without the reader opening another command. The
// message is allowed to wrap; what it must not do is disappear or lose its
// alignment with the detail column.
func TestTraceShowsHTTPErrorReason(t *testing.T) {
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage(runID)
		page.Events[1].HTTP.ErrorCode = "FIELD_INTEGRITY_EXCEPTION"
		page.Events[1].HTTP.ErrorMessage = "There's a problem with this country. Please select a country from the list of valid countries.: Mailing Country"
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty", "--width", "120")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "FIELD_INTEGRITY_EXCEPTION") {
		t.Errorf("the error code is missing:\n%s", stdout)
	}
	if !strings.Contains(stdout, "problem with this country") {
		t.Errorf("the error message is missing:\n%s", stdout)
	}

	// Every wrapped line stays in the detail column, so the message reads as one
	// cell rather than as a stray paragraph.
	var sawReason bool
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "FIELD_INTEGRITY_EXCEPTION") && strings.HasPrefix(line, " ") {
			sawReason = true
			if indent := len([]rune(line)) - len([]rune(strings.TrimLeft(line, " "))); indent != 25 {
				t.Errorf("the reason starts at column %d, want 25: %q", indent+1, line)
			}
		}
		if strings.Contains(line, "Please select") {
			if indent := len([]rune(line)) - len([]rune(strings.TrimLeft(line, " "))); indent != 25 {
				t.Errorf("a wrapped reason line starts at column %d, want 25: %q", indent+1, line)
			}
		}
	}
	if !sawReason {
		t.Errorf("the reason is not on its own aligned line:\n%s", stdout)
	}
}

// TestTraceHTTPErrorReasonStatesMissingCapture distinguishes "the response
// explained nothing" from "no response body was captured", which is what a
// metadata-only recording leaves behind.
func TestTraceHTTPErrorReasonStatesMissingCapture(t *testing.T) {
	status := 500
	cases := []struct {
		name   string
		http   *timeline.HTTPEvent
		want   string
		reject string
	}{
		{
			name: "code and message",
			http: &timeline.HTTPEvent{StatusCode: &status, ErrorCode: "BOOM", ErrorMessage: "it broke"},
			want: "BOOM: it broke",
		},
		{
			name: "message only",
			http: &timeline.HTTPEvent{StatusCode: &status, ErrorMessage: "it broke"},
			want: "it broke",
		},
		{
			name: "code only",
			http: &timeline.HTTPEvent{StatusCode: &status, ErrorCode: "BOOM"},
			want: "BOOM",
		},
		{
			name: "nothing captured is stated, not silent",
			http: &timeline.HTTPEvent{StatusCode: &status},
			want: "no error message captured",
		},
		{
			name: "a success has no reason",
			http: &timeline.HTTPEvent{StatusCode: statusPtr(200)},
			want: "",
		},
		{
			name: "a transport error has no reason line",
			http: &timeline.HTTPEvent{ErrorClass: "timeout"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errorReason(tc.http)
			if got != tc.want {
				t.Errorf("errorReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWrapAtKeepsWholeWords checks the wrapping used for a long reason: it breaks
// on spaces, never mid-word, and never exceeds the width.
func TestWrapAtKeepsWholeWords(t *testing.T) {
	message := "There's a problem with this country. Please select a country from the list of valid countries."
	lines := wrapAt(message, 40)
	if len(lines) < 2 {
		t.Fatalf("expected a wrapped message, got %v", lines)
	}
	for _, line := range lines {
		if len([]rune(line)) > 40 {
			t.Errorf("line is %d wide, want at most 40: %q", len([]rune(line)), line)
		}
		if strings.HasPrefix(line, " ") || strings.HasSuffix(line, " ") {
			t.Errorf("line has stray padding: %q", line)
		}
	}
	// Rejoining reproduces the original words, so nothing was lost or split.
	if got := strings.Join(lines, " "); got != message {
		t.Errorf("wrapping changed the text:\n got %q\nwant %q", got, message)
	}

	// A width that disables wrapping leaves the value alone.
	if got := wrapAt(message, 0); len(got) != 1 || got[0] != message {
		t.Errorf("a non-positive width should not wrap: %v", got)
	}
	if got := wrapAt(message, 500); len(got) != 1 {
		t.Errorf("a value that fits should not wrap: %v", got)
	}
}

func statusPtr(code int) *int { return &code }

// TestTraceLogLineIsShownAsStored pins the decision to leave a ctx.log line
// intact. Splitting its structured fields into key=value pairs was tried and read
// worse: a short field ended up alone on its own line above a wrapped object. One
// event stays one row.
func TestTraceLogLineIsShownAsStored(t *testing.T) {
	runID := "run-1"
	message := `shopify page {"count":1,"first":{"email":"bb@gmail.com"},"level":"info","logger":"otter","page":0}`
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage(runID)
		page.Events = append(page.Events, timeline.Event{
			Kind: timeline.KindLog, At: page.Events[0].At, Source: timeline.SourceRunLogs,
			ID: 9, RunID: runID, Stream: "otter", Message: message,
		})
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty", "--width", "200")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}

	var row string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "shopify page") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("the log row is missing:\n%s", stdout)
	}
	// The stored line is there verbatim, JSON suffix and all.
	if !strings.Contains(row, message) {
		t.Errorf("the row does not carry the stored line:\n got %q\nwant contains %q", row, message)
	}
	// One event, one row: no continuation carrying its fields, and no label.
	if strings.Contains(stdout, "[L") {
		t.Errorf("log lines must not be labelled:\n%s", stdout)
	}
	fieldsLeaked := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "  ") && strings.Contains(line, "count=1") {
			fieldsLeaked++
		}
	}
	if fieldsLeaked > 0 {
		t.Errorf("the fields should not be split onto their own line:\n%s", stdout)
	}
}

// TestTraceLogWithoutFieldsIsUnchanged keeps a plain stdout line simple.
func TestTraceLogWithoutFieldsIsUnchanged(t *testing.T) {
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage(runID)
		page.Events = append(page.Events, timeline.Event{
			Kind: timeline.KindLog, At: page.Events[0].At, Source: timeline.SourceRunLogs,
			ID: 9, RunID: runID, Stream: "stdout", Message: "just a sentence, no fields",
		})
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "just a sentence, no fields") {
		t.Errorf("the line is missing:\n%s", stdout)
	}
}

// TestTraceLogWithoutFieldsHasNoFootnote keeps a plain stdout line from growing a
// footnote it does not need.
func TestTraceLogWithoutFieldsHasNoFootnote(t *testing.T) {
	runID := "run-1"
	server := traceAPI(t, func(w http.ResponseWriter, r *http.Request, req *timeline.Request) {
		page := tracePage(runID)
		page.Events = append(page.Events, timeline.Event{
			Kind: timeline.KindLog, At: page.Events[0].At, Source: timeline.SourceRunLogs,
			ID: 9, RunID: runID, Stream: "stdout", Message: "just a sentence, no fields",
		})
		_ = json.NewEncoder(w).Encode(page)
	})

	code, stdout, stderr := runCLI(t, "--api", server.URL, "trace", runID, "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "just a sentence, no fields") {
		t.Errorf("the line is missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "[L1]") {
		t.Errorf("a line with no fields should have no footnote:\n%s", stdout)
	}
}
