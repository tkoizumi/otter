package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Activation is the most privileged operation the runtime has -- it decides
// whether customer work runs at all -- so a control or read credential must not
// be able to reach it, in either direction.
func TestMaintenanceEndpointsAreAdminOnly(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	defer srv.Close()

	control := mintToken(t, srv, "gateway", ScopeControl)
	read := mintToken(t, srv, "viewer", ScopeRead)

	for _, tc := range []struct {
		name   string
		method string
		token  string
	}{
		{"enter with control", http.MethodPost, control.Token},
		{"enter with read", http.MethodPost, read.Token},
		{"exit with control", http.MethodDelete, control.Token},
		{"exit with read", http.MethodDelete, read.Token},
		{"read with control", http.MethodGet, control.Token},
		{"read with read", http.MethodGet, read.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := do(t, tc.method, srv.URL+"/v1/runtime/maintenance", nil, bearer(tc.token))
			wantStatus(t, res, http.StatusForbidden)
		})
	}

	// Anonymous is refused too, rather than being treated as admin on loopback.
	anon := do(t, http.MethodPost, srv.URL+"/v1/runtime/maintenance", nil, nil)
	wantStatus(t, anon, http.StatusUnauthorized)
}

// The round trip: an admin enters maintenance, the state reports the gate, and
// an admin activates it again. The response carries the resulting state rather
// than a bare success, because "which mode did it land in" is the fact an
// operator needs while waiting on a window.
func TestMaintenanceRoundTrip(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	defer srv.Close()

	body, err := json.Marshal(enterMaintenanceRequest{Reason: "planned upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	entered := do(t, http.MethodPost, srv.URL+"/v1/runtime/maintenance", body, adminHeaders())
	wantStatus(t, entered, http.StatusOK)
	var view MaintenanceView
	entered.decode(t, &view)
	if view.AcceptingWork {
		t.Error("the runtime still reports accepting work after entering maintenance")
	}
	if view.Reason != "planned upgrade" {
		t.Errorf("reason = %q, want the caller's", view.Reason)
	}

	got := do(t, http.MethodGet, srv.URL+"/v1/runtime/maintenance", nil, adminHeaders())
	wantStatus(t, got, http.StatusOK)
	var read MaintenanceView
	got.decode(t, &read)
	if read.Mode != view.Mode || read.AcceptingWork != view.AcceptingWork {
		t.Errorf("GET disagrees with POST: %+v vs %+v", read, view)
	}

	exited := do(t, http.MethodDelete, srv.URL+"/v1/runtime/maintenance", nil, adminHeaders())
	wantStatus(t, exited, http.StatusOK)
	var serving MaintenanceView
	exited.decode(t, &serving)
	if !serving.AcceptingWork {
		t.Error("the runtime does not report accepting work after activation")
	}
}

// An empty body is legal. A deploy script that only needs the gate must not
// have to invent a reason to get one.
func TestEnterMaintenanceAcceptsAnEmptyBody(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	defer srv.Close()

	res := do(t, http.MethodPost, srv.URL+"/v1/runtime/maintenance", nil, adminHeaders())
	wantStatus(t, res, http.StatusOK)
}

// A gate is not a liveness failure. An unauthenticated probe asks whether the
// process is up, and "held back on purpose" must still answer that it is --
// otherwise a monitor restarts a runtime an operator deliberately gated.
func TestHealthStaysOkWhileGatedAndReportsTheReason(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.maintenance = MaintenanceView{
		Mode: "maintenance", AcceptingWork: false, Explicit: true,
		Since: "2026-10-05T12:00:00Z", Reason: "snapshot",
	}
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	defer srv.Close()

	// Anonymous: liveness only, and still ok.
	anon := do(t, http.MethodGet, srv.URL+"/health", nil, nil)
	wantStatus(t, anon, http.StatusOK)
	var liveness HealthResponse
	anon.decode(t, &liveness)
	if liveness.Status != "ok" {
		t.Errorf("status = %q, want ok: a gated runtime is alive", liveness.Status)
	}
	if liveness.Maintenance != nil {
		t.Error("an unauthenticated health read leaked the maintenance state")
	}

	// Authenticated: the same liveness answer, plus what an operator needs to
	// tell "held back" from "broken".
	authed := do(t, http.MethodGet, srv.URL+"/health", nil, adminHeaders())
	wantStatus(t, authed, http.StatusOK)
	var health HealthResponse
	authed.decode(t, &health)
	if health.Status != "ok" {
		t.Errorf("authenticated status = %q, want ok", health.Status)
	}
	if health.Maintenance == nil {
		t.Fatal("health did not report the maintenance state")
	}
	if health.Maintenance.AcceptingWork {
		t.Error("health reports accepting work while gated")
	}
	if health.Maintenance.Reason != "snapshot" {
		t.Errorf("reason = %q, want it reported", health.Maintenance.Reason)
	}
}

// A submission refused by the gate answers 503 with a machine-readable code,
// the same shape a paused job uses, so a caller can tell "come back later" from
// "your request was wrong".
func TestGatedSubmissionAnswersServiceUnavailable(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.gated = true
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, b)
	defer srv.Close()

	res := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", nil, adminHeaders())
	wantStatus(t, res, http.StatusServiceUnavailable)
	var e ErrorResponse
	res.decode(t, &e)
	if e.Error.Code != CodeUnavailable {
		t.Errorf("code = %q, want %q", e.Error.Code, CodeUnavailable)
	}
}
