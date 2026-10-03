package api

import (
	"net/http"
	"testing"
)

// Changing a cadence is admin-only and answers with the resulting schedule, so
// a caller can log what changed. The schedule is runtime state, which is why it
// has its own endpoint rather than living in the manifest.
func TestScheduleEndpointRoundTrip(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	// Admin-only: a cadence decides when code runs.
	anonymous := do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/schedule",
		[]byte(`{"cron":"*/15 * * * *"}`), nil)
	wantStatus(t, anonymous, http.StatusUnauthorized)

	set := do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/schedule",
		[]byte(`{"cron":"*/15 * * * *"}`), adminHeaders())
	wantStatus(t, set, http.StatusOK)

	var view ScheduleView
	set.decode(t, &view)
	if view.Cron != "*/15 * * * *" || !view.Changed {
		t.Fatalf("schedule response = %+v, want the new cron marked changed", view)
	}
	if view.JobID != "int-A" {
		t.Errorf("schedule response = %+v, want int-A", view)
	}

	// Repeating the same value is a successful no-op, so a deploy script can
	// apply it unconditionally.
	repeated := do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/schedule",
		[]byte(`{"cron":"*/15 * * * *"}`), adminHeaders())
	wantStatus(t, repeated, http.StatusOK)
	var repeatView ScheduleView
	repeated.decode(t, &repeatView)
	if repeatView.Changed {
		t.Error("an unchanged cadence should report changed false")
	}

	// The single-job endpoint carries the cadence a reader needs.
	got := do(t, http.MethodGet, srv.URL+"/v1/jobs/int-A", nil, adminHeaders())
	wantStatus(t, got, http.StatusOK)
	var job JobView
	got.decode(t, &job)
	if job.Triggers.Cron != "*/15 * * * *" {
		t.Errorf("job triggers = %+v, want the stored cadence", job.Triggers)
	}

	// Clearing is its own request, and it reports an empty cadence rather than
	// falling back to anything.
	cleared := do(t, http.MethodDelete, srv.URL+"/v1/jobs/int-A/schedule", nil, adminHeaders())
	wantStatus(t, cleared, http.StatusOK)
	var clearedView ScheduleView
	cleared.decode(t, &clearedView)
	if clearedView.Cron != "" {
		t.Fatalf("cleared schedule = %+v, want an empty cron", clearedView)
	}

	// The handler forwards the empty value rather than inventing a default.
	last := b.scheduleCalls[len(b.scheduleCalls)-1]
	if last.cron != "" {
		t.Errorf("backend saw cron %q on clear, want empty", last.cron)
	}
}

// A malformed body is a bad request, and it must not reach the backend: a
// rejected schedule change should leave the stored cadence alone.
func TestScheduleEndpointRejectsABadBody(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	bad := do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/schedule",
		[]byte(`{`), adminHeaders())
	wantStatus(t, bad, http.StatusBadRequest)

	if len(b.scheduleCalls) != 0 {
		t.Fatalf("a malformed body reached the backend: %+v", b.scheduleCalls)
	}
}

func TestScheduleEndpointMapsBackendErrors(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	missing := do(t, http.MethodPut, srv.URL+"/v1/jobs/ghost/schedule",
		[]byte(`{"cron":"@daily"}`), adminHeaders())
	wantStatus(t, missing, http.StatusNotFound)

	// An invalid expression is the caller's mistake, so it is a 400 and not a
	// conflict.
	b.scheduleErr = ErrInvalid
	invalid := do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/schedule",
		[]byte(`{"cron":"nonsense"}`), adminHeaders())
	wantStatus(t, invalid, http.StatusBadRequest)
	if code := invalid.errorEnvelope(t).Error.Code; code != CodeInvalid {
		t.Errorf("error code = %q, want %q", code, CodeInvalid)
	}
}

// The schedule-id surface round-trips: create, list, get, patch, pause, resume,
// delete. It is the CL-22 API the CLI and the control plane are written
// against.
func TestScheduleIdEndpointsRoundTrip(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	create := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/schedules",
		[]byte(`{"cron":"@daily","payload":{"dataset":7}}`), adminHeaders())
	wantStatus(t, create, http.StatusCreated)
	var view ScheduleView
	create.decode(t, &view)
	if view.ID == "" || view.Cron != "@daily" || !view.Changed {
		t.Fatalf("created schedule = %+v", view)
	}

	// The same Idempotency-Key is a replay: the same schedule, no second row.
	hdr := adminHeaders()
	hdr["Idempotency-Key"] = "cmd-1"
	first := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/schedules",
		[]byte(`{"cron":"@daily"}`), hdr)
	wantStatus(t, first, http.StatusCreated)
	var firstView ScheduleView
	first.decode(t, &firstView)

	replay := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/schedules",
		[]byte(`{"cron":"@daily"}`), hdr)
	wantStatus(t, replay, http.StatusOK)
	var replayView ScheduleView
	replay.decode(t, &replayView)
	if replayView.ID != firstView.ID || replayView.Changed {
		t.Fatalf("idempotent replay = %+v, want the existing row unchanged", replayView)
	}

	list := do(t, http.MethodGet, srv.URL+"/v1/jobs/int-A/schedules", nil, adminHeaders())
	wantStatus(t, list, http.StatusOK)
	var listed ScheduleList
	list.decode(t, &listed)
	if len(listed.Schedules) != 2 {
		t.Fatalf("listed schedules = %d, want 2", len(listed.Schedules))
	}

	got := do(t, http.MethodGet, srv.URL+"/v1/schedules/"+view.ID, nil, adminHeaders())
	wantStatus(t, got, http.StatusOK)

	patch := do(t, http.MethodPatch, srv.URL+"/v1/schedules/"+view.ID,
		[]byte(`{"cron":"@hourly"}`), adminHeaders())
	wantStatus(t, patch, http.StatusOK)
	var patched ScheduleView
	patch.decode(t, &patched)
	if patched.Cron != "@hourly" || !patched.Changed {
		t.Fatalf("patched schedule = %+v", patched)
	}

	paused := do(t, http.MethodPost, srv.URL+"/v1/schedules/"+view.ID+"/pause", nil, adminHeaders())
	wantStatus(t, paused, http.StatusOK)
	var pausedView ScheduleView
	paused.decode(t, &pausedView)
	if !pausedView.Paused {
		t.Fatalf("pause view = %+v, want paused", pausedView)
	}

	resumed := do(t, http.MethodPost, srv.URL+"/v1/schedules/"+view.ID+"/resume", nil, adminHeaders())
	wantStatus(t, resumed, http.StatusOK)

	removed := do(t, http.MethodDelete, srv.URL+"/v1/schedules/"+view.ID, nil, adminHeaders())
	wantStatus(t, removed, http.StatusNoContent)

	gone := do(t, http.MethodGet, srv.URL+"/v1/schedules/"+view.ID, nil, adminHeaders())
	wantStatus(t, gone, http.StatusNotFound)
}

// A manifest-owned row is refused with 409, so the API cannot fight a reload.
func TestManifestOwnedScheduleRefusesMutation(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.schedByID["sched-m"] = ScheduleView{ID: "sched-m", JobID: "int-A", Cron: "@daily", Origin: "manifest"}

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	patch := do(t, http.MethodPatch, srv.URL+"/v1/schedules/sched-m",
		[]byte(`{"cron":"@hourly"}`), adminHeaders())
	wantStatus(t, patch, http.StatusConflict)

	del := do(t, http.MethodDelete, srv.URL+"/v1/schedules/sched-m", nil, adminHeaders())
	wantStatus(t, del, http.StatusConflict)
}

// The create body is validated at the edge: an empty cron never reaches the
// backend, and a control-scoped token is required.
func TestScheduleCreateValidatesAndScopes(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	empty := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/schedules",
		[]byte(`{"cron":"  "}`), adminHeaders())
	wantStatus(t, empty, http.StatusBadRequest)

	anonymous := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/schedules",
		[]byte(`{"cron":"@daily"}`), nil)
	wantStatus(t, anonymous, http.StatusUnauthorized)

	read := mintToken(t, srv, "reader", ScopeRead)
	forbidden := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/schedules",
		[]byte(`{"cron":"@daily"}`), bearer(read.Token))
	wantStatus(t, forbidden, http.StatusForbidden)
}
