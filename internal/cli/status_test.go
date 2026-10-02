package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
)

func timePtr(t time.Time) *time.Time { return &t }

// The stalest success -- and a job that never succeeded -- must be what a
// reader sees first, because that is the signal the line exists for.
func TestFreshnessLineLeadsWithTheStalest(t *testing.T) {
	now := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	got := freshnessLine([]api.HealthFreshness{
		{JobID: "b", Name: "b", LastSuccessAt: timePtr(now.Add(-5 * time.Minute))},
		{JobID: "a", Name: "a", LastSuccessAt: timePtr(now.Add(-2 * time.Hour))},
		{JobID: "c", Name: "c"},
	}, now)

	want := "c never, a 2h0m0s ago, b 5m0s ago"
	if got != want {
		t.Errorf("freshnessLine() = %q, want %q", got, want)
	}
}

func TestFreshnessLineCapsALongJobList(t *testing.T) {
	now := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	entries := make([]api.HealthFreshness, 0, 6)
	for i := 0; i < 6; i++ {
		entries = append(entries, api.HealthFreshness{
			JobID:         fmt.Sprintf("job-%d", i),
			LastSuccessAt: timePtr(now.Add(-time.Duration(i) * time.Minute)),
		})
	}

	got := freshnessLine(entries, now)
	want := "job-5 5m0s ago, job-4 4m0s ago, job-3 3m0s ago, job-2 2m0s ago, +2 more"
	if got != want {
		t.Errorf("freshnessLine() = %q, want %q", got, want)
	}
}

func TestBacklogLineIsDeepestFirst(t *testing.T) {
	got := backlogLine(map[string]int{"a": 2, "b": 5, "c": 1})
	if want := "b 5, a 2, c 1"; got != want {
		t.Errorf("backlogLine() = %q, want %q", got, want)
	}
}

func TestFormatBytesUsesBinaryUnits(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{4096, "4.0 KiB"},
		{1 << 20, "1.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{2 << 30, "2.0 GiB"},
	}
	for _, tc := range tests {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// cmdStatus must print the signals the daemon reports, not just the counts it
// printed before: an operator reading the terminal should not have to curl
// /health to see the queue's age.
func TestStatusPrintsTheOperationalSignals(t *testing.T) {
	// The freshness instant is relative to the test clock, because the status
	// line renders an age against "now".
	lastSuccess := time.Now().UTC().Add(-3 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":"v0.2.0","uptime_seconds":90,
			"jobs":{"total":1,"valid":1,"invalid":0},"queue_depth":3,
			"runs":{"running":1,"queued":2},
			"queue":{"oldest_waiting_at":"2026-10-01T11:49:00Z","oldest_waiting_seconds":660,
				"by_job":{"shopify":2,"report":1},"retrying":1,"next_retry_at":%q},
			"freshness":[{"job_id":"shopify","name":"shopify","last_success_at":%q,"age_seconds":180}],
			"storage":{"db_bytes":4096,"disk_free_bytes":1073741824,"disk_total_bytes":2147483648}}`,
			time.Now().UTC().Add(4*time.Minute).Format(time.RFC3339Nano),
			lastSuccess.Format(time.RFC3339Nano))
	}))
	defer srv.Close()

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.cmdStatus(context.Background(), globals{api: srv.URL}); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}

	text := out.String()
	for _, want := range []string{
		"queue depth:   3",
		"queue age:     oldest waiting 11m0s",
		"retries:       1 retrying, next in ",
		"backlog:       shopify 2, report 1",
		"storage:       db 4.0 KiB, disk 1.0 GiB free of 2.0 GiB",
		"last success:  shopify ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status output is missing %q:\n%s", want, text)
		}
	}

	// The age is rendered from the instant, so it must be the instant's age.
	match := regexp.MustCompile(`last success:\s+shopify (\S+) ago`).FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("status output has no freshness age:\n%s", text)
	}
	age, err := time.ParseDuration(match[1])
	if err != nil {
		t.Fatalf("freshness age %q is not a duration: %v", match[1], err)
	}
	if age < 2*time.Minute+50*time.Second || age > 3*time.Minute+10*time.Second {
		t.Errorf("freshness age = %s, want about 3m", age)
	}
}

// The schedule view renders the last success that came with the job listing, so
// asking for schedules must not list runs once per job.
func TestScheduleRendersTheDaemonsLastSuccess(t *testing.T) {
	runListings := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/runs") {
			runListings++
			http.Error(w, "the schedule view must not list runs", http.StatusInternalServerError)
			return
		}
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()

	succeeded := time.Now().UTC().Add(-90 * time.Minute)
	list := []api.JobView{
		{
			ID: "id-1", Name: "counter", Valid: true,
			Triggers:      api.TriggerView{Cron: "*/5 * * * *"},
			LastSuccessAt: &succeeded,
		},
		{
			ID: "id-2", Name: "silent", Valid: true,
			Triggers: api.TriggerView{Cron: "0 * * * *"},
		},
	}

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.printSchedule(context.Background(), globals{api: srv.URL}, list, false); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	text := out.String()
	for _, want := range []string{"LAST SUCCESS", "counter", "1h30m0s ago)", "never succeeded"} {
		if !strings.Contains(text, want) {
			t.Errorf("schedule output is missing %q:\n%s", want, text)
		}
	}
	if runListings != 0 {
		t.Errorf("the schedule view listed runs %d times, want none", runListings)
	}
}

// A daemon that reports none of the queue signals must still print a status
// line rather than a blank one: "nothing waiting" and "none" are answers.
func TestStatusSaysNothingWaiting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","version":"v0.2.0","uptime_seconds":90,
			"jobs":{"total":0,"valid":0,"invalid":0},"queue_depth":0,"runs":{},
			"queue":{"retrying":0}}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.cmdStatus(context.Background(), globals{api: srv.URL}); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}
	text := out.String()
	for _, want := range []string{"queue age:     nothing waiting", "retries:       none"} {
		if !strings.Contains(text, want) {
			t.Errorf("status output is missing %q:\n%s", want, text)
		}
	}
}
