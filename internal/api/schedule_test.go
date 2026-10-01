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
