package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
)

// pauseRecorder is a daemon stand-in that records what the CLI sent and answers
// with a canned pause view.
type pauseRecorder struct {
	path   string
	body   string
	status int
	view   api.PauseView
}

func (p *pauseRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		p.body = string(raw)

		status := p.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"code":"conflict","message":"nope"}}`)
			return
		}
		if err := json.NewEncoder(w).Encode(p.view); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
}

// withWorkingDir points path-based reference resolution at dir for one test.
func withWorkingDir(t *testing.T, dir string) {
	t.Helper()
	original := workingDirForTest
	workingDirForTest = func() (string, error) { return dir, nil }
	t.Cleanup(func() { workingDirForTest = original })
}

// writeManifest lays a minimal manifest into a fresh directory and returns it.
func writeManifest(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "version: 1\nname: " + name + "\nentrypoint: main.py\n"
	if err := os.WriteFile(filepath.Join(dir, config.ManifestFileName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

// The point of the whole feature: standing in the job directory,
// `otter pause` needs no argument and no id. It resolves through the manifest,
// exactly like `otter run`.
func TestPauseWithNoArgumentUsesTheWorkingDirectory(t *testing.T) {
	rec := &pauseRecorder{view: api.PauseView{
		JobID: "id-1", Name: "counter", Paused: true, Changed: true,
	}}
	srv := rec.server(t)
	defer srv.Close()

	withWorkingDir(t, writeManifest(t, "counter"))

	var out bytes.Buffer
	app := New("test", &out, &out)
	code := app.cmdPauseResume(context.Background(), globals{api: srv.URL}, nil, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}

	if rec.path != "/v1/jobs/counter/pause" {
		t.Errorf("request path = %q, want /v1/jobs/counter/pause", rec.path)
	}
	if rec.body != "" {
		t.Errorf("pause sent a request body %q, want none", rec.body)
	}
	if text := out.String(); !strings.Contains(text, "paused: counter") {
		t.Errorf("output does not name the pause:\n%s", text)
	}
	if text := out.String(); !strings.Contains(text, "otter run still runs it on demand") {
		t.Errorf("output does not say manual runs still work:\n%s", text)
	}
}

func TestResumeWithNoArgumentUsesTheWorkingDirectory(t *testing.T) {
	rec := &pauseRecorder{view: api.PauseView{
		JobID: "id-1", Name: "counter", Paused: false, Changed: true,
	}}
	srv := rec.server(t)
	defer srv.Close()

	withWorkingDir(t, writeManifest(t, "counter"))

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.cmdPauseResume(context.Background(), globals{api: srv.URL}, nil, false); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if rec.path != "/v1/jobs/counter/resume" {
		t.Errorf("request path = %q, want /v1/jobs/counter/resume", rec.path)
	}
	if text := out.String(); !strings.Contains(text, "resumed: counter") {
		t.Errorf("output = %q, want the resume named", text)
	}
}

// A repeat must be visible as a no-op: a deploy script calls pause every time
// and needs to know whether this call was the one that changed anything.
func TestPauseReportsThatNothingChanged(t *testing.T) {
	rec := &pauseRecorder{view: api.PauseView{
		JobID: "id-1", Name: "counter", Paused: true, Changed: false,
	}}
	srv := rec.server(t)
	defer srv.Close()

	withWorkingDir(t, writeManifest(t, "counter"))

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.cmdPauseResume(context.Background(), globals{api: srv.URL}, nil, true); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if text := out.String(); !strings.Contains(text, "already paused: counter") {
		t.Errorf("output = %q, want an explicit no-op", text)
	}
}

// A name, a path and an identity all address the same job, and the path
// form is what `otter pause .` in a job directory sends.
func TestPauseAcceptsAPathArgument(t *testing.T) {
	rec := &pauseRecorder{view: api.PauseView{
		JobID: "id-1", Name: "counter", Paused: true, Changed: true,
	}}
	srv := rec.server(t)
	defer srv.Close()

	dir := writeManifest(t, "counter")
	withWorkingDir(t, t.TempDir())

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.cmdPauseResume(context.Background(), globals{api: srv.URL}, []string{dir}, true); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}
	if rec.path != "/v1/jobs/counter/pause" {
		t.Errorf("request path = %q, want the manifest's name", rec.path)
	}
}

func TestPauseRefusesMoreThanOneJob(t *testing.T) {
	var out bytes.Buffer
	app := New("test", &out, &out)
	code := app.cmdPauseResume(context.Background(), globals{api: "http://127.0.0.1:1"},
		[]string{"one", "two"}, true)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

// A conflict -- a retired identity, say -- must be the daemon's message, not a
// generic failure.
func TestPauseSurfacesTheDaemonsRefusal(t *testing.T) {
	rec := &pauseRecorder{status: http.StatusConflict}
	srv := rec.server(t)
	defer srv.Close()

	withWorkingDir(t, writeManifest(t, "counter"))

	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	code := app.cmdPauseResume(context.Background(), globals{api: srv.URL}, nil, true)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if errOut.Len() == 0 {
		t.Error("a refused pause should explain itself on stderr")
	}
}

// `otter cancel` is the missing half of pause: pause stops new admission and
// leaves work in flight, and cancel is how that work is ended.
func TestCancelStopsTheNamedRun(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"run_id":"run-1","status":"cancelled"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.cmdCancel(context.Background(), globals{api: srv.URL}, []string{"run-1"}); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}
	if path != "/v1/runs/run-1/cancel" {
		t.Errorf("request path = %q, want /v1/runs/run-1/cancel", path)
	}
	if text := out.String(); !strings.Contains(text, "cancelled    run-1") {
		t.Errorf("output = %q, want the cancelled run named", text)
	}
}

func TestCancelRequiresARunID(t *testing.T) {
	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.cmdCancel(context.Background(), globals{api: "http://127.0.0.1:1"}, nil); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

// A paused job has no next run, and the schedule view has to say so.
// A blank column is what the docs warn reads as "silently not firing".
func TestScheduleViewNamesAPausedJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/runs") {
			fmt.Fprint(w, `{"runs":[]}`)
			return
		}
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()

	list := []api.JobView{{
		ID: "id-1", Name: "counter", Valid: true,
		Triggers: api.TriggerView{Cron: "*/5 * * * *", Paused: true},
	}}

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.printSchedule(context.Background(), globals{api: srv.URL}, list, false); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if text := out.String(); !strings.Contains(text, "paused") {
		t.Errorf("schedule view does not name the pause:\n%s", text)
	}
}

func TestPausedSummarySaysSinceWhen(t *testing.T) {
	at := mustParseTime(t, "2026-09-13T17:05:00Z")
	tests := []struct {
		name string
		in   api.TriggerView
		want string
	}{
		{"no instant", api.TriggerView{Paused: true}, "yes"},
		{"instant", api.TriggerView{Paused: true, PausedAt: &at},
			"since " + at.Local().Format("2006-01-02 15:04:05")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pausedSummary(tc.in); got != tc.want {
				t.Errorf("pausedSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

// mustParseTime parses an RFC 3339 instant or fails the test.
func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return at
}

// The new verbs act on a running runtime, so they must refuse to run without a
// workspace or an explicit --api like every other daemon-backed command.
func TestPauseResumeAndCancelNeedADaemon(t *testing.T) {
	for _, command := range []string{"pause", "resume", "cancel"} {
		if !needsDaemon(command) {
			t.Errorf("needsDaemon(%q) = false, want true", command)
		}
	}
}

// `otter inspect` follows the same convention as `otter run` and
// `otter release`: standing in the job directory, the argument is
// optional. It also has to explain a pause, since the missing next run is the
// thing that would otherwise look like a fault.
func TestInspectWithNoArgumentShowsThePause(t *testing.T) {
	var path string
	at := time.Date(2026, 9, 13, 17, 5, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/runs") {
			fmt.Fprint(w, `{"runs":[]}`)
			return
		}
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(api.JobView{
			ID:               "counter",
			Name:             "counter",
			Path:             "/tmp/counter",
			Entrypoint:       "main.py",
			PythonExecutable: "python3",
			TimeoutSeconds:   300,
			Concurrency:      1,
			Valid:            true,
			Capture:          "metadata",
			Retry:            api.RetryView{Backoff: "none", InitialDelay: "2s", MaxDelay: "1m0s"},
			Triggers:         api.TriggerView{Cron: "*/5 * * * *", Paused: true, PausedAt: &at},
		})
	}))
	defer srv.Close()

	withWorkingDir(t, writeManifest(t, "counter"))

	var out bytes.Buffer
	app := New("test", &out, &out)
	if code := app.cmdInspect(context.Background(), globals{api: srv.URL}, nil); code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out.String())
	}
	if path != "/v1/jobs/counter" {
		t.Errorf("request path = %q, want /v1/jobs/counter", path)
	}
	text := out.String()
	if !strings.Contains(text, "paused:") || !strings.Contains(text, "since ") {
		t.Errorf("inspect output does not explain the pause:\n%s", text)
	}
	if strings.Contains(text, "next run:") {
		t.Errorf("a paused job should not report a next run:\n%s", text)
	}
}
