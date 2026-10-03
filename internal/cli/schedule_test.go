package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
)

// scheduleRecorder is a daemon stand-in for the schedule family. It records the
// last request and answers with a canned schedule view.
type scheduleRecorder struct {
	method string
	path   string
	body   string
	header http.Header
	status int
	view   api.ScheduleView
	list   api.ScheduleList
}

func (s *scheduleRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.method = r.Method
		s.path = r.URL.Path
		s.header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		s.body = string(raw)

		status := s.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		if status >= 400 {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"code":"conflict","message":"nope"}}`)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/schedules") {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(s.list)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(s.view)
	}))
}

func runSchedule(t *testing.T, rec *scheduleRecorder, args ...string) (int, string) {
	t.Helper()
	srv := rec.server(t)
	defer srv.Close()

	var out bytes.Buffer
	app := New("test", &out, &out)
	code := app.cmdSchedule(context.Background(), globals{api: srv.URL}, args)
	return code, out.String()
}

// `otter schedule add` posts to the job's schedule collection, sends an
// idempotency key so a retry cannot duplicate, and prints the new schedule.
func TestScheduleAddPostsWithIdempotencyKey(t *testing.T) {
	rec := &scheduleRecorder{view: api.ScheduleView{
		ID: "sched-1", JobID: "counter", Cron: "@hourly", Origin: "api", Changed: true,
	}}
	withWorkingDir(t, writeManifest(t, "counter"))

	code, out := runSchedule(t, rec, "add", "@hourly")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/jobs/counter/schedules" {
		t.Fatalf("request = %s %s, want POST /v1/jobs/counter/schedules", rec.method, rec.path)
	}
	if rec.header.Get("Idempotency-Key") == "" {
		t.Error("schedule add sent no Idempotency-Key")
	}
	var sent api.ScheduleCreateRequest
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatalf("decode body %q: %v", rec.body, err)
	}
	if sent.Cron != "@hourly" {
		t.Errorf("create body cron = %q, want @hourly", sent.Cron)
	}
	if !strings.Contains(out, "id: sched-1") {
		t.Errorf("output does not name the new schedule:\n%s", out)
	}
}

// The same add twice derives the same idempotency key, which is what makes a
// re-run of the same script converge instead of duplicating.
func TestScheduleAddDerivesAStableKey(t *testing.T) {
	first := &scheduleRecorder{view: api.ScheduleView{ID: "sched-1", JobID: "counter", Cron: "@hourly", Changed: true}}
	withWorkingDir(t, writeManifest(t, "counter"))
	if code, out := runSchedule(t, first, "add", "@hourly"); code != 0 {
		t.Fatalf("first add failed (%d):\n%s", code, out)
	}

	second := &scheduleRecorder{view: api.ScheduleView{ID: "sched-1", JobID: "counter", Cron: "@hourly", Changed: false}}
	if code, out := runSchedule(t, second, "add", "@hourly"); code != 0 {
		t.Fatalf("second add failed (%d):\n%s", code, out)
	}
	if first.header.Get("Idempotency-Key") != second.header.Get("Idempotency-Key") {
		t.Errorf("identical adds derived different keys: %q vs %q",
			first.header.Get("Idempotency-Key"), second.header.Get("Idempotency-Key"))
	}
}

// list, pause, resume, remove and update each hit the schedule-id route.
func TestScheduleSubcommandsRouteByScheduleID(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		method string
		path   string
		want   string
	}{
		{"list", []string{"list", "counter"}, http.MethodGet, "/v1/jobs/counter/schedules", "*/5 * * * *"},
		{"pause", []string{"pause", "sched-1"}, http.MethodPost, "/v1/schedules/sched-1/pause", "id: sched-1"},
		{"resume", []string{"resume", "sched-1"}, http.MethodPost, "/v1/schedules/sched-1/resume", "id: sched-1"},
		{"remove", []string{"remove", "sched-1"}, http.MethodDelete, "/v1/schedules/sched-1", "removed sched-1"},
		{"update", []string{"update", "sched-1", "--cron", "@daily"}, http.MethodPatch, "/v1/schedules/sched-1", "id: sched-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &scheduleRecorder{
				view: api.ScheduleView{ID: "sched-1", JobID: "counter", Cron: "*/5 * * * *", Origin: "api", Changed: true},
				list: api.ScheduleList{Schedules: []api.ScheduleView{
					{ID: "sched-1", JobID: "counter", Cron: "*/5 * * * *", Origin: "api"},
				}},
			}
			code, out := runSchedule(t, rec, tc.args...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
			}
			if rec.method != tc.method || rec.path != tc.path {
				t.Fatalf("request = %s %s, want %s %s", rec.method, rec.path, tc.method, tc.path)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output = %q, want it to contain %q", out, tc.want)
			}
		})
	}
}

// A conflict from the daemon (a manifest-owned row) is reported, not swallowed.
func TestScheduleUpdateReportsConflict(t *testing.T) {
	rec := &scheduleRecorder{status: http.StatusConflict}
	code, out := runSchedule(t, rec, "update", "sched-1", "--cron", "@daily")
	if code == 0 {
		t.Fatalf("a 409 exited 0; output:\n%s", out)
	}
	if !strings.Contains(out, "nope") {
		t.Errorf("output does not carry the daemon's message:\n%s", out)
	}
}
