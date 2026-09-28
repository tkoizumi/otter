package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/runs"
)

// The summary is one line with a colon, the shape `make sync-run` used, so a
// human reads it and a script can grep it.
func TestPrintRunOutcomeIsOneStatusLine(t *testing.T) {
	for _, status := range []runs.Status{runs.StatusSucceeded, runs.StatusFailed, runs.StatusTimedOut} {
		view := &api.RunView{Run: &runs.Run{ID: "x", Status: status}, LatestStatus: status}
		var out bytes.Buffer
		app := New("test", &out, &out)
		app.printRunOutcome(&out, view)
		if got, want := out.String(), "status: "+string(status)+"\n"; got != want {
			t.Errorf("outcome = %q, want %q", got, want)
		}
	}
}

// Output is printed as the daemon stores it: the message with any structured
// fields as a trailing JSON object, exactly what `otter logs` shows.
func TestRawLogLineMatchesTheStoredRecord(t *testing.T) {
	tests := []struct{ in, want string }{
		{`sync finished {"complete":true,"failed":0,"level":"info"}`, `sync finished {"complete":true,"failed":0,"level":"info"}`},
		{`run started (attempt 1 of 3, trigger manual)`, `run started (attempt 1 of 3, trigger manual)`},
		{`{"level":"info","message":"no text"}`, `{"level":"info","message":"no text"}`},
	}
	for _, tc := range tests {
		if got := rawLogLine(tc.in); got != tc.want {
			t.Errorf("rawLogLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The id must be the full one: the daemon does not resolve prefixes, so an
// abbreviated id would be unusable in the next command.
func TestRunPrintsTheWholeIDAndNothingElseUpFront(t *testing.T) {
	id := "986d91e8-dde4-45be-b298-c9332c220498"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"logs":[{"id":1,"run_id":%q,"stream":"otter","message":"sync finished {\"complete\":true,\"failed\":0,\"level\":\"info\"}"}]}`, id)
	}))
	defer server.Close()

	var out bytes.Buffer
	app := New("test", &out, &out)
	app.printRunOutput(context.Background(), api.NewClient(server.URL, ""), id)
	if got, want := out.String(), "sync finished {\"complete\":true,\"failed\":0,\"level\":\"info\"}\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// A log fetch that fails must not lose the run itself.
func TestPrintRunOutputReportsAFailedFetchWithoutLosingTheRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	app.printRunOutput(context.Background(), api.NewClient(server.URL, ""), "abc")
	if out.Len() != 0 {
		t.Errorf("stdout got %q, want nothing", out.String())
	}
	if !strings.Contains(errOut.String(), "otter logs abc --follow") {
		t.Errorf("stderr does not say how to read the run:\n%s", errOut.String())
	}
}

// `otter run` has to be usable in a shell conditional.
func TestRunExitCode(t *testing.T) {
	tests := []struct {
		status runs.Status
		want   int
	}{
		{runs.StatusSucceeded, 0},
		{runs.StatusFailed, 1},
		{runs.StatusTimedOut, 1},
		{runs.StatusCancelled, 1},
	}
	for _, tc := range tests {
		view := &api.RunView{Run: &runs.Run{Status: tc.status}, LatestStatus: tc.status}
		if got := runExitCode(view); got != tc.want {
			t.Errorf("status %s -> exit %d, want %d", tc.status, got, tc.want)
		}
	}
}

// waitForRun must poll until the whole retry chain settles, not stop at the
// submitted attempt: a failed attempt becomes visible before its retry is
// queued, so the chain can look finished while another attempt is coming.
func TestWaitForRunFollowsRetriesToTheEnd(t *testing.T) {
	timeline := []struct {
		status   string
		attempts string
	}{
		{"failed", `[{"id":"root-run","status":"failed","attempt":1}]`},
		{"failed", `[{"id":"root-run","status":"failed","attempt":1}]`},
		{"queued", `[{"id":"root-run","status":"failed","attempt":1},{"id":"retry-run","status":"queued","attempt":2}]`},
		{"succeeded", `[{"id":"root-run","status":"failed","attempt":1},{"id":"retry-run","status":"succeeded","attempt":2}]`},
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		step := timeline[len(timeline)-1]
		if calls < len(timeline) {
			step = timeline[calls]
		}
		calls++
		// max_attempts 3 with attempt 1 failed: the policy can still retry, so
		// the wait must keep watching rather than call the chain exhausted.
		fmt.Fprintf(w, `{"id":"root-run","integration_id":"x","status":"failed","latest_status":%q,"max_attempts":3,"attempts":%s}`,
			step.status, step.attempts)
	}))
	defer server.Close()

	app := New("test", io.Discard, io.Discard)
	view, err := app.waitForRun(context.Background(), api.NewClient(server.URL, ""), "root-run", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForRun: %v", err)
	}
	if view.LatestStatus != runs.StatusSucceeded {
		t.Errorf("settled on %q, want succeeded", view.LatestStatus)
	}
	if calls < 3 {
		t.Errorf("returned after %d poll(s); it must keep going while an attempt is retrying", calls)
	}
}

// A timeout is retried just like a failure, so the wait has to follow it into
// the next attempt instead of reporting the timeout while the retry is coming.
func TestWaitForRunFollowsATimeoutIntoItsRetry(t *testing.T) {
	timeline := []struct {
		status   string
		attempts string
	}{
		{"timed_out", `[{"id":"root-run","status":"timed_out","attempt":1}]`},
		{"running", `[{"id":"root-run","status":"timed_out","attempt":1},{"id":"retry-run","status":"running","attempt":2}]`},
		{"succeeded", `[{"id":"root-run","status":"timed_out","attempt":1},{"id":"retry-run","status":"succeeded","attempt":2}]`},
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		step := timeline[len(timeline)-1]
		if calls < len(timeline) {
			step = timeline[calls]
		}
		calls++
		fmt.Fprintf(w, `{"id":"root-run","integration_id":"x","status":"timed_out","latest_status":%q,"max_attempts":2,"attempts":%s}`,
			step.status, step.attempts)
	}))
	defer server.Close()

	app := New("test", io.Discard, io.Discard)
	view, err := app.waitForRun(context.Background(), api.NewClient(server.URL, ""), "root-run", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForRun: %v", err)
	}
	if view.LatestStatus != runs.StatusSucceeded {
		t.Errorf("settled on %q, want succeeded", view.LatestStatus)
	}
	if calls < 3 {
		t.Errorf("returned after %d poll(s); a timeout with attempts left is not the end of the chain", calls)
	}
}

// A failure or timeout the manifest cannot retry is the end of the chain on the
// first view. Without this, `otter run` spends retryGrace watching a chain that
// will never move -- which is every run of an integration with retries disabled.
func TestWaitForRunSettlesWhenRetriesAreExhausted(t *testing.T) {
	for _, status := range []runs.Status{runs.StatusFailed, runs.StatusTimedOut} {
		t.Run(string(status), func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				calls++
				fmt.Fprintf(w, `{"id":"root-run","integration_id":"x","status":%q,"attempt":1,"latest_status":%q,"max_attempts":1,"attempts":[{"id":"root-run","status":%q,"attempt":1}]}`,
					status, status, status)
			}))
			defer server.Close()

			app := New("test", io.Discard, io.Discard)
			start := time.Now()
			view, err := app.waitForRun(context.Background(), api.NewClient(server.URL, ""), "root-run", 30*time.Second)
			if err != nil {
				t.Fatalf("waitForRun: %v", err)
			}
			if view.LatestStatus != status {
				t.Errorf("settled on %q, want %s", view.LatestStatus, status)
			}
			if calls != 1 {
				t.Errorf("polled %d time(s); an exhausted chain must settle on the first view", calls)
			}
			if elapsed := time.Since(start); elapsed > retryGrace/2 {
				t.Errorf("took %s; an exhausted chain must not spend the %s grace", elapsed, retryGrace)
			}
		})
	}
}

// When the daemon cannot report the ceiling (an integration that has been
// removed, or an older daemon), the grace is still what bounds the wait: a
// failed chain is watched briefly in case a retry appears.
func TestWaitForRunFallsBackToTheGraceWithoutACeiling(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls++
		fmt.Fprint(w, `{"id":"root-run","integration_id":"x","status":"failed","attempt":2,"latest_status":"failed","attempts":[{"id":"root-run","status":"failed","attempt":2}]}`)
	}))
	defer server.Close()

	app := New("test", io.Discard, io.Discard)
	start := time.Now()
	view, err := app.waitForRun(context.Background(), api.NewClient(server.URL, ""), "root-run", 30*time.Second)
	if err != nil {
		t.Fatalf("waitForRun: %v", err)
	}
	if view.LatestStatus != runs.StatusFailed {
		t.Errorf("settled on %q, want failed", view.LatestStatus)
	}
	if elapsed := time.Since(start); elapsed < retryGrace {
		t.Errorf("returned after %s; an unknown ceiling must be watched for the %s grace", elapsed, retryGrace)
	}
	if calls < 2 {
		t.Errorf("polled %d time(s); the grace must be spent polling, not sleeping", calls)
	}
}

func TestWaitForRunTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"root-run","integration_id":"x","status":"running","latest_status":"running","attempts":[]}`)
	}))
	defer server.Close()

	app := New("test", io.Discard, io.Discard)
	_, err := app.waitForRun(context.Background(), api.NewClient(server.URL, ""), "root-run", 300*time.Millisecond)
	if err == nil {
		t.Fatal("waitForRun returned without an error for a run that never finished")
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("timeout error does not explain itself: %v", err)
	}
}

// Both spellings of a long flag must work, and a flag written after the
// integration name must not become a second positional.
func TestRunArgumentParsing(t *testing.T) {
	takesValue := func(arg string) bool {
		name := strings.TrimLeft(arg, "-")
		if i := strings.Index(name, "="); i >= 0 {
			name = name[:i]
		}
		return name == "body" || name == "timeout"
	}
	tests := []struct {
		in   []string
		want []string
	}{
		{[]string{"counter", "--no-wait"}, []string{"-no-wait", "counter"}},
		{[]string{"counter", "--body", "{}"}, []string{"-body", "{}", "counter"}},
		{[]string{"--no-wait", "counter"}, []string{"-no-wait", "counter"}},
		{[]string{"counter"}, []string{"counter"}},
		{[]string{"counter", "--", "--no-wait"}, []string{"counter", "--", "--no-wait"}},
	}
	for _, tc := range tests {
		got := flagsFirst(normalizeLongFlags(tc.in), takesValue)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("pipeline(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The waiting path must name the run it queued: the id is the handle for
// everything that follows, and it has to be on stdout even if the wait is
// interrupted. This regressed twice, so it is pinned end to end through
// cmdRun rather than by inspecting the pieces.
func TestCmdRunPrintsTheIDItQueued(t *testing.T) {
	id := "986d91e8-dde4-45be-b298-c9332c220498"
	var logPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		logPath = r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/runs"):
			fmt.Fprintf(w, `{"run_id":%q,"status":"queued"}`, id)
		case strings.HasSuffix(r.URL.Path, "/logs"):
			fmt.Fprint(w, `{"logs":[{"id":1,"run_id":"x","stream":"otter","message":"sync finished {\"level\":\"info\"}"}]}`)
		default:
			fmt.Fprintf(w, `{"id":%q,"status":"succeeded","latest_status":"succeeded","attempts":[]}`, id)
		}
	}))
	defer server.Close()

	// Force the waiting path, which is otherwise gated on a terminal.
	original := interactiveOutput
	interactiveOutput = func(io.Writer) bool { return true }
	defer func() { interactiveOutput = original }()

	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	code := app.cmdRun(context.Background(), globals{api: server.URL}, []string{"demo"})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}

	got := out.String()
	// Exactly once: the id goes out before the wait and must not be repeated
	// after it.
	if n := strings.Count(got, "run: "+id); n != 1 {
		t.Errorf("the queued run id appears %d times, want exactly 1:\n%s", n, got)
	}
	if !strings.Contains(got, "status: succeeded") {
		t.Errorf("no status line:\n%s", got)
	}
	if !strings.Contains(got, `sync finished {"level":"info"}`) {
		t.Errorf("the run's output is missing:\n%s", got)
	}
	// Order matters: the id comes out before anything is waited on.
	if strings.Index(got, "run: "+id) > strings.Index(got, "status:") {
		t.Errorf("the id was printed after the status:\n%s", got)
	}
	_ = logPath
}

// A failed run whose manifest cannot retry has to come back as soon as the
// daemon reports it: a 321ms failure that leaves the terminal silent for the
// grace is the bug this pins end to end, through cmdRun.
func TestCmdRunDoesNotLingerOnAnExhaustedFailure(t *testing.T) {
	id := "0d1f2e3a-4b5c-4d6e-8f90-a1b2c3d4e5f6"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/runs"):
			fmt.Fprintf(w, `{"run_id":%q,"status":"queued"}`, id)
		case strings.HasSuffix(r.URL.Path, "/logs"):
			fmt.Fprint(w, `{"logs":[{"id":1,"run_id":"x","stream":"otter","message":"run failed (attempt 1 of 1, 321ms), exit code 1"}]}`)
		default:
			// A single allowed attempt, already failed: the chain is over.
			fmt.Fprintf(w, `{"id":%q,"status":"failed","attempt":1,"latest_status":"failed","max_attempts":1,"attempts":[{"id":%q,"status":"failed","attempt":1}]}`, id, id)
		}
	}))
	defer server.Close()

	original := interactiveOutput
	interactiveOutput = func(io.Writer) bool { return true }
	defer func() { interactiveOutput = original }()

	var out, errOut bytes.Buffer
	app := New("test", &out, &errOut)
	start := time.Now()
	code := app.cmdRun(context.Background(), globals{api: server.URL}, []string{"demo"})
	elapsed := time.Since(start)

	if code != 1 {
		t.Fatalf("exit %d, want 1 for a failed run; stderr: %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "status: failed") {
		t.Errorf("no failure status line:\n%s", got)
	}
	if !strings.Contains(got, "run failed (attempt 1 of 1") {
		t.Errorf("the run's output is missing:\n%s", got)
	}
	if elapsed > retryGrace/2 {
		t.Errorf("took %s; an exhausted failure must not spend the %s grace", elapsed, retryGrace)
	}
}
