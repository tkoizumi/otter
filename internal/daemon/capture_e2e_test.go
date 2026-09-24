package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
)

// captureFixtureSource is the integration the acceptance scenario runs. It adds
// no logging of its own: everything the operator needs must come from capture.
//
// It reads a cursor over HTTP, posts a transformed payload that the origin
// rejects with 400, reads the error body, and then fails.
const captureFixtureSource = `
import json
import os
import urllib.error
import urllib.request

from otter import Context

ctx = Context.from_environment()
base = os.environ["ORIGIN_URL"]

with urllib.request.urlopen(base + "/cursor") as response:
    cursor = json.loads(response.read())

payload = {"cursor": cursor["cursor"], "items": [1, 2, 3]}
request = urllib.request.Request(
    base + "/records",
    data=json.dumps(payload).encode("utf-8"),
    headers={
        "Content-Type": "application/json",
        "Authorization": "Bearer " + cursor["password"],
    },
    method="POST",
)
try:
    with urllib.request.urlopen(request) as response:
        response.read()
except urllib.error.HTTPError as exc:
    body = json.loads(exc.read().decode("utf-8"))
    raise RuntimeError("upstream rejected the batch: %s" % body["error"])
`

// TestHTTPCaptureEndToEnd is the milestone's acceptance test: after a failed
// JSON POST, the request list and the request detail reveal the read value, the
// GET response, the POST body and the error response, without the integration
// having logged anything.
func TestHTTPCaptureEndToEnd(t *testing.T) {
	requirePython(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cursor":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"cursor":   "cur-42",
				"password": "origin-secret-value",
			})
		case "/records":
			body, _ := io.ReadAll(r.Body)
			// The transformed payload must reach the origin intact; the capture
			// record is what proves it did.
			if !strings.Contains(string(body), "cur-42") {
				writeJSON(t, w, http.StatusBadRequest, map[string]any{"error": "cursor missing"})
				return
			}
			writeJSON(t, w, http.StatusBadRequest, map[string]any{
				"error":        "cursor rejected",
				"access_token": "origin-secret-value",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()

	root := t.TempDir()
	writeIntegration(t, root, "capture-demo", fmt.Sprintf(`
version: 1
name: capture-demo
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
env:
  ORIGIN_URL: %s
`, origin.URL), captureFixtureSource)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "capture-demo",
		api.TriggerPayload{Type: api.TriggerManual, Body: json.RawMessage(`{"customer_id":123}`)},
		api.SubmitRunOptions{Capture: inspection.PolicyFull})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}

	view := awaitTerminal(t, d, runID)
	if view.Run.Status != runs.StatusFailed {
		t.Fatalf("run status = %s, want failed", view.Run.Status)
	}
	if view.Run.CapturePolicy != string(inspection.PolicyFull) {
		t.Errorf("run capture policy = %q, want full", view.Run.CapturePolicy)
	}

	// The recording must be complete: both exchanges, both bodies.
	summary, err := d.CaptureSummary(context.Background(), runID)
	if err != nil {
		t.Fatalf("capture summary: %v", err)
	}
	if summary.RequestCount != 2 {
		t.Fatalf("captured %d requests, want 2 (%+v)", summary.RequestCount, summary)
	}
	if summary.State != inspection.CaptureComplete {
		t.Errorf("capture state = %q, want complete (summary: %+v)", summary.State, summary)
	}

	requests, err := d.ListCaptureRequests(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("listed %d requests, want 2", len(requests))
	}

	getID, postID := "", ""
	for _, request := range requests {
		switch request.Method {
		case http.MethodGet:
			getID = request.RequestID
		case http.MethodPost:
			postID = request.RequestID
		}
	}
	if getID == "" || postID == "" {
		t.Fatalf("expected one GET and one POST, got %+v", requests)
	}

	// The read value: the GET response the integration consumed.
	get, err := d.GetCaptureRequest(context.Background(), runID, getID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if get.StatusCode == nil || *get.StatusCode != 200 {
		t.Errorf("GET status = %v, want 200", get.StatusCode)
	}
	if get.ResponseBody == nil || get.ResponseBody.State != inspection.BodyCaptured {
		t.Fatalf("GET response body was not captured: %+v", get.ResponseBody)
	}
	if got := string(get.ResponseBody.JSON); !strings.Contains(got, "cur-42") {
		t.Errorf("GET response body = %s, want the cursor value", got)
	}
	// A credential-shaped field is redacted even though the integration saw it.
	if got := string(get.ResponseBody.JSON); !strings.Contains(got, inspection.RedactedPlaceholder) {
		t.Errorf("GET response body was not redacted: %s", got)
	}
	if got := string(get.ResponseBody.JSON); strings.Contains(got, "origin-secret-value") {
		t.Errorf("a secret reached stored capture: %s", got)
	}

	// The POST body and the error response.
	post, err := d.GetCaptureRequest(context.Background(), runID, postID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if post.StatusCode == nil || *post.StatusCode != 400 {
		t.Errorf("POST status = %v, want 400", post.StatusCode)
	}
	if post.RequestBody == nil || post.RequestBody.State != inspection.BodyCaptured {
		t.Fatalf("POST request body was not captured: %+v", post.RequestBody)
	}
	if got := string(post.RequestBody.JSON); !strings.Contains(got, "cur-42") {
		t.Errorf("POST request body = %s, want the transformed payload", got)
	}
	if post.ResponseBody == nil || post.ResponseBody.State != inspection.BodyCaptured {
		t.Fatalf("POST response body was not captured: %+v", post.ResponseBody)
	}
	if got := string(post.ResponseBody.JSON); !strings.Contains(got, "cursor rejected") {
		t.Errorf("POST response body = %s, want the upstream error", got)
	}

	// The request-scoped credential the integration sent is redacted.
	var sawRedactedAuthorization bool
	for _, header := range post.RequestHeaders {
		if strings.EqualFold(header.Name, "Authorization") {
			sawRedactedAuthorization = header.Value == inspection.RedactedPlaceholder
		}
	}
	if !sawRedactedAuthorization {
		t.Errorf("the POST Authorization header was not redacted: %+v", post.RequestHeaders)
	}

	// The same exchange is reachable by request id alone: the daemon resolves the
	// owning run from the stored row, because the id itself does not carry it.
	byID, err := d.GetCaptureRequestByID(context.Background(), postID)
	if err != nil {
		t.Fatalf("get request by id alone: %v", err)
	}
	if byID.RunID != runID || byID.RequestID != postID {
		t.Errorf("lookup by id returned %s/%s, want %s/%s", byID.RunID, byID.RequestID, runID, postID)
	}

	// Finally, nothing anywhere in storage may contain the origin's secret.
	assertNoSecretInCapture(t, d)
}

// TestCaptureIsMetadataByDefault proves a normal run records summaries and never
// payloads, so enabling capture does not quietly persist bodies everywhere.
func TestCaptureMetadataStoresNoPayloads(t *testing.T) {
	requirePython(t)

	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"ok": true, "password": "origin-secret-value"})
	}))
	defer origin.Close()

	root := t.TempDir()
	writeIntegration(t, root, "metadata-demo", fmt.Sprintf(`
version: 1
name: metadata-demo
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
env:
  ORIGIN_URL: %s
`, origin.URL), `
import os
import urllib.request

with urllib.request.urlopen(os.environ["ORIGIN_URL"] + "/data") as response:
    response.read()
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "metadata-demo",
		api.TriggerPayload{Type: api.TriggerManual},
		api.SubmitRunOptions{Capture: inspection.PolicyMetadata})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	awaitTerminal(t, d, runID)

	requests, err := d.ListCaptureRequests(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("listed %d requests, want 1", len(requests))
	}
	if requests[0].Completeness() != "metadata" {
		t.Errorf("completeness = %q, want metadata", requests[0].Completeness())
	}
	if requests[0].StatusCode == nil || *requests[0].StatusCode != 200 {
		t.Errorf("status = %v, want 200", requests[0].StatusCode)
	}

	detail, err := d.GetCaptureRequest(context.Background(), runID, requests[0].RequestID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if detail.RequestBody != nil || detail.ResponseBody != nil {
		t.Errorf("a metadata run must not store payloads: %+v", detail)
	}
	if len(detail.ResponseHeaders) != 0 {
		t.Errorf("a metadata run must not store headers: %+v", detail.ResponseHeaders)
	}
}

// TestCaptureDefaultsToFull is the point of the default: a run nobody
// configured -- the shape of a cron trigger -- still records the payloads needed
// to explain a failure after the fact. Nothing here passes --capture or a
// SubmitRunOptions policy.
func TestCaptureDefaultsToFull(t *testing.T) {
	requirePython(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusBadRequest, map[string]any{
			"error":        "cursor rejected",
			"access_token": "origin-secret-value",
		})
	}))
	defer origin.Close()

	root := t.TempDir()
	writeIntegration(t, root, "default-full-demo", fmt.Sprintf(`
version: 1
name: default-full-demo
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
env:
  ORIGIN_URL: %s
`, origin.URL), `
import json
import os
import urllib.error
import urllib.request

request = urllib.request.Request(
    os.environ["ORIGIN_URL"] + "/records",
    data=json.dumps({"cursor": "cur-42"}).encode("utf-8"),
    headers={"Content-Type": "application/json"},
    method="POST",
)
try:
    with urllib.request.urlopen(request) as response:
        response.read()
except urllib.error.HTTPError as exc:
    raise RuntimeError("upstream rejected the batch: %s" % exc.read().decode("utf-8"))
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	// No options at all: this is exactly what a cron tick submits.
	runID, err := d.SubmitRun(context.Background(), "default-full-demo",
		api.TriggerPayload{Type: api.TriggerCron})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	view := awaitTerminal(t, d, runID)
	if view.Run.CapturePolicy != string(inspection.PolicyFull) {
		t.Fatalf("run capture policy = %q, want full", view.Run.CapturePolicy)
	}

	requests, err := d.ListCaptureRequests(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("listed %d requests, want 1", len(requests))
	}
	if got := requests[0].Completeness(); got != "full" {
		t.Errorf("completeness = %q, want full", got)
	}

	detail, err := d.GetCaptureRequest(context.Background(), runID, requests[0].RequestID)
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if detail.RequestBody == nil || detail.RequestBody.State != inspection.BodyCaptured {
		t.Errorf("the default run stored no request body: %+v", detail.RequestBody)
	}
	if detail.ResponseBody == nil || detail.ResponseBody.State != inspection.BodyCaptured {
		t.Errorf("the default run stored no response body: %+v", detail.ResponseBody)
	}
	// Defaulting to full must not mean defaulting to unredacted.
	if got := string(detail.ResponseBody.JSON); strings.Contains(got, "origin-secret-value") {
		t.Errorf("a secret reached stored capture: %s", got)
	}
}

// TestManifestCaptureOffStopsRecording is the opt-out: the integration declares
// that its payloads must not be stored, and the deployment-wide default does not
// override that decision.
func TestManifestCaptureOffStopsRecording(t *testing.T) {
	requirePython(t)

	root := t.TempDir()
	writeIntegration(t, root, "capture-off-demo", `
version: 1
name: capture-off-demo
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
capture: off
`, "print('nothing to capture')\n")

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRun(context.Background(), "capture-off-demo",
		api.TriggerPayload{Type: api.TriggerCron})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	view := awaitTerminal(t, d, runID)
	if view.Run.CapturePolicy != string(inspection.PolicyOff) {
		t.Fatalf("run capture policy = %q, want off", view.Run.CapturePolicy)
	}

	summary, err := d.CaptureSummary(context.Background(), runID)
	if err != nil {
		t.Fatalf("capture summary: %v", err)
	}
	if summary.State != inspection.CaptureOff {
		t.Errorf("capture state = %q, want off", summary.State)
	}
}

// TestCapturePolicyIsInheritedByRetries proves two things at once: a retry
// records the policy its parent was submitted with, and each attempt owns its
// own requests rather than sharing one recording.
func TestCapturePolicyIsInheritedByRetries(t *testing.T) {
	requirePython(t)

	var calls int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeJSON(t, w, http.StatusOK, map[string]any{"ok": true, "password": "origin-secret-value"})
	}))
	defer origin.Close()

	root := t.TempDir()
	writeIntegration(t, root, "retry-capture", fmt.Sprintf(`
version: 1
name: retry-capture
entrypoint: main.py
timeout: 60
retry:
  attempts: 2
  backoff: exponential
  initial_delay: 100ms
env:
  ORIGIN_URL: %s
`, origin.URL), `
import os
import urllib.request

from otter import run


@run
def main(ctx):
    with urllib.request.urlopen(os.environ["ORIGIN_URL"] + "/data") as response:
        response.read()
    raise RuntimeError("always fails")
`)

	d := newDaemon(t, root, "", nil, nil)
	startDaemon(t, d)

	runID, err := d.SubmitRunWithOptions(context.Background(), "retry-capture",
		api.TriggerPayload{Type: api.TriggerManual},
		api.SubmitRunOptions{Capture: inspection.PolicyFull})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	view := awaitTerminal(t, d, runID)

	if len(view.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(view.Attempts))
	}
	for i, attempt := range view.Attempts {
		if attempt.CapturePolicy != string(inspection.PolicyFull) {
			t.Errorf("attempt %d capture policy = %q, want full", i+1, attempt.CapturePolicy)
		}
		summary, err := d.CaptureSummary(context.Background(), attempt.ID)
		if err != nil {
			t.Fatalf("capture summary for attempt %d: %v", i+1, err)
		}
		if summary.RequestCount != 1 {
			t.Errorf("attempt %d recorded %d requests, want its own single request",
				i+1, summary.RequestCount)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("the origin saw %d calls, want 2 (one per attempt)", got)
	}
}

// TestCaptureOverheadIsBounded measures the same fixture under each policy and
// reports latency, in-process heap and database growth. It is a guard rail, not
// a precise benchmark: the thresholds are loose on purpose, so the test fails
// only on a real regression such as capture blocking the request path or
// growing storage without bound.
func TestCaptureOverheadIsBounded(t *testing.T) {
	requirePython(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"ok": true, "index": 1, "password": "origin-secret-value",
		})
	}))
	defer origin.Close()

	const requestCount = 25

	type measurement struct {
		policy   inspection.Policy
		elapsed  time.Duration
		dbBytes  int64
		heapByte uint64
	}
	results := make([]measurement, 0, 3)

	for _, policy := range []inspection.Policy{
		inspection.PolicyOff, inspection.PolicyMetadata, inspection.PolicyFull,
	} {
		root := t.TempDir()
		writeIntegration(t, root, "overhead", fmt.Sprintf(`
version: 1
name: overhead
entrypoint: main.py
timeout: 120
retry:
  attempts: 0
env:
  ORIGIN_URL: %s
  REQUEST_COUNT: "%d"
`, origin.URL, requestCount), `
import os
import urllib.request

from otter import run


@run
def main(ctx):
    base = os.environ["ORIGIN_URL"]
    for _ in range(int(os.environ.get("REQUEST_COUNT", "25"))):
        with urllib.request.urlopen(base + "/data") as response:
            response.read()
`)

		d := newDaemon(t, root, "", nil, nil)
		startDaemon(t, d)

		var before runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		dbBefore := databaseBytes(t, d)
		started := time.Now()

		runID, err := d.SubmitRunWithOptions(context.Background(), "overhead",
			api.TriggerPayload{Type: api.TriggerManual},
			api.SubmitRunOptions{Capture: policy})
		if err != nil {
			t.Fatalf("submit run with capture %s: %v", policy, err)
		}
		view := awaitTerminal(t, d, runID)
		elapsed := time.Since(started)

		if view.LatestStatus != runs.StatusSucceeded {
			t.Fatalf("capture %s: run status = %s, want succeeded", policy, view.LatestStatus)
		}
		summary, err := d.CaptureSummary(context.Background(), runID)
		if err != nil {
			t.Fatalf("capture summary with %s: %v", policy, err)
		}
		if policy == inspection.PolicyFull && summary.RequestCount != requestCount {
			t.Errorf("full capture recorded %d requests, want %d", summary.RequestCount, requestCount)
		}
		if policy == inspection.PolicyMetadata && summary.RequestCount != requestCount {
			t.Errorf("metadata capture recorded %d requests, want %d", summary.RequestCount, requestCount)
		}
		if policy == inspection.PolicyOff && summary.State != inspection.CaptureOff {
			t.Errorf("off capture state = %q, want off", summary.State)
		}

		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		results = append(results, measurement{
			policy:   policy,
			elapsed:  elapsed,
			dbBytes:  databaseBytes(t, d) - dbBefore,
			heapByte: after.HeapAlloc,
		})

		// Storage is bounded by the run's capture quota, whatever the policy.
		if growth := databaseBytes(t, d) - dbBefore; growth > 10*1024*1024 {
			t.Errorf("capture %s grew the database by %d bytes, above the 10 MiB per-run quota",
				policy, growth)
		}
	}

	for _, result := range results {
		t.Logf("capture=%-8s latency=%s db_growth=%d bytes heap=%d bytes",
			result.policy, result.elapsed.Round(time.Millisecond), result.dbBytes, result.heapByte)
	}

	// Capture must never block the request path. The threshold is deliberately
	// generous: it exists to catch capture adding seconds, not milliseconds.
	off := results[0].elapsed
	full := results[2].elapsed
	if full > off+5*time.Second {
		t.Errorf("full capture took %s against %s with capture off: capture is blocking the run",
			full, off)
	}
}

// databaseBytes reports the on-disk size of the daemon's database, including the
// WAL, which is where freshly written rows live until a checkpoint.
func databaseBytes(t *testing.T, d *Daemon) int64 {
	t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(d.db.Path + suffix)
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Errorf("encode fixture response: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// assertNoSecretInCapture reads every stored column that could hold a payload
// and fails if the origin's secret appears anywhere.
func assertNoSecretInCapture(t *testing.T, d *Daemon) {
	t.Helper()
	const secret = "origin-secret-value"

	rows, err := d.db.QueryContext(context.Background(),
		`SELECT request_headers, response_headers, request_body, response_body, sanitized_url,
		        transport_error, call_site
		   FROM http_exchanges`)
	if err != nil {
		t.Fatalf("read capture rows: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var requestHeaders, responseHeaders, requestBody, responseBody, url, transportError, callSite string
		if err := rows.Scan(&requestHeaders, &responseHeaders, &requestBody, &responseBody,
			&url, &transportError, &callSite); err != nil {
			t.Fatalf("scan capture row: %v", err)
		}
		combined := strings.Join([]string{
			requestHeaders, responseHeaders, requestBody, responseBody, url, transportError, callSite,
		}, "\n")
		if strings.Contains(combined, secret) {
			t.Fatalf("a secret reached stored capture:\n%s", combined)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate capture rows: %v", err)
	}
}

// requirePythonModule skips a test when the run's interpreter cannot import a
// module. The optional adapters need their client actually installed, and the
// SDK must never depend on either one.
func requirePythonModule(t *testing.T, module string) {
	t.Helper()
	requirePython(t)
	cmd := exec.Command("python3", "-c", "import "+module)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("python3 cannot import %s; skipping adapter coverage test: %s",
			module, strings.TrimSpace(string(out)))
	}
}

// A third-party client must be recorded exactly like urllib, and the run's
// coverage must name the adapter that made that possible. Without the adapter
// report, an integration using requests or httpx would show "coverage: urllib"
// next to an empty request list -- the most misleading answer capture can give.
func TestCaptureCoversInstalledClientAdapters(t *testing.T) {
	cases := []struct {
		name   string
		module string
		source string
	}{
		{
			name:   inspection.AdapterHTTPX,
			module: "httpx",
			source: `
import os

import httpx

response = httpx.post(
    os.environ["ORIGIN_URL"] + "/records",
    json={"cursor": "cur-42", "access_token": "sentinel-secret"},
)
raise RuntimeError("upstream rejected the batch: %s" % response.json()["error"])
`,
		},
		{
			name:   inspection.AdapterRequests,
			module: "requests",
			source: `
import os

import requests

response = requests.post(
    os.environ["ORIGIN_URL"] + "/records",
    json={"cursor": "cur-42", "access_token": "sentinel-secret"},
)
raise RuntimeError("upstream rejected the batch: %s" % response.json()["error"])
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requirePythonModule(t, tc.module)

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusBadRequest, map[string]any{
					"error":        "cursor rejected",
					"access_token": "origin-secret-value",
				})
			}))
			defer origin.Close()

			root := t.TempDir()
			writeIntegration(t, root, tc.name+"-demo", fmt.Sprintf(`
version: 1
name: %s-demo
entrypoint: main.py
timeout: 60
retry:
  attempts: 0
env:
  ORIGIN_URL: %s
`, tc.name, origin.URL), tc.source)

			d := newDaemon(t, root, "", nil, nil)
			startDaemon(t, d)

			runID, err := d.SubmitRun(context.Background(), tc.name+"-demo",
				api.TriggerPayload{Type: api.TriggerManual})
			if err != nil {
				t.Fatalf("submit run: %v", err)
			}
			view := awaitTerminal(t, d, runID)
			if view.Run.Status != runs.StatusFailed {
				t.Fatalf("run status = %s, want failed", view.Run.Status)
			}

			summary, err := d.CaptureSummary(context.Background(), runID)
			if err != nil {
				t.Fatalf("capture summary: %v", err)
			}
			found := false
			for _, adapter := range summary.Adapters {
				if adapter == tc.name {
					found = true
				}
			}
			if !found {
				t.Errorf("adapters = %v, want %s among them", summary.Adapters, tc.name)
			}
			if !strings.Contains(summary.Coverage, tc.name) {
				t.Errorf("coverage = %q, want it to name %s", summary.Coverage, tc.name)
			}

			requests, err := d.ListCaptureRequests(context.Background(), runID, 0, 100)
			if err != nil {
				t.Fatalf("list requests: %v", err)
			}
			if len(requests) != 1 {
				t.Fatalf("listed %d requests, want 1", len(requests))
			}
			detail, err := d.GetCaptureRequest(context.Background(), runID, requests[0].RequestID)
			if err != nil {
				t.Fatalf("get request: %v", err)
			}
			if detail.RequestBody == nil || detail.RequestBody.State != inspection.BodyCaptured {
				t.Fatalf("request body = %+v, want a captured body", detail.RequestBody)
			}
			if !strings.Contains(string(detail.RequestBody.JSON), `"access_token":"REDACTED"`) {
				t.Errorf("request body was not redacted: %s", detail.RequestBody.JSON)
			}
			if detail.ResponseBody == nil || detail.ResponseBody.State != inspection.BodyCaptured {
				t.Fatalf("response body = %+v, want a captured 400 body", detail.ResponseBody)
			}
			assertNoSecretInCapture(t, d)
		})
	}
}
