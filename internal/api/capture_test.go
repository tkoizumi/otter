package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/runs"
)

// fakeCapture is a small in-memory stand-in for internal/inspection.Store. It
// deliberately enforces the same visible rules the real store does -- unconfigured
// runs are refused, terminal events complete an exchange, duplicate producer
// sequences are idempotent -- so the API tests exercise the contract rather than
// the fake.
type fakeCapture struct {
	mu        sync.Mutex
	summaries map[string]*inspection.RunCapture
	exchanges map[string][]inspection.Exchange
	// lastSubmittedPolicy is the policy the most recent submission resolved.
	lastSubmittedPolicy inspection.Policy
}

func newFakeCapture() *fakeCapture {
	return &fakeCapture{
		summaries: map[string]*inspection.RunCapture{},
		exchanges: map[string][]inspection.Exchange{},
	}
}

func (c *fakeCapture) begin(runID, integrationID string, policy inspection.Policy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The submitted policy is remembered so a test can assert what the
	// submission endpoint resolved.
	c.lastSubmittedPolicy = policy
	c.summaries[runID] = &inspection.RunCapture{
		RunID:         runID,
		State:         inspection.CapturePending,
		IntegrationID: integrationID,
		Policy:        policy,
		SchemaVersion: inspection.SchemaVersion,
		PolicyVersion: inspection.PolicyVersion,
		Adapters:      []string{inspection.AdapterURLLib},
		Coverage:      inspection.Coverage,
		Finalization:  inspection.FinalizationPending,
	}
}

func (c *fakeCapture) summary(runID string) *inspection.RunCapture {
	c.mu.Lock()
	defer c.mu.Unlock()
	if summary, ok := c.summaries[runID]; ok {
		return summary
	}
	return inspection.UnavailableCapture(runID)
}

func (c *fakeCapture) ingest(runID string, batch inspection.EventBatch) (*inspection.IngestResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	summary, ok := c.summaries[runID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", inspection.ErrNotConfigured, runID)
	}
	if batch.Policy != summary.Policy {
		return nil, fmt.Errorf("batch policy %q does not match the run's recorded policy %q",
			batch.Policy, summary.Policy)
	}

	result := &inspection.IngestResult{Finalization: batch.Finalization}
	existing := c.exchanges[runID]
	for _, event := range batch.Events {
		index := -1
		for i := range existing {
			if existing[i].RequestID == event.RequestID {
				index = i
				break
			}
		}
		if index >= 0 && existing[index].ProducerSeq == event.ProducerSeq {
			result.Duplicates++
			continue
		}
		// Merge the event into the exchange rather than replacing it, so the
		// started/response/completed lifecycle behaves as the real store does.
		exchange := inspection.Exchange{
			RunID:         runID,
			RequestID:     event.RequestID,
			IntegrationID: summary.IntegrationID,
			OccurredAt:    event.OccurredAt,
			Phase:         inspection.PhaseInProgress,
		}
		if index >= 0 {
			exchange = existing[index]
		} else {
			exchange.ID = int64(len(existing) + 1)
		}
		if event.ProducerSeq > exchange.ProducerSeq {
			exchange.ProducerSeq = event.ProducerSeq
		}
		if event.Method != "" {
			exchange.Method = event.Method
		}
		if event.URL != "" {
			exchange.URL = event.URL
		}
		if event.CallSite != "" {
			exchange.CallSite = event.CallSite
		}
		if event.StatusCode != nil {
			exchange.StatusCode = event.StatusCode
		}
		if event.TransportClass != "" {
			exchange.TransportClass = event.TransportClass
		}
		if event.DurationTotalMS != nil {
			exchange.DurationTotalMS = event.DurationTotalMS
		}
		if len(event.RequestHeaders) > 0 {
			exchange.RequestHeaders = event.RequestHeaders
		}
		if len(event.ResponseHeaders) > 0 {
			exchange.ResponseHeaders = event.ResponseHeaders
		}
		if event.RequestBody != nil {
			exchange.RequestBody = event.RequestBody
		}
		if event.ResponseBody != nil {
			exchange.ResponseBody = event.ResponseBody
		}
		if event.Kind == inspection.EventCompleted {
			exchange.Complete = true
			exchange.Phase = inspection.PhaseFor(event)
		}
		exchange.Payloads = inspection.PayloadCoverage(batch.Policy, exchange.RequestBody, exchange.ResponseBody)
		if index >= 0 {
			existing[index] = exchange
		} else {
			existing = append(existing, exchange)
		}
		result.Accepted++
	}
	c.exchanges[runID] = existing
	return result, nil
}

func (c *fakeCapture) list(runID string, afterID int64, limit int) []inspection.ExchangeSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	out := []inspection.ExchangeSummary{}
	for _, exchange := range c.exchanges[runID] {
		if exchange.ID <= afterID {
			continue
		}
		out = append(out, inspection.ExchangeSummary{
			ID:              exchange.ID,
			RequestID:       exchange.RequestID,
			Phase:           exchange.Phase,
			Complete:        exchange.Complete,
			Method:          exchange.Method,
			URL:             exchange.URL,
			StatusCode:      exchange.StatusCode,
			TransportClass:  exchange.TransportClass,
			DurationTotalMS: exchange.DurationTotalMS,
			OccurredAt:      exchange.OccurredAt,
			Payloads:        exchange.Payloads,
		})
		if len(out) == limit {
			break
		}
	}
	return out
}

func (c *fakeCapture) get(runID, requestID string) (*inspection.Exchange, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.exchanges[runID] {
		if c.exchanges[runID][i].RequestID == requestID {
			exchange := c.exchanges[runID][i]
			return &exchange, nil
		}
	}
	return nil, inspection.ErrNotFound
}

// findByRequestID mirrors the real store's global lookup: it deliberately
// searches every run, so a duplicate id surfaces as more than one match.
func (c *fakeCapture) findByRequestID(requestID string) []inspection.Exchange {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []inspection.Exchange
	for _, exchanges := range c.exchanges {
		for i := range exchanges {
			if exchanges[i].RequestID == requestID {
				out = append(out, exchanges[i])
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ------------------------------------------------------- api.Backend methods

func (f *fakeBackend) IngestCaptureEvents(ctx context.Context, runID string, batch inspection.EventBatch) (*inspection.IngestResult, error) {
	if !f.hasRun(runID) {
		return nil, runs.ErrNotFound
	}
	return f.capture.ingest(runID, batch)
}

func (f *fakeBackend) CaptureSummary(ctx context.Context, runID string) (*inspection.RunCapture, error) {
	if !f.hasRun(runID) {
		return nil, runs.ErrNotFound
	}
	return f.capture.summary(runID), nil
}

func (f *fakeBackend) ListCaptureRequests(ctx context.Context, runID string, afterID int64, limit int) ([]inspection.ExchangeSummary, error) {
	if !f.hasRun(runID) {
		return nil, runs.ErrNotFound
	}
	return f.capture.list(runID, afterID, limit), nil
}

func (f *fakeBackend) GetCaptureRequest(ctx context.Context, runID, requestID string) (*inspection.Exchange, error) {
	if !f.hasRun(runID) {
		return nil, runs.ErrNotFound
	}
	return f.capture.get(runID, requestID)
}

func (f *fakeBackend) GetCaptureRequestByID(ctx context.Context, requestID string) (*inspection.Exchange, error) {
	matches := f.capture.findByRequestID(requestID)
	switch len(matches) {
	case 0:
		return nil, inspection.ErrNotFound
	case 1:
		exchange := matches[0]
		return &exchange, nil
	}
	runIDs := make([]string, 0, len(matches))
	for _, match := range matches {
		runIDs = append(runIDs, match.RunID)
	}
	return nil, fmt.Errorf("%w: request id %q appears in runs %s; pass the run id",
		inspection.ErrAmbiguous, requestID, strings.Join(runIDs, ", "))
}

func (f *fakeBackend) hasRun(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.runs[runID]
	return ok
}

// SubmitRunWithOptions records the submission option and creates the per-run
// capture summary, exactly as the daemon does at submission time.
func (f *fakeBackend) SubmitRunWithOptions(ctx context.Context, integrationID string, payload TriggerPayload, opts SubmitRunOptions) (string, error) {
	runID, err := f.SubmitRun(ctx, integrationID, payload)
	if err != nil {
		return "", err
	}
	policy := opts.Capture
	if !policy.Valid() {
		policy = inspection.DefaultPolicy
	}
	f.capture.begin(runID, integrationID, policy)
	return runID, nil
}

// submittedPolicy reports the policy the most recent submission resolved.
func (f *fakeBackend) submittedPolicy() inspection.Policy {
	f.capture.mu.Lock()
	defer f.capture.mu.Unlock()
	return f.capture.lastSubmittedPolicy
}

// ------------------------------------------------------------------- helpers

func adminHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer admin-secret"}
}

func runTokenHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func captureBatch(policy inspection.Policy, events ...inspection.RequestEvent) []byte {
	body, err := json.Marshal(inspection.EventBatch{
		SchemaVersion: inspection.SchemaVersion,
		Policy:        policy,
		Events:        events,
	})
	if err != nil {
		panic(err)
	}
	return body
}

func captureStart(requestID string, seq int64) inspection.RequestEvent {
	return inspection.RequestEvent{
		Kind:        inspection.EventStarted,
		RequestID:   requestID,
		ProducerSeq: seq,
		OccurredAt:  time.Now().UTC(),
		Method:      "POST",
		URL:         "https://api.example.com/v1/items",
		CallSite:    "source.py:42 in push",
	}
}

func seedCapturedRun(t *testing.T, b *fakeBackend, runID, integrationID string, policy inspection.Policy) {
	t.Helper()
	b.addRun(&runs.Run{ID: runID, IntegrationID: integrationID, Status: runs.StatusRunning})
	b.capture.begin(runID, integrationID, policy)
}

// --------------------------------------------------------------------- tests

func TestCaptureIngestIsRunScoped(t *testing.T) {
	b := newFakeBackend()
	seedCapturedRun(t, b, "run-A", "int-A", inspection.PolicyMetadata)
	seedCapturedRun(t, b, "run-B", "int-A", inspection.PolicyMetadata)
	b.runTokens["token-A"] = RunToken{RunID: "run-A", IntegrationID: "int-A"}

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	body := captureBatch(inspection.PolicyMetadata, captureStart("req-1", 1))

	// A run token may submit its own run's events.
	r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events", body, runTokenHeaders("token-A"))
	wantStatus(t, r, http.StatusAccepted)

	// It may not attribute events to another run.
	r = do(t, http.MethodPost, srv.URL+"/v1/runs/run-B/requests/events", body, runTokenHeaders("token-A"))
	wantStatus(t, r, http.StatusForbidden)

	// An operator may submit for any run.
	r = do(t, http.MethodPost, srv.URL+"/v1/runs/run-B/requests/events", body, adminHeaders())
	wantStatus(t, r, http.StatusAccepted)

	// No token at all is unauthorized.
	r = do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events", body, nil)
	wantStatus(t, r, http.StatusUnauthorized)

	// A token that is no longer live -- revoked, or never issued -- is
	// unauthorized too, so a finished run cannot keep reporting.
	r = do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events", body, runTokenHeaders("revoked-token"))
	wantStatus(t, r, http.StatusUnauthorized)
}

func TestCaptureIngestValidatesTheBatch(t *testing.T) {
	b := newFakeBackend()
	seedCapturedRun(t, b, "run-A", "int-A", inspection.PolicyMetadata)
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	payloadUnderMetadata := inspection.RequestEvent{
		Kind: inspection.EventStarted, RequestID: "req-2", Method: "POST",
		RequestBody: &inspection.BodyDescriptor{State: inspection.BodyCaptured, JSON: json.RawMessage(`{"a":1}`)},
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"not json", []byte("{")},
		{"wrong schema version", []byte(`{"schema_version":99,"policy":"metadata","events":[{"kind":"request.started","request_id":"req-1","method":"GET"}]}`)},
		{"unknown kind", []byte(`{"schema_version":1,"policy":"metadata","events":[{"kind":"request.teleported","request_id":"req-1"}]}`)},
		{"payload under a metadata policy", captureBatch(inspection.PolicyMetadata, payloadUnderMetadata)},
		{"empty batch", []byte(`{"schema_version":1,"policy":"metadata","events":[]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events", tc.body, adminHeaders())
			wantStatus(t, r, http.StatusBadRequest)
		})
	}
}

func TestCaptureIngestOnUnconfiguredRunIsAConflict(t *testing.T) {
	b := newFakeBackend()
	// The run exists but was submitted before capture, so there is no summary.
	b.addRun(&runs.Run{ID: "run-old", IntegrationID: "int-A", Status: runs.StatusSucceeded})
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	body := captureBatch(inspection.PolicyMetadata, captureStart("req-1", 1))
	r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-old/requests/events", body, adminHeaders())
	wantStatus(t, r, http.StatusConflict)
}

func TestCaptureReadsRequireOperatorAuthorization(t *testing.T) {
	b := newFakeBackend()
	seedCapturedRun(t, b, "run-A", "int-A", inspection.PolicyMetadata)
	b.runTokens["token-A"] = RunToken{RunID: "run-A", IntegrationID: "int-A"}
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	// A run token is not an operator: payload inspection is out of its scope.
	r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/requests", nil, runTokenHeaders("token-A"))
	wantStatus(t, r, http.StatusForbidden)

	r = do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/requests", nil, adminHeaders())
	wantStatus(t, r, http.StatusOK)
}

func TestCaptureListAndDetail(t *testing.T) {
	b := newFakeBackend()
	seedCapturedRun(t, b, "run-A", "int-A", inspection.PolicyFull)
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	const marker = "unique-payload-marker"
	status := 400
	start := captureStart("req-1", 1)
	start.RequestBody = &inspection.BodyDescriptor{
		State: inspection.BodyCaptured, ContentType: "application/json",
		JSON: json.RawMessage(`{"note":"` + marker + `"}`),
	}
	completed := inspection.RequestEvent{
		Kind: inspection.EventCompleted, RequestID: "req-1", ProducerSeq: 2,
		OccurredAt: time.Now().UTC(), StatusCode: &status,
		DurationTotalMS: int64Ptr(12),
		ResponseBody: &inspection.BodyDescriptor{
			State: inspection.BodyCaptured, JSON: json.RawMessage(`{"error":"bad request"}`),
		},
	}

	r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events", captureBatch(inspection.PolicyFull, start), adminHeaders())
	wantStatus(t, r, http.StatusAccepted)
	r = do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events", captureBatch(inspection.PolicyFull, completed), adminHeaders())
	wantStatus(t, r, http.StatusAccepted)

	// The list reports the exchange and the capture state, but no payload.
	r = do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/requests", nil, adminHeaders())
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(string(r.body), marker) {
		t.Errorf("the list response leaked a payload: %s", r.body)
	}
	var list CaptureRequestsResponse
	if err := json.Unmarshal(r.body, &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, r.body)
	}
	if list.Capture == nil || list.Capture.State != inspection.CapturePending {
		t.Errorf("capture = %+v, want a pending summary", list.Capture)
	}
	if len(list.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(list.Requests))
	}
	if list.Requests[0].StatusCode == nil || *list.Requests[0].StatusCode != 400 {
		t.Errorf("status = %v, want 400", list.Requests[0].StatusCode)
	}
	if list.Requests[0].Completeness() != "full" {
		t.Errorf("completeness = %q, want full", list.Requests[0].Completeness())
	}

	// The detail includes the sanitized bodies.
	r = do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/requests/req-1", nil, adminHeaders())
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(string(r.body), marker) {
		t.Errorf("the detail response is missing the request body: %s", r.body)
	}
	var detail CaptureRequestResponse
	if err := json.Unmarshal(r.body, &detail); err != nil {
		t.Fatalf("decode detail: %v (%s)", err, r.body)
	}
	if detail.Request == nil || detail.Request.ResponseBody == nil {
		t.Fatalf("detail = %+v, want both bodies", detail.Request)
	}
	if string(detail.Request.ResponseBody.JSON) != `{"error":"bad request"}` {
		t.Errorf("response body = %s", detail.Request.ResponseBody.JSON)
	}

	// An unknown request, and an unknown run, are both not found.
	r = do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/requests/req-nope", nil, adminHeaders())
	wantStatus(t, r, http.StatusNotFound)
	r = do(t, http.MethodGet, srv.URL+"/v1/runs/run-nope/requests", nil, adminHeaders())
	wantStatus(t, r, http.StatusNotFound)
}

func TestCaptureLookupByRequestIDAlone(t *testing.T) {
	b := newFakeBackend()
	seedCapturedRun(t, b, "run-A", "int-A", inspection.PolicyMetadata)
	b.runTokens["token-A"] = RunToken{RunID: "run-A", IntegrationID: "int-A"}
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events",
		captureBatch(inspection.PolicyMetadata, captureStart("req-1", 1)), adminHeaders())
	wantStatus(t, r, http.StatusAccepted)

	// The id alone finds the exchange and carries the owning run's capture state.
	r = do(t, http.MethodGet, srv.URL+"/v1/requests/req-1", nil, adminHeaders())
	wantStatus(t, r, http.StatusOK)
	var detail CaptureRequestResponse
	if err := json.Unmarshal(r.body, &detail); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.body)
	}
	if detail.Request == nil || detail.Request.RunID != "run-A" {
		t.Fatalf("request = %+v, want the run-A exchange", detail.Request)
	}
	if detail.Capture == nil || detail.Capture.RunID != "run-A" {
		t.Errorf("capture = %+v, want run-A's summary", detail.Capture)
	}

	// Inspection stays operator-only on the run-less path too.
	r = do(t, http.MethodGet, srv.URL+"/v1/requests/req-1", nil, runTokenHeaders("token-A"))
	wantStatus(t, r, http.StatusForbidden)
	r = do(t, http.MethodGet, srv.URL+"/v1/requests/req-1", nil, nil)
	wantStatus(t, r, http.StatusUnauthorized)

	// An unknown id is not found, exactly as on the run-scoped path.
	r = do(t, http.MethodGet, srv.URL+"/v1/requests/req-nope", nil, adminHeaders())
	wantStatus(t, r, http.StatusNotFound)
}

func TestCaptureLookupByRequestIDAmbiguousAcrossRuns(t *testing.T) {
	b := newFakeBackend()
	seedCapturedRun(t, b, "run-A", "int-A", inspection.PolicyMetadata)
	seedCapturedRun(t, b, "run-B", "int-A", inspection.PolicyMetadata)
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	for _, runID := range []string{"run-A", "run-B"} {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/"+runID+"/requests/events",
			captureBatch(inspection.PolicyMetadata, captureStart("req-shared", 1)), adminHeaders())
		wantStatus(t, r, http.StatusAccepted)
	}

	// The id identifies no single run, so it is a conflict rather than an
	// arbitrary match, and the message names the candidates.
	r := do(t, http.MethodGet, srv.URL+"/v1/requests/req-shared", nil, adminHeaders())
	wantStatus(t, r, http.StatusConflict)
	for _, want := range []string{"run-A", "run-B"} {
		if !strings.Contains(string(r.body), want) {
			t.Errorf("the conflict should name %s: %s", want, r.body)
		}
	}

	// Naming the run still resolves exactly one exchange.
	r = do(t, http.MethodGet, srv.URL+"/v1/runs/run-B/requests/req-shared", nil, adminHeaders())
	wantStatus(t, r, http.StatusOK)
}

func TestCaptureUnavailableIsReportedNotImpliedEmpty(t *testing.T) {
	b := newFakeBackend()
	// A historical run: it exists but predates capture entirely.
	b.addRun(&runs.Run{ID: "run-old", IntegrationID: "int-A", Status: runs.StatusSucceeded})
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-old/requests", nil, adminHeaders())
	wantStatus(t, r, http.StatusOK)

	var list CaptureRequestsResponse
	if err := json.Unmarshal(r.body, &list); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.body)
	}
	if list.Capture == nil {
		t.Fatal("a run without capture must still report a capture object")
	}
	if list.Capture.State != inspection.CaptureUnavailable {
		t.Errorf("state = %q, want unavailable", list.Capture.State)
	}
	if len(list.Requests) != 0 {
		t.Errorf("requests = %d, want none", len(list.Requests))
	}
}

func TestCaptureListPaginates(t *testing.T) {
	b := newFakeBackend()
	seedCapturedRun(t, b, "run-A", "int-A", inspection.PolicyMetadata)
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	for i := 1; i <= 3; i++ {
		body := captureBatch(inspection.PolicyMetadata, captureStart(fmt.Sprintf("req-%d", i), int64(i)))
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-A/requests/events", body, adminHeaders())
		wantStatus(t, r, http.StatusAccepted)
	}

	r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/requests?limit=2", nil, adminHeaders())
	wantStatus(t, r, http.StatusOK)
	var first CaptureRequestsResponse
	if err := json.Unmarshal(r.body, &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(first.Requests) != 2 {
		t.Fatalf("first page = %d, want 2", len(first.Requests))
	}

	r = do(t, http.MethodGet,
		fmt.Sprintf("%s/v1/runs/run-A/requests?after_id=%d", srv.URL, first.Requests[len(first.Requests)-1].ID),
		nil, adminHeaders())
	wantStatus(t, r, http.StatusOK)
	var second CaptureRequestsResponse
	if err := json.Unmarshal(r.body, &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(second.Requests) != 1 || second.Requests[0].RequestID != "req-3" {
		t.Errorf("second page = %+v, want only req-3", second.Requests)
	}

	// A bad cursor is a client error, not a silent empty page.
	r = do(t, http.MethodGet, srv.URL+"/v1/runs/run-A/requests?after_id=-1", nil, adminHeaders())
	wantStatus(t, r, http.StatusBadRequest)
}

func TestSubmitRunCaptureOption(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	// The default for a normal run is metadata.
	r := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/runs",
		[]byte(`{"customer_id":123}`), adminHeaders())
	wantStatus(t, r, http.StatusAccepted)
	if got := b.submittedPolicy(); got != inspection.DefaultPolicy {
		t.Errorf("default policy = %q, want %q", got, inspection.DefaultPolicy)
	}

	// An explicit policy is resolved and recorded.
	r = do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/runs?capture=full", nil, adminHeaders())
	wantStatus(t, r, http.StatusAccepted)
	if got := b.submittedPolicy(); got != inspection.PolicyFull {
		t.Errorf("policy = %q, want full", got)
	}

	// An unknown policy is a client error and queues nothing.
	queued := len(b.submitted)
	r = do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/runs?capture=everything", nil, adminHeaders())
	wantStatus(t, r, http.StatusBadRequest)
	if len(b.submitted) != queued {
		t.Errorf("an invalid policy must not queue a run")
	}

	// The trigger body is still the trigger body, verbatim.
	if len(b.submitted) == 0 {
		t.Fatal("no submission was recorded")
	}
	if string(b.submitted[0].payload.Body) != `{"customer_id":123}` {
		t.Errorf("trigger body = %s, want it recorded verbatim", b.submitted[0].payload.Body)
	}
}

func int64Ptr(v int64) *int64 { return &v }
