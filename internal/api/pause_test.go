package api

import (
	"net/http"
	"testing"

	"github.com/tkoizumi/otter/internal/runs"
)

// Pause and resume are admin-only, idempotent, and answer with the resulting
// trigger state rather than just "ok", so a caller can log what changed.
func TestPauseEndpointRoundTrip(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	// Admin-only: pause changes what every trigger of an integration does.
	anonymous := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/pause", nil, nil)
	wantStatus(t, anonymous, http.StatusUnauthorized)

	paused := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/pause", nil, adminHeaders())
	wantStatus(t, paused, http.StatusOK)

	var view PauseView
	paused.decode(t, &view)
	if !view.Paused || !view.Changed {
		t.Fatalf("pause response = %+v, want paused and changed", view)
	}
	if view.IntegrationID != "int-A" {
		t.Errorf("pause response = %+v, want int-A", view)
	}
	if view.Since == nil {
		t.Error("pause response should say when the pause began")
	}

	// The single-integration endpoint carries the state a reader needs.
	got := do(t, http.MethodGet, srv.URL+"/v1/integrations/int-A", nil, adminHeaders())
	wantStatus(t, got, http.StatusOK)
	var integration IntegrationView
	got.decode(t, &integration)
	if !integration.Triggers.Paused {
		t.Errorf("integration triggers = %+v, want the pause", integration.Triggers)
	}

	// Repeating the pause is a successful no-op.
	repeated := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/pause", nil, adminHeaders())
	wantStatus(t, repeated, http.StatusOK)
	var repeatView PauseView
	repeated.decode(t, &repeatView)
	if repeatView.Changed {
		t.Error("a repeated pause should report changed false")
	}

	resumed := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/resume", nil, adminHeaders())
	wantStatus(t, resumed, http.StatusOK)
	var resumeView PauseView
	resumed.decode(t, &resumeView)
	if resumeView.Paused || !resumeView.Changed {
		t.Fatalf("resume response = %+v, want enabled and changed", resumeView)
	}
	if resumeView.Since != nil {
		t.Error("an enabled integration should not report a pause instant")
	}
}

// Neither verb takes a body, and one that is sent anyway is simply not read:
// the request still means "pause this integration".
func TestPauseEndpointIgnoresABody(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	r := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/pause",
		[]byte(`{"reason":"ignored"}`), adminHeaders())
	wantStatus(t, r, http.StatusOK)
}

func TestPauseEndpointMapsBackendErrors(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("int-A", true, "")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	missing := do(t, http.MethodPost, srv.URL+"/v1/integrations/ghost/pause", nil, adminHeaders())
	wantStatus(t, missing, http.StatusNotFound)

	// A retired identity is a conflict: the request is well formed and the
	// integration exists, but it cannot carry this control.
	b.pauseErr = ErrConflict
	refused := do(t, http.MethodPost, srv.URL+"/v1/integrations/int-A/pause", nil, adminHeaders())
	wantStatus(t, refused, http.StatusConflict)
	if code := refused.errorEnvelope(t).Error.Code; code != CodeConflict {
		t.Errorf("error code = %q, want %q", code, CodeConflict)
	}
}

// A paused integration is not accepting autonomous triggers, so the webhook is
// 503 -- the route exists and the caller is authorized, but this integration is
// not taking work. A manual run is the operator asking for one and still works.
func TestPausedWebhookIsUnavailableButManualRunsStillWork(t *testing.T) {
	b := newFakeBackend()
	b.addIntegration("hooked", true, "webhook-token")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	pause := do(t, http.MethodPost, srv.URL+"/v1/integrations/hooked/pause", nil, adminHeaders())
	wantStatus(t, pause, http.StatusOK)

	before := b.submissionCount()
	hook := do(t, http.MethodPost, srv.URL+"/v1/hooks/hooked", []byte(`{"event":"push"}`),
		map[string]string{"X-Otter-Token": "webhook-token"})
	wantStatus(t, hook, http.StatusServiceUnavailable)
	if code := hook.errorEnvelope(t).Error.Code; code != CodeUnavailable {
		t.Errorf("webhook error code = %q, want %q", code, CodeUnavailable)
	}
	if b.submissionCount() != before {
		t.Error("a paused webhook still reached SubmitRun")
	}

	manual := do(t, http.MethodPost, srv.URL+"/v1/integrations/hooked/runs", []byte(`{}`), adminHeaders())
	wantStatus(t, manual, http.StatusAccepted)
	var accepted SubmitRunResponse
	manual.decode(t, &accepted)
	if accepted.RunID == "" || accepted.Status != string(runs.StatusQueued) {
		t.Fatalf("manual run while paused = %+v, want a queued run", accepted)
	}
}
