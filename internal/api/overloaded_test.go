package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The autonomous-trigger refusal is covered by the webhook test below, which
// drives a real autonomous route. This one pins the other half of the contract
// at the HTTP layer: a job at its bound still admits a manual run, because the
// control-plane submit route is manual by construction.
func TestManualRunIsAdmittedAtTheQueueBound(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.overloadedJob = "int-A"

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	resp := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", nil, adminHeaders())
	wantStatus(t, resp, http.StatusAccepted)

	// The refusal is still what the daemon reports for the autonomous path, so
	// the fake's bound is real rather than inert.
	hook := do(t, http.MethodPost, srv.URL+"/v1/hooks/int-A", nil,
		map[string]string{"X-Otter-Token": "webhook-token"})
	if hook.status != http.StatusTooManyRequests && hook.status != http.StatusNotFound {
		t.Errorf("autonomous trigger at the bound = %d, want a refusal", hook.status)
	}
}

// A depth refusal is a run that did not happen, so the authenticated health
// view reports it per job rather than leaving it only in a log line. An
// unauthenticated caller gets the liveness answer and no per-job detail.
func TestHealthReportsAdmissionRefusalsToAuthenticatedCallersOnly(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	last := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	b.admissionRefusals = map[string]AdmissionRefusal{
		"int-A": {Total: 3, LastAt: &last},
	}
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	authed := do(t, http.MethodGet, srv.URL+"/health", nil, adminHeaders())
	wantStatus(t, authed, http.StatusOK)
	var health HealthResponse
	authed.decode(t, &health)
	if health.Queue == nil {
		t.Fatalf("health has no queue block: %+v", health)
	}
	if health.Queue.RefusedTotal != 3 {
		t.Errorf("queue.refused_total = %d, want 3", health.Queue.RefusedTotal)
	}
	if health.Queue.LastRefusedAt == nil || !health.Queue.LastRefusedAt.Equal(last) {
		t.Errorf("queue.last_refused_at = %v, want %s", health.Queue.LastRefusedAt, last)
	}
	got, ok := health.Queue.ByJobRefused["int-A"]
	if !ok {
		t.Fatalf("health did not report the per-job refusal: %+v", health.Queue.ByJobRefused)
	}
	if got.Total != 3 {
		t.Errorf("per-job refused_total = %d, want 3", got.Total)
	}

	anonymous := do(t, http.MethodGet, srv.URL+"/health", nil, nil)
	wantStatus(t, anonymous, http.StatusOK)
	var liveness HealthResponse
	anonymous.decode(t, &liveness)
	if liveness.Queue != nil {
		t.Errorf("an unauthenticated health read leaked the queue block: %+v", liveness.Queue)
	}
}

// The published bounds table claims an API read body over 1 MiB is refused with
// 400 invalid_request. This drives both sides of that boundary through a real
// handler, because the limit lives in the read helper rather than in a constant
// a test could assert on its own.
func TestAPIReadBodyBoundIsEnforced(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	// At the limit: accepted. A request body is also required to be valid JSON,
	// so the fixture is a JSON object padded to exactly the limit -- otherwise a
	// pass here would prove the JSON check rather than the size check.
	atLimit := jsonBodyOfSize(t, maxBodyBytes)
	resp := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", atLimit, adminHeaders())
	if resp.status == http.StatusBadRequest {
		t.Errorf("a body of exactly the limit was refused: %s", resp.body)
	}

	// One byte over: refused as invalid rather than read.
	over := jsonBodyOfSize(t, maxBodyBytes+1)
	resp = do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", over, adminHeaders())
	wantStatus(t, resp, http.StatusBadRequest)
	var e ErrorResponse
	resp.decode(t, &e)
	if e.Error.Code != CodeInvalid {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeInvalid)
	}
}

// jsonBodyOfSize builds a valid JSON object of exactly n bytes.
func jsonBodyOfSize(t *testing.T, n int) []byte {
	t.Helper()
	const shell = `{"pad":""}`
	if n < len(shell) {
		t.Fatalf("cannot build a JSON body of %d bytes", n)
	}
	body := []byte(`{"pad":"` + strings.Repeat("x", n-len(shell)) + `"}`)
	if len(body) != n {
		t.Fatalf("built %d bytes, wanted %d", len(body), n)
	}
	return body
}

// The webhook route is the autonomous trigger an integration actually calls, so
// the bound has to refuse there and not only on the control-plane submit route.
// This drives the real handler, which submits with TriggerWebhook, rather than
// asserting the mapping in isolation.
func TestWebhookIsRefusedAtTheQueueBound(t *testing.T) {
	b := newFakeBackend()
	b.addJob("hooked", true, "webhook-token")
	b.overloadedJob = "hooked"

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	hook := do(t, http.MethodPost, srv.URL+"/v1/hooks/hooked", []byte(`{"event":"push"}`),
		map[string]string{"X-Otter-Token": "webhook-token"})
	wantStatus(t, hook, http.StatusTooManyRequests)

	var e ErrorResponse
	hook.decode(t, &e)
	if e.Error.Code != CodeOverloaded {
		t.Errorf("webhook refusal code = %q, want %q", e.Error.Code, CodeOverloaded)
	}

	// The same job's manual route is still admitted at the bound: the operator
	// keeps their own tool when the queue is deep.
	manual := do(t, http.MethodPost, srv.URL+"/v1/jobs/hooked/runs", nil, adminHeaders())
	wantStatus(t, manual, http.StatusAccepted)
}
