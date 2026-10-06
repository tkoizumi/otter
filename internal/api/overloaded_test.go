package api

import (
	"net/http"
	"testing"
	"time"
)

// A job at its max_queue_depth refuses a trigger with 429, a machine-readable
// code, and a retry hint. 429 rather than 503: unlike a pause, there is a known
// relief -- the queue drains -- so the caller is told to retry rather than that
// the job is closed. The daemon decides which triggers the bound applies to and
// admits manual runs past it; that half is asserted in internal/daemon.
func TestOverloadedTriggerReturnsTooManyRequests(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.overloadedJob = "int-A"

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	resp := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", nil, adminHeaders())
	wantStatus(t, resp, http.StatusTooManyRequests)
	if got := resp.header.Get("Retry-After"); got == "" {
		t.Error("an overloaded response should carry Retry-After, because the relief is known")
	}
	var e ErrorResponse
	resp.decode(t, &e)
	if e.Error.Code != CodeOverloaded {
		t.Errorf("error code = %q, want %q", e.Error.Code, CodeOverloaded)
	}
	if e.Error.Message == "" {
		t.Error("an overloaded response should explain which bound was reached")
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
	got, ok := health.AdmissionRefusals["int-A"]
	if !ok {
		t.Fatalf("health did not report the refusal: %+v", health.AdmissionRefusals)
	}
	if got.Total != 3 {
		t.Errorf("refused_total = %d, want 3", got.Total)
	}
	if got.LastAt == nil || !got.LastAt.Equal(last) {
		t.Errorf("last_refused_at = %v, want %s", got.LastAt, last)
	}

	anonymous := do(t, http.MethodGet, srv.URL+"/health", nil, nil)
	wantStatus(t, anonymous, http.StatusOK)
	var liveness HealthResponse
	anonymous.decode(t, &liveness)
	if len(liveness.AdmissionRefusals) != 0 {
		t.Errorf("an unauthenticated health read leaked per-job refusals: %+v", liveness.AdmissionRefusals)
	}
}
