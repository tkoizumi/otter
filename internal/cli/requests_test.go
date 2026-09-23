package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/inspection"
)

// captureAPI serves the two inspection read endpoints from a fixed response, so
// the CLI tests exercise rendering rather than the daemon.
func captureAPI(t *testing.T, list api.CaptureRequestsResponse, detail api.CaptureRequestResponse, detailStatus int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/requests"):
			_ = json.NewEncoder(w).Encode(list)
		default:
			if detailStatus != http.StatusOK {
				w.WriteHeader(detailStatus)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"capture record not found"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(detail)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := New("test", &stdout, &stderr)
	code := app.Run(context.Background(), args)
	return code, stdout.String(), stderr.String()
}

func TestRequestsListsCaptureStateAndRows(t *testing.T) {
	status := 400
	list := api.CaptureRequestsResponse{
		Capture: &inspection.RunCapture{
			RunID: "run-1", State: inspection.CaptureComplete, Policy: inspection.PolicyFull,
			RequestCount: 1, CompletedCount: 1, RedactionCount: 2, Coverage: inspection.Coverage,
		},
		Requests: []inspection.ExchangeSummary{{
			ID: 1, RequestID: "req-1", Phase: inspection.PhaseCompleted, Complete: true,
			Method: "POST", URL: "https://api.example.com/v1/items", StatusCode: &status,
			DurationTotalMS: int64PtrCLI(12), Payloads: "full",
		}},
	}
	server := captureAPI(t, list, api.CaptureRequestResponse{}, http.StatusOK)

	// Flag after the positional argument: the reorderer must still parse it.
	code, stdout, stderr := runCLI(t, "--api", server.URL, "requests", "run-1", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"capture: complete", "req-1", "POST", "400", "full", "api.example.com"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestRequestsDistinguishesUnavailableFromEmpty(t *testing.T) {
	// A run with no recording at all: this must not read like "no requests".
	list := api.CaptureRequestsResponse{
		Capture:  inspection.UnavailableCapture("run-old"),
		Requests: []inspection.ExchangeSummary{},
	}
	server := captureAPI(t, list, api.CaptureRequestResponse{}, http.StatusOK)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "requests", "run-old", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "unavailable") {
		t.Errorf("an unrecorded run must say so:\n%s", stdout)
	}
	if strings.Contains(stdout, "observed no outgoing HTTP requests") {
		t.Errorf("an unrecorded run must not imply zero requests were made:\n%s", stdout)
	}

	// The genuinely-empty case says something different.
	list.Capture = &inspection.RunCapture{
		RunID: "run-2", State: inspection.CaptureComplete, Policy: inspection.PolicyMetadata,
	}
	server2 := captureAPI(t, list, api.CaptureRequestResponse{}, http.StatusOK)
	code, stdout, stderr = runCLI(t, "--api", server2.URL, "requests", "run-2", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "observed no outgoing HTTP requests") {
		t.Errorf("a complete empty recording should say it observed nothing:\n%s", stdout)
	}
}

func TestRequestsReportsIncompleteAndExpired(t *testing.T) {
	cases := []struct {
		name    string
		capture *inspection.RunCapture
		want    string
	}{
		{
			name: "incomplete",
			capture: &inspection.RunCapture{
				RunID: "r", State: inspection.CaptureIncomplete, Policy: inspection.PolicyFull,
				RequestCount: 1, IncompleteCount: 1, DroppedEvents: 2,
			},
			want: "incomplete",
		},
		{
			name: "expired",
			capture: &inspection.RunCapture{
				RunID: "r", State: inspection.CaptureExpired, Policy: inspection.PolicyFull,
				RequestCount: 3, PayloadsExpired: true,
			},
			want: "expired",
		},
		{
			name: "off",
			capture: &inspection.RunCapture{
				RunID: "r", State: inspection.CaptureOff, Policy: inspection.PolicyOff,
			},
			want: "capture was disabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := captureAPI(t, api.CaptureRequestsResponse{Capture: tc.capture}, api.CaptureRequestResponse{}, http.StatusOK)
			code, stdout, stderr := runCLI(t, "--api", server.URL, "requests", "run-1", "--pretty")
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, stderr)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("output is missing %q:\n%s", tc.want, stdout)
			}
		})
	}
}

func TestRequestsJSONIsMachineReadable(t *testing.T) {
	list := api.CaptureRequestsResponse{
		Capture: &inspection.RunCapture{RunID: "run-1", State: inspection.CaptureComplete, Policy: inspection.PolicyMetadata},
		Requests: []inspection.ExchangeSummary{{
			ID: 1, RequestID: "req-1", Method: "GET", URL: "https://api.example.com/x",
			Payloads: "metadata", OccurredAt: time.Now().UTC(),
		}},
	}
	server := captureAPI(t, list, api.CaptureRequestResponse{}, http.StatusOK)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "requests", "run-1", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var decoded api.CaptureRequestsResponse
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("--json output is not the API response: %v\n%s", err, stdout)
	}
	if decoded.Capture == nil || decoded.Capture.State != inspection.CaptureComplete {
		t.Errorf("decoded capture = %+v", decoded.Capture)
	}
	if len(decoded.Requests) != 1 || decoded.Requests[0].RequestID != "req-1" {
		t.Errorf("decoded requests = %+v", decoded.Requests)
	}
}

func TestRequestShowsBodiesRedactionsAndOmissions(t *testing.T) {
	status := 400
	detail := api.CaptureRequestResponse{
		Capture: &inspection.RunCapture{RunID: "run-1", State: inspection.CaptureComplete, Policy: inspection.PolicyFull},
		Request: &inspection.Exchange{
			ID: 1, RunID: "run-1", RequestID: "req-1",
			Phase: inspection.PhaseCompleted, Complete: true,
			Method: "POST", URL: "https://api.example.com/records", CallSite: "main.py:27 in main",
			StatusCode:      &status,
			DurationTotalMS: int64PtrCLI(1),
			RequestHeaders:  []inspection.HeaderPair{{Name: "Authorization", Value: inspection.RedactedPlaceholder}},
			RequestBody: &inspection.BodyDescriptor{
				State: inspection.BodyCaptured, ContentType: "application/json",
				JSON: json.RawMessage(`{"cursor":"cur-42"}`),
			},
			ResponseBody: &inspection.BodyDescriptor{
				State: inspection.BodyOmitted, Reason: inspection.ReasonIncomplete,
			},
			Payloads: "partial",
		},
	}
	server := captureAPI(t, api.CaptureRequestsResponse{}, detail, http.StatusOK)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "request", "run-1", "req-1", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{
		"POST https://api.example.com/records",
		"status:    400",
		"call site: main.py:27 in main",
		"cur-42",
		"Authorization: REDACTED",
		"did not read the body to the end",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestRequestEscapesControlCharacters(t *testing.T) {
	detail := api.CaptureRequestResponse{
		Capture: &inspection.RunCapture{RunID: "run-1", State: inspection.CaptureComplete, Policy: inspection.PolicyFull},
		Request: &inspection.Exchange{
			ID: 1, RunID: "run-1", RequestID: "req-1",
			Phase: inspection.PhaseCompleted, Complete: true,
			Method: "GET", URL: "https://api.example.com/x",
			ResponseHeaders: []inspection.HeaderPair{
				{Name: "X-Evil", Value: "\x1b[31mforged\x1b[0m"},
			},
		},
	}
	server := captureAPI(t, api.CaptureRequestsResponse{}, detail, http.StatusOK)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "request", "run-1", "req-1", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.ContainsRune(stdout, 0x1b) {
		t.Errorf("a raw escape character reached the terminal:\n%q", stdout)
	}
	if !strings.Contains(stdout, `\x1b[31m`) {
		t.Errorf("the escape was not shown visibly:\n%s", stdout)
	}
}

func TestRequestUnknownIDFails(t *testing.T) {
	server := captureAPI(t, api.CaptureRequestsResponse{}, api.CaptureRequestResponse{}, http.StatusNotFound)
	code, _, stderr := runCLI(t, "--api", server.URL, "request", "run-1", "missing", "--pretty")
	if code == 0 {
		t.Fatal("a missing request must not exit 0")
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q, want a not-found message", stderr)
	}
}

func TestRequestAcceptsRequestIDAlone(t *testing.T) {
	detail := api.CaptureRequestResponse{
		Capture: &inspection.RunCapture{RunID: "run-1", State: inspection.CaptureComplete, Policy: inspection.PolicyMetadata},
		Request: &inspection.Exchange{
			ID: 1, RunID: "run-1", RequestID: "req-1",
			Phase: inspection.PhaseCompleted, Complete: true,
			Method: "GET", URL: "https://api.example.com/x",
		},
	}
	server := captureAPI(t, api.CaptureRequestsResponse{}, detail, http.StatusOK)

	code, stdout, stderr := runCLI(t, "--api", server.URL, "request", "req-1", "--pretty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "GET https://api.example.com/x") {
		t.Errorf("output is missing the exchange:\n%s", stdout)
	}
}

func TestRequestByIDAloneUnknownHintsAtRunID(t *testing.T) {
	// The likeliest mistake in the one-argument form is passing a run id and
	// leaving the request id off, so the failure should point at the run's list.
	server := captureAPI(t, api.CaptureRequestsResponse{}, api.CaptureRequestResponse{}, http.StatusNotFound)

	code, _, stderr := runCLI(t, "--api", server.URL, "request", "run-1", "--pretty")
	if code == 0 {
		t.Fatal("an unknown id must not exit 0")
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q, want a not-found message", stderr)
	}
	if !strings.Contains(stderr, "otter requests") {
		t.Errorf("stderr = %q, want a hint naming the run's request list", stderr)
	}
}

func TestRequestsUsageErrors(t *testing.T) {
	code, _, stderr := runCLI(t, "requests")
	if code != 2 {
		t.Errorf("exit = %d, want 2 for a missing run id", code)
	}
	if !strings.Contains(stderr, "usage: otter requests") {
		t.Errorf("stderr = %q, want usage", stderr)
	}

	// The detail command takes one positional (a request id) or two (run and
	// request); zero and three are both usage errors. Neither reaches the daemon.
	for _, args := range [][]string{{"request"}, {"request", "a", "b", "c"}} {
		code, _, stderr = runCLI(t, args...)
		if code != 2 {
			t.Errorf("args %v: exit = %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "usage: otter request") {
			t.Errorf("args %v: stderr = %q, want usage", args, stderr)
		}
	}
}

func int64PtrCLI(v int64) *int64 { return &v }
