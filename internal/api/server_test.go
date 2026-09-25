package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/timeline"
)

// fakeBackend is an in-memory Backend that lets the real HTTP handler be
// exercised end to end through httptest.
type fakeBackend struct {
	mu sync.Mutex

	version   string
	startedAt time.Time

	integrations map[string]IntegrationView
	webhookFor   map[string]string
	runTokens    map[string]RunToken

	runs      map[string]*runs.Run
	runOrder  []string
	nextRun   int
	logs      map[string][]runs.LogEntry
	nextLogID int64
	state     map[string]map[string]json.RawMessage

	queueDepth int
	runCounts  map[string]int

	// capture is the in-memory HTTP capture fake, defined in capture_test.go.
	capture *fakeCapture

	// timeline seam for the API contract tests, defined in timeline_test.go.
	timelinePage     *timeline.Page
	timelineErr      error
	timelineRequests []timeline.Request

	submitted []submittedRun
	cancelled []string

	reloads   int
	reloadOut ReloadResult
	reloadErr error
}

type submittedRun struct {
	integrationID string
	payload       TriggerPayload
}

var _ Backend = (*fakeBackend)(nil)

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		version:      "test-1.0.0",
		startedAt:    time.Now().Add(-90 * time.Second).UTC(),
		integrations: map[string]IntegrationView{},
		webhookFor:   map[string]string{},
		runTokens:    map[string]RunToken{},
		runs:         map[string]*runs.Run{},
		logs:         map[string][]runs.LogEntry{},
		state:        map[string]map[string]json.RawMessage{},
		runCounts:    map[string]int{},
		capture:      newFakeCapture(),
	}
}

func (f *fakeBackend) addIntegration(id string, valid bool, webhookToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.integrations[id] = IntegrationView{
		ID:       id,
		Name:     id,
		Path:     "/integrations/" + id,
		Valid:    valid,
		Triggers: TriggerView{WebhookEnabled: webhookToken != "", WebhookToken: webhookToken},
	}
	if webhookToken != "" {
		f.webhookFor[id] = webhookToken
	}
}

func (f *fakeBackend) addRun(r *runs.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[r.ID] = r
	f.runOrder = append(f.runOrder, r.ID)
}

func (f *fakeBackend) addLogs(runID string, entries ...runs.LogEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range entries {
		if e.ID > f.nextLogID {
			f.nextLogID = e.ID
		}
		f.logs[runID] = append(f.logs[runID], e)
	}
}

func (f *fakeBackend) seedState(integrationID, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state[integrationID] == nil {
		f.state[integrationID] = map[string]json.RawMessage{}
	}
	f.state[integrationID][key] = json.RawMessage(value)
}

func (f *fakeBackend) submissionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submitted)
}

func (f *fakeBackend) lastSubmission() (submittedRun, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.submitted) == 0 {
		return submittedRun{}, false
	}
	return f.submitted[len(f.submitted)-1], true
}

func (f *fakeBackend) logsFor(runID string) []runs.LogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runs.LogEntry(nil), f.logs[runID]...)
}

// ------------------------------------------------------------- Backend impl

func (f *fakeBackend) Version() string { return f.version }

func (f *fakeBackend) StartedAt() time.Time { return f.startedAt }

func (f *fakeBackend) ListIntegrations() []IntegrationView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]IntegrationView, 0, len(f.integrations))
	for _, v := range f.integrations {
		// A listing must never leak the webhook token.
		v.Triggers.WebhookToken = ""
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *fakeBackend) GetIntegration(id string) (IntegrationView, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.integrations[id]
	return v, ok
}

func (f *fakeBackend) IntegrationGeneration(id string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.integrations[id]
	if !ok {
		return 0, false
	}
	return v.Generation, true
}

func (f *fakeBackend) ResolveIntegration(ref string) (IntegrationView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.integrations[ref]; ok {
		return v, nil
	}
	for _, v := range f.integrations {
		if v.Name == ref {
			return v, nil
		}
	}
	return IntegrationView{}, fmt.Errorf("integration %q: %w", ref, ErrNotFound)
}

func (f *fakeBackend) RegisterIntegration(_ context.Context, path string) (IntegrationView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return IntegrationView{ID: path, Name: path, Path: path, Valid: true}, nil
}

func (f *fakeBackend) ResetIntegration(_ context.Context, ref string) (ResetView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ResetView{OldID: ref, NewID: ref + "-new", Name: ref, Path: "/tmp/" + ref}, nil
}

func (f *fakeBackend) DeleteIntegration(_ context.Context, ref string) (DeletedView, error) {
	return DeletedView{Deleted: true, ID: ref, Name: ref}, nil
}

func (f *fakeBackend) MoveIntegration(_ context.Context, ref, destination string) (IntegrationView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return IntegrationView{ID: ref, Name: ref, Path: destination, Valid: true}, nil
}

func (f *fakeBackend) Reload(_ context.Context) (ReloadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	if f.reloadErr != nil {
		return ReloadResult{}, f.reloadErr
	}
	return f.reloadOut, nil
}

func (f *fakeBackend) SubmitRun(_ context.Context, integrationID string, payload TriggerPayload) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, submittedRun{integrationID: integrationID, payload: payload})
	f.nextRun++
	runID := fmt.Sprintf("submitted-%d", f.nextRun)
	f.runs[runID] = &runs.Run{
		ID:            runID,
		IntegrationID: integrationID,
		TriggerType:   payload.Type,
		Status:        runs.StatusQueued,
		Attempt:       1,
		CreatedAt:     time.Now().UTC(),
	}
	f.runOrder = append(f.runOrder, runID)
	return runID, nil
}

func (f *fakeBackend) CancelRun(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.runs[runID]; !ok {
		return ErrNotFound
	}
	f.cancelled = append(f.cancelled, runID)
	return nil
}

func (f *fakeBackend) GetRunDetail(_ context.Context, runID string) (*RunView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	return &RunView{Run: &cp, RootRunID: cp.ID, LatestStatus: cp.Status, Attempts: []*runs.Run{&cp}}, nil
}

func (f *fakeBackend) ListRuns(_ context.Context, filter runs.Filter) ([]*runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*runs.Run{}
	for _, id := range f.runOrder {
		r := f.runs[id]
		if filter.IntegrationID != "" && r.IntegrationID != filter.IntegrationID {
			continue
		}
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeBackend) RunLogs(_ context.Context, runID string, afterID int64, limit int) ([]runs.LogEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runs.LogEntry
	for _, e := range f.logs[runID] {
		if e.ID <= afterID {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeBackend) AppendRunLog(_ context.Context, runID, stream, message string, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextLogID++
	f.logs[runID] = append(f.logs[runID], runs.LogEntry{
		ID:        f.nextLogID,
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Stream:    stream,
		Message:   message,
	})
	return nil
}

func (f *fakeBackend) GetState(_ context.Context, integrationID, key string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.state[integrationID][key]
	if !ok {
		return nil, ErrNotFound
	}
	return append(json.RawMessage(nil), v...), nil
}

func (f *fakeBackend) SetState(_ context.Context, integrationID, key string, value json.RawMessage) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state[integrationID] == nil {
		f.state[integrationID] = map[string]json.RawMessage{}
	}
	f.state[integrationID][key] = append(json.RawMessage(nil), value...)
	return time.Now().UTC(), nil
}

func (f *fakeBackend) DeleteState(_ context.Context, integrationID, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.state[integrationID][key]; !ok {
		return false, nil
	}
	delete(f.state[integrationID], key)
	return true, nil
}

func (f *fakeBackend) AllState(_ context.Context, integrationID string) (map[string]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]json.RawMessage{}
	for k, v := range f.state[integrationID] {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out, nil
}

func (f *fakeBackend) QueueDepth(context.Context) (int, error) { return f.queueDepth, nil }

func (f *fakeBackend) RunCounts(context.Context) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.runCounts {
		out[k] = v
	}
	return out, nil
}

func (f *fakeBackend) ResolveRunToken(token string) (RunToken, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scope, ok := f.runTokens[token]
	return scope, ok
}

func (f *fakeBackend) WebhookTokenFor(integrationID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token, ok := f.webhookFor[integrationID]
	return token, ok && token != ""
}

// ------------------------------------------------------------------ helpers

func testLogger() *logging.Logger {
	return logging.New(io.Discard, logging.FormatJSON, logging.LevelError)
}

func newTestServer(t *testing.T, cfg ServerConfig, b Backend) *httptest.Server {
	t.Helper()
	return httptest.NewServer(NewServer(cfg, b, testLogger()).Handler())
}

type httpResult struct {
	status int
	body   []byte
	header http.Header
}

func do(t *testing.T, method, url string, body []byte, hdr map[string]string) httpResult {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return httpResult{status: resp.StatusCode, body: data, header: resp.Header}
}

func (r httpResult) errorEnvelope(t *testing.T) ErrorResponse {
	t.Helper()
	var env ErrorResponse
	if err := json.Unmarshal(r.body, &env); err != nil {
		t.Fatalf("response body is not the JSON error envelope: %q: %v", r.body, err)
	}
	if env.Error.Code == "" {
		t.Fatalf("error envelope has no code: %q", r.body)
	}
	return env
}

func (r httpResult) decode(t *testing.T, out any) {
	t.Helper()
	if err := json.Unmarshal(r.body, out); err != nil {
		t.Fatalf("decode response %q: %v", r.body, err)
	}
}

func wantStatus(t *testing.T, r httpResult, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status = %d, want %d (body: %s)", r.status, want, r.body)
	}
}

// --------------------------------------------------------------------- tests

func TestAuthenticationWithoutTokenOnLoopback(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	for _, path := range []string{"/health", "/v1/integrations", "/v1/integrations/int-A", "/v1/runs"} {
		t.Run(path, func(t *testing.T) {
			r := do(t, http.MethodGet, srv.URL+path, nil, nil)
			wantStatus(t, r, http.StatusOK)
		})
	}
}

func TestAuthenticationWithAPIToken(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "s3cret-token"}, b)
	defer srv.Close()

	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"missing header", nil, http.StatusUnauthorized},
		{"wrong token", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"wrong scheme", map[string]string{"Authorization": "Basic s3cret-token"}, http.StatusUnauthorized},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, http.StatusUnauthorized},
		{"correct token", map[string]string{"Authorization": "Bearer s3cret-token"}, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, http.MethodGet, srv.URL+"/v1/integrations", nil, tc.hdr)
			wantStatus(t, r, tc.want)
			if tc.want == http.StatusUnauthorized {
				if env := r.errorEnvelope(t); env.Error.Code != CodeUnauthorized {
					t.Fatalf("error code = %q, want %q", env.Error.Code, CodeUnauthorized)
				}
			}
		})
	}
}

func TestRunTokenScopes(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	b.addIntegration("int-B", true, "")
	b.runTokens["run-token"] = RunToken{RunID: "run-A", IntegrationID: "int-A"}
	now := time.Now().UTC()
	b.addRun(&runs.Run{ID: "run-A", IntegrationID: "int-A", Status: runs.StatusQueued, Attempt: 1, CreatedAt: now})
	b.addRun(&runs.Run{ID: "run-B", IntegrationID: "int-B", Status: runs.StatusQueued, Attempt: 1, CreatedAt: now})
	b.seedState("int-A", "present", "1")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	auth := map[string]string{"Authorization": "Bearer run-token"}

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		want   int
	}{
		{"read own run", http.MethodGet, "/v1/runs/run-A", nil, http.StatusOK},
		{"read own logs", http.MethodGet, "/v1/runs/run-A/logs", nil, http.StatusOK},
		{"append own log", http.MethodPost, "/v1/runs/run-A/logs", []byte(`{"message":"hi"}`), http.StatusCreated},
		{"read own state", http.MethodGet, "/v1/integrations/int-A/state", nil, http.StatusOK},
		{"read own state key", http.MethodGet, "/v1/integrations/int-A/state/present", nil, http.StatusOK},
		{"write own state key", http.MethodPut, "/v1/integrations/int-A/state/fresh", []byte(`{"x":1}`), http.StatusOK},
		{"delete own state key", http.MethodDelete, "/v1/integrations/int-A/state/present", nil, http.StatusOK},
		{"read other run", http.MethodGet, "/v1/runs/run-B", nil, http.StatusForbidden},
		{"read other run logs", http.MethodGet, "/v1/runs/run-B/logs", nil, http.StatusForbidden},
		{"append to other run", http.MethodPost, "/v1/runs/run-B/logs", []byte(`{"message":"hi"}`), http.StatusForbidden},
		{"read other integration state", http.MethodGet, "/v1/integrations/int-B/state", nil, http.StatusForbidden},
		{"write other integration state", http.MethodPut, "/v1/integrations/int-B/state/k", []byte(`1`), http.StatusForbidden},
		{"admin list integrations", http.MethodGet, "/v1/integrations", nil, http.StatusForbidden},
		{"admin submit run", http.MethodPost, "/v1/integrations/int-A/runs", nil, http.StatusForbidden},
		{"admin list runs", http.MethodGet, "/v1/runs", nil, http.StatusForbidden},
		{"admin cancel run", http.MethodPost, "/v1/runs/run-A/cancel", nil, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, tc.method, srv.URL+tc.path, tc.body, auth)
			wantStatus(t, r, tc.want)
			if tc.want == http.StatusForbidden {
				if env := r.errorEnvelope(t); env.Error.Code != CodeForbidden {
					t.Fatalf("error code = %q, want %q", env.Error.Code, CodeForbidden)
				}
			}
		})
	}
}

// A run token carries the identity generation it was authorized against. When
// a reset, move, retirement or deletion bumps that generation, a token minted
// before the change can no longer write state -- the token itself stays valid,
// which is exactly why the check has to happen at the mutation.
func TestStateWriteRefusesAStaleGeneration(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	b.integrations["int-A"] = IntegrationView{ID: "int-A", Name: "counter", Valid: true, Generation: 3}
	b.runTokens["stale"] = RunToken{RunID: "run-A", IntegrationID: "int-A", Generation: 2}
	b.seedState("int-A", "count", "1")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	r := do(t, http.MethodPut, srv.URL+"/v1/integrations/int-A/state/count", []byte(`2`),
		map[string]string{"Authorization": "Bearer stale"})
	wantStatus(t, r, http.StatusConflict)
	if env := r.errorEnvelope(t); env.Error.Code != CodeConflict {
		t.Fatalf("error code = %q, want %q", env.Error.Code, CodeConflict)
	}

	// A token at the current generation is unaffected.
	b.runTokens["fresh"] = RunToken{RunID: "run-A", IntegrationID: "int-A", Generation: 3}
	r = do(t, http.MethodPut, srv.URL+"/v1/integrations/int-A/state/count", []byte(`2`),
		map[string]string{"Authorization": "Bearer fresh"})
	wantStatus(t, r, http.StatusOK)
}

func TestWebhookAuthentication(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("hooked", true, "webhook-token")
	b.addIntegration("disabled", true, "")

	// A webhook caller must not need the admin bearer token.
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	cases := []struct {
		name string
		path string
		hdr  map[string]string
		want int
	}{
		{"missing token", "/v1/hooks/hooked", nil, http.StatusUnauthorized},
		{"wrong token", "/v1/hooks/hooked", map[string]string{"X-Otter-Token": "nope"}, http.StatusUnauthorized},
		{"header token", "/v1/hooks/hooked", map[string]string{"X-Otter-Token": "webhook-token"}, http.StatusAccepted},
		{"query token", "/v1/hooks/hooked?token=webhook-token", nil, http.StatusAccepted},
		{"unknown integration", "/v1/hooks/ghost", map[string]string{"X-Otter-Token": "webhook-token"}, http.StatusNotFound},
		{"webhook disabled", "/v1/hooks/disabled", map[string]string{"X-Otter-Token": "webhook-token"}, http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := b.submissionCount()
			r := do(t, http.MethodPost, srv.URL+tc.path, []byte(`{"event":"push"}`), tc.hdr)
			wantStatus(t, r, tc.want)

			switch tc.want {
			case http.StatusAccepted:
				var out SubmitRunResponse
				r.decode(t, &out)
				if out.RunID == "" || out.Status != string(runs.StatusQueued) {
					t.Fatalf("unexpected submit response: %+v", out)
				}
				last, ok := b.lastSubmission()
				if !ok || b.submissionCount() != before+1 {
					t.Fatalf("webhook did not reach SubmitRun")
				}
				if last.integrationID != "hooked" || last.payload.Type != TriggerWebhook {
					t.Fatalf("unexpected submission: %+v", last)
				}
				if string(last.payload.Body) != `{"event":"push"}` {
					t.Fatalf("webhook body = %s, want the original JSON body", last.payload.Body)
				}
				if _, leaked := last.payload.Headers["X-Otter-Token"]; leaked {
					t.Fatalf("webhook token leaked into recorded headers")
				}
			case http.StatusNotFound:
				if b.submissionCount() != before {
					t.Fatalf("rejected webhook must not queue a run")
				}
			}
		})
	}
}

func TestHealthEndpoint(t *testing.T) {
	b := newFakeBackend()
	b.version = "v9.9.9"
	b.startedAt = time.Now().Add(-2 * time.Minute).UTC()
	b.addIntegration("ok", true, "")
	b.addIntegration("bad", false, "")
	b.queueDepth = 3
	b.runCounts = map[string]int{"queued": 1, "running": 2}

	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	r := do(t, http.MethodGet, srv.URL+"/health", nil, nil)
	wantStatus(t, r, http.StatusOK)

	var raw map[string]json.RawMessage
	r.decode(t, &raw)
	for _, key := range []string{"status", "version", "uptime_seconds", "integrations", "queue_depth", "runs"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("health response is missing %q: %s", key, r.body)
		}
	}
	var counts map[string]json.RawMessage
	if err := json.Unmarshal(raw["integrations"], &counts); err != nil {
		t.Fatalf("integrations is not an object: %v", err)
	}
	for _, key := range []string{"total", "valid", "invalid"} {
		if _, ok := counts[key]; !ok {
			t.Fatalf("integrations is missing %q: %s", key, raw["integrations"])
		}
	}

	var health HealthResponse
	r.decode(t, &health)
	if health.Status != "ok" {
		t.Fatalf("status = %q, want ok", health.Status)
	}
	if health.Version != "v9.9.9" {
		t.Fatalf("version = %q", health.Version)
	}
	if health.UptimeSeconds <= 0 {
		t.Fatalf("uptime_seconds = %v, want > 0", health.UptimeSeconds)
	}
	if health.Integrations == nil {
		t.Fatal("an authenticated /health response must include integrations")
	}
	if *health.Integrations != (HealthCounts{Total: 2, Valid: 1, Invalid: 1}) {
		t.Fatalf("integrations = %+v", *health.Integrations)
	}
	if health.QueueDepth == nil || *health.QueueDepth != 3 {
		t.Fatalf("queue_depth = %v, want 3", health.QueueDepth)
	}
	if health.Runs["running"] != 2 || health.Runs["queued"] != 1 {
		t.Fatalf("runs = %+v", health.Runs)
	}
}

// TestHealthHidesCountersFromUnauthenticatedCallers covers the liveness probe:
// it must still answer 200, but must not disclose operational detail when a
// token is configured and none (or a wrong one) was presented.
func TestHealthHidesCountersFromUnauthenticatedCallers(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "s3cret"}, b)
	defer srv.Close()

	healthURL := srv.URL + "/health"

	t.Run("no token", func(t *testing.T) {
		r := do(t, http.MethodGet, healthURL, nil, nil)
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 so liveness probes keep working", r.status)
		}
		var health HealthResponse
		r.decode(t, &health)
		if health.Status != "ok" || health.Version == "" {
			t.Fatalf("liveness payload = %+v", health)
		}
		if health.Integrations != nil || health.QueueDepth != nil || len(health.Runs) != 0 {
			t.Fatalf("unauthenticated /health leaked counters: %s", r.body)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		r := do(t, http.MethodGet, healthURL, nil, map[string]string{"Authorization": "Bearer nope"})
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.status)
		}
		var health HealthResponse
		r.decode(t, &health)
		if health.Integrations != nil {
			t.Fatalf("a wrong token must not reveal counters: %s", r.body)
		}
	})

	t.Run("valid token", func(t *testing.T) {
		r := do(t, http.MethodGet, healthURL, nil, map[string]string{"Authorization": "Bearer s3cret"})
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.status)
		}
		var health HealthResponse
		r.decode(t, &health)
		if health.Integrations == nil || health.QueueDepth == nil {
			t.Fatalf("an authenticated caller should see counters: %s", r.body)
		}
	})
}

func TestSubmitRunEndpoint(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	t.Run("empty body succeeds", func(t *testing.T) {
		before := b.submissionCount()
		r := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/runs", nil, nil)
		wantStatus(t, r, http.StatusAccepted)
		var out SubmitRunResponse
		r.decode(t, &out)
		if out.RunID == "" || out.Status != string(runs.StatusQueued) {
			t.Fatalf("unexpected response: %+v", out)
		}
		last, ok := b.lastSubmission()
		if !ok || b.submissionCount() != before+1 {
			t.Fatalf("SubmitRun was not called")
		}
		if last.integrationID != "int-A" || last.payload.Type != TriggerManual {
			t.Fatalf("unexpected submission: %+v", last)
		}
		if len(last.payload.Body) != 0 {
			t.Fatalf("empty body should not become a trigger body, got %q", last.payload.Body)
		}
	})

	t.Run("invalid json rejected", func(t *testing.T) {
		before := b.submissionCount()
		r := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/runs", []byte(`{"not json`), nil)
		wantStatus(t, r, http.StatusBadRequest)
		if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
			t.Fatalf("error code = %q", env.Error.Code)
		}
		if b.submissionCount() != before {
			t.Fatalf("invalid body must not queue a run")
		}
	})

	t.Run("valid json passed through", func(t *testing.T) {
		body := []byte(`{"a":1,"b":[true,null]}`)
		r := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/runs", body, nil)
		wantStatus(t, r, http.StatusAccepted)
		last, ok := b.lastSubmission()
		if !ok {
			t.Fatalf("SubmitRun was not called")
		}
		if string(last.payload.Body) != string(body) {
			t.Fatalf("trigger body = %s, want %s", last.payload.Body, body)
		}
	})
}

func TestListRunsValidation(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	b.addRun(&runs.Run{ID: "run-1", IntegrationID: "int-A", Status: runs.StatusQueued, Attempt: 1, CreatedAt: time.Now().UTC()})
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	cases := []struct {
		query string
		want  int
	}{
		{"?status=bogus", http.StatusBadRequest},
		{"?limit=0", http.StatusBadRequest},
		{"?limit=abc", http.StatusBadRequest},
		{"?limit=-3", http.StatusBadRequest},
		{"?offset=abc", http.StatusBadRequest},
		{"?offset=-1", http.StatusBadRequest},
		{"?status=queued", http.StatusOK},
		{"?limit=5&offset=0", http.StatusOK},
		{"", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			r := do(t, http.MethodGet, srv.URL+"/v1/runs"+tc.query, nil, nil)
			wantStatus(t, r, tc.want)
			if tc.want == http.StatusBadRequest {
				if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
					t.Fatalf("error code = %q", env.Error.Code)
				}
			}
		})
	}
}

func TestIntegrationEndpointsAndTokenScope(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "wh-secret-token")
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	t.Run("single includes webhook token", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/integrations/int-A", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var view IntegrationView
		r.decode(t, &view)
		if view.ID != "int-A" {
			t.Fatalf("id = %q", view.ID)
		}
		if view.Triggers.WebhookToken != "wh-secret-token" {
			t.Fatalf("webhook_token = %q", view.Triggers.WebhookToken)
		}
		if !bytes.Contains(r.body, []byte(`"webhook_token"`)) {
			t.Fatalf("single integration response has no webhook_token field: %s", r.body)
		}
	})

	t.Run("list never leaks webhook token", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/integrations", nil, nil)
		wantStatus(t, r, http.StatusOK)
		if bytes.Contains(r.body, []byte("wh-secret-token")) {
			t.Fatalf("list response leaked the webhook token: %s", r.body)
		}
		var out struct {
			Integrations []IntegrationView `json:"integrations"`
		}
		r.decode(t, &out)
		if len(out.Integrations) != 1 {
			t.Fatalf("integrations = %d, want 1", len(out.Integrations))
		}
		for _, v := range out.Integrations {
			if v.Triggers.WebhookToken != "" {
				t.Fatalf("listed integration carries a webhook token")
			}
		}
	})

	t.Run("unknown integration is 404", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/integrations/ghost", nil, nil)
		wantStatus(t, r, http.StatusNotFound)
		if env := r.errorEnvelope(t); env.Error.Code != CodeNotFound {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})
}

func TestStateEndpoints(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	b.seedState("int-A", "num", "123")
	b.seedState("int-A", "obj", `{"a":1}`)
	b.seedState("int-A", "gone", `"bye"`)
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	rawCases := []struct {
		key  string
		want string
	}{
		{"num", "123"},
		{"obj", `{"a":1}`},
	}
	for _, tc := range rawCases {
		t.Run("raw "+tc.key, func(t *testing.T) {
			r := do(t, http.MethodGet, srv.URL+"/v1/integrations/int-A/state/"+tc.key, nil, nil)
			wantStatus(t, r, http.StatusOK)
			if got := strings.TrimRight(string(r.body), "\n"); got != tc.want {
				t.Fatalf("body = %q, want raw value %q (no envelope)", got, tc.want)
			}
			if ct := r.header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type = %q", ct)
			}
		})
	}

	t.Run("unknown key is 404", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/integrations/int-A/state/missing", nil, nil)
		wantStatus(t, r, http.StatusNotFound)
		if env := r.errorEnvelope(t); env.Error.Code != CodeNotFound {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})

	t.Run("all state", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/integrations/int-A/state", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var out StateResponse
		r.decode(t, &out)
		if len(out.State) != 3 {
			t.Fatalf("state = %v", out.State)
		}
		if string(out.State["num"]) != "123" {
			t.Fatalf("num = %s", out.State["num"])
		}
	})

	t.Run("put rejects non-json", func(t *testing.T) {
		r := do(t, http.MethodPut, srv.URL+"/v1/integrations/int-A/state/k", []byte("not json"), nil)
		wantStatus(t, r, http.StatusBadRequest)
		if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})

	t.Run("put echoes value", func(t *testing.T) {
		r := do(t, http.MethodPut, srv.URL+"/v1/integrations/int-A/state/k", []byte(`{"x":true}`), nil)
		wantStatus(t, r, http.StatusOK)
		var out SetStateResponse
		r.decode(t, &out)
		if out.IntegrationID != "int-A" || out.Key != "k" {
			t.Fatalf("unexpected response: %+v", out)
		}
		if string(out.Value) != `{"x":true}` {
			t.Fatalf("value = %s", out.Value)
		}
		// The write must be visible to a following read.
		got := do(t, http.MethodGet, srv.URL+"/v1/integrations/int-A/state/k", nil, nil)
		wantStatus(t, got, http.StatusOK)
		if strings.TrimSpace(string(got.body)) != `{"x":true}` {
			t.Fatalf("read back = %s", got.body)
		}
	})

	t.Run("delete then 404", func(t *testing.T) {
		r := do(t, http.MethodDelete, srv.URL+"/v1/integrations/int-A/state/gone", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var out DeleteStateResponse
		r.decode(t, &out)
		if !out.Deleted || out.Key != "gone" {
			t.Fatalf("unexpected response: %+v", out)
		}

		again := do(t, http.MethodDelete, srv.URL+"/v1/integrations/int-A/state/gone", nil, nil)
		wantStatus(t, again, http.StatusNotFound)
		if env := again.errorEnvelope(t); env.Error.Code != CodeNotFound {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})
}

func TestRunLogEndpoints(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	now := time.Now().UTC()
	b.addRun(&runs.Run{ID: "run-1", IntegrationID: "int-A", Status: runs.StatusRunning, Attempt: 1, CreatedAt: now})
	b.addLogs("run-1", runs.LogEntry{ID: 1, RunID: "run-1", Timestamp: now, Stream: runs.StreamOtter, Message: "first"})
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	t.Run("list logs", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-1/logs", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var out struct {
			Logs []runs.LogEntry `json:"logs"`
		}
		r.decode(t, &out)
		if len(out.Logs) != 1 || out.Logs[0].Message != "first" || out.Logs[0].Stream != runs.StreamOtter {
			t.Fatalf("logs = %+v", out.Logs)
		}
	})

	t.Run("after_id validation", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-1/logs?after_id=abc", nil, nil)
		wantStatus(t, r, http.StatusBadRequest)
		if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})

	t.Run("after_id filters", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-1/logs?after_id=1", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var out struct {
			Logs []runs.LogEntry `json:"logs"`
		}
		r.decode(t, &out)
		if len(out.Logs) != 0 {
			t.Fatalf("logs = %+v, want none after id 1", out.Logs)
		}
	})

	t.Run("append log", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs",
			[]byte(`{"stream":"stdout","message":"hello"}`), nil)
		wantStatus(t, r, http.StatusCreated)

		logs := b.logsFor("run-1")
		if len(logs) != 2 {
			t.Fatalf("recorded logs = %+v", logs)
		}
		if logs[1].Stream != runs.StreamStdout || logs[1].Message != "hello" {
			t.Fatalf("recorded entry = %+v", logs[1])
		}
	})

	t.Run("append log defaults stream", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs", []byte(`{"message":"defaulted"}`), nil)
		wantStatus(t, r, http.StatusCreated)
		logs := b.logsFor("run-1")
		if logs[len(logs)-1].Stream != runs.StreamOtter {
			t.Fatalf("stream = %q, want %q", logs[len(logs)-1].Stream, runs.StreamOtter)
		}
	})

	t.Run("rejects bad stream", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs",
			[]byte(`{"stream":"bogus","message":"hi"}`), nil)
		wantStatus(t, r, http.StatusBadRequest)
		if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})

	t.Run("rejects empty message", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs",
			[]byte(`{"stream":"stdout","message":"   "}`), nil)
		wantStatus(t, r, http.StatusBadRequest)
	})

	t.Run("rejects malformed body", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs", []byte(`[1,2,3]`), nil)
		wantStatus(t, r, http.StatusBadRequest)
	})
}

func TestUnknownRouteReturnsJSONEnvelope(t *testing.T) {
	b := newFakeBackend()
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	for _, req := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/does-not-exist"},
		{http.MethodPost, "/nope"},
	} {
		t.Run(req.method+" "+req.path, func(t *testing.T) {
			r := do(t, req.method, srv.URL+req.path, nil, nil)
			wantStatus(t, r, http.StatusNotFound)
			if ct := r.header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type = %q, want JSON", ct)
			}
			if env := r.errorEnvelope(t); env.Error.Code != CodeNotFound {
				t.Fatalf("error code = %q", env.Error.Code)
			}
		})
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	big := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	r := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/runs", big, nil)
	wantStatus(t, r, http.StatusBadRequest)
	if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
		t.Fatalf("error code = %q", env.Error.Code)
	}
}

func TestReloadEndpointIsAdminOnlyAndReturnsTheResult(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")
	b.runTokens["run-token"] = RunToken{RunID: "run-A", IntegrationID: "int-A"}
	b.reloadOut = ReloadResult{
		Added:         []string{"int-B"},
		Total:         2,
		Valid:         2,
		RunsCancelled: 1,
	}

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	admin := map[string]string{"Authorization": "Bearer admin-secret"}

	// A per-run token is authenticated but not an admin. Reload changes what
	// the whole runtime can address, so it is behind the admin token.
	forbidden := do(t, http.MethodPost, srv.URL+"/v1/reload", nil,
		map[string]string{"Authorization": "Bearer run-token"})
	wantStatus(t, forbidden, http.StatusForbidden)
	if env := forbidden.errorEnvelope(t); env.Error.Code != CodeForbidden {
		t.Errorf("error code = %q, want %q", env.Error.Code, CodeForbidden)
	}

	ok := do(t, http.MethodPost, srv.URL+"/v1/reload", nil, admin)
	wantStatus(t, ok, http.StatusOK)

	var got ReloadResult
	ok.decode(t, &got)
	if len(got.Added) != 1 || got.Added[0] != "int-B" {
		t.Errorf("added = %v, want [int-B]", got.Added)
	}
	if got.RunsCancelled != 1 {
		t.Errorf("runs_cancelled = %d, want 1", got.RunsCancelled)
	}
	if b.reloads != 1 {
		t.Errorf("backend reloads = %d, want 1", b.reloads)
	}

	// A reload already in progress is a conflict, not a server fault.
	b.mu.Lock()
	b.reloadErr = ErrConflict
	b.mu.Unlock()

	conflict := do(t, http.MethodPost, srv.URL+"/v1/reload", nil, admin)
	wantStatus(t, conflict, http.StatusConflict)
	if env := conflict.errorEnvelope(t); env.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", env.Error.Code, CodeConflict)
	}
}
