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

func TestShortID(t *testing.T) {
	tests := map[string]string{
		"4f1c2a7e-2b1d-4f6a-9c3e-8a5b0d7e1f22": "4f1c2a7e",
		"noseparator":                          "noseparator",
		"":                                     "",
	}
	for in, want := range tests {
		if got := shortID(in); got != want {
			t.Errorf("shortID(%q) = %q, want %q", in, got, want)
		}
	}
}

// `otter run` has to be usable in a shell conditional, so the exit code
// follows the run's outcome rather than the fact that it was queued.
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

// A retried run: the submitted attempt failed, a later attempt succeeded, and
// the summary must report the chain's outcome and the newest attempt's timing.
func TestPrintRunOutcomeFollowsTheRetryChain(t *testing.T) {
	view := &api.RunView{
		Run: &runs.Run{
			ID:       "first-attempt-0000",
			Status:   runs.StatusFailed,
			Attempt:  1,
			ExitCode: intPtr(1),
		},
		LatestStatus: runs.StatusSucceeded,
		Attempts: []*runs.Run{
			{ID: "first-attempt-0000", Status: runs.StatusFailed, Attempt: 1},
			{ID: "second-attempt-111", Status: runs.StatusSucceeded, Attempt: 2},
		},
	}

	var out bytes.Buffer
	app := New("test", &out, &out)
	app.printRunOutcome(&out, view)
	got := out.String()

	for _, want := range []string{"succeeded", "attempt 2 of 2"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("outcome does not mention %q:\n%s", want, got)
		}
	}
	if bytes.Contains([]byte(got), []byte("exit code  1")) {
		t.Errorf("outcome reported the failed attempt's exit code:\n%s", got)
	}
}

func TestPrintRunOutcomeReportsFailure(t *testing.T) {
	view := &api.RunView{
		Run:          &runs.Run{ID: "abc", Status: runs.StatusFailed, Attempt: 1, ExitCode: intPtr(1)},
		LatestStatus: runs.StatusFailed,
		Attempts:     []*runs.Run{{ID: "abc", Status: runs.StatusFailed, Attempt: 1, ExitCode: intPtr(1)}},
	}
	var out bytes.Buffer
	app := New("test", &out, &out)
	app.printRunOutcome(&out, view)
	got := out.String()

	if !bytes.Contains([]byte(got), []byte("failed")) || !bytes.Contains([]byte(got), []byte("exit code  1")) {
		t.Errorf("failure outcome is incomplete:\n%s", got)
	}
	if runExitCode(view) == 0 {
		t.Error("a failed run reported success")
	}
}

func intPtr(n int) *int { return &n }

// waitForRun must poll until the whole retry chain settles, not stop at the
// submitted attempt: a failed attempt is retried as a new run, so the run that
// was queued goes terminal while the work is still going.
func TestWaitForRunFollowsRetriesToTheEnd(t *testing.T) {
	// A daemon that reports the submitted attempt failed, then a retry
	// running, then that retry succeeded.
	// The failure becomes visible before the retry is: for a while the chain
	// looks finished, with only the root attempt present.
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
		fmt.Fprintf(w, `{"id":"root-run","integration_id":"x","status":"failed","latest_status":%q,"attempts":%s}`,
			step.status, step.attempts)
	}))
	defer server.Close()

	app := New("test", io.Discard, io.Discard)
	client := api.NewClient(server.URL, "")
	view, err := app.waitForRun(context.Background(), client, "root-run", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForRun: %v", err)
	}
	if view.LatestStatus != runs.StatusSucceeded {
		t.Errorf("settled on %q, want succeeded", view.LatestStatus)
	}
	if calls < 3 {
		t.Errorf("returned after %d poll(s); it must keep going while an attempt is retrying", calls)
	}
	if got := runExitCode(view); got != 0 {
		t.Errorf("exit %d for a chain that ended succeeded, want 0", got)
	}
}

// A run that never settles must not hang the command forever.
func TestWaitForRunTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"root-run","integration_id":"x","status":"running","latest_status":"running","attempts":[]}`)
	}))
	defer server.Close()

	app := New("test", io.Discard, io.Discard)
	client := api.NewClient(server.URL, "")
	_, err := app.waitForRun(context.Background(), client, "root-run", 300*time.Millisecond)
	if err == nil {
		t.Fatal("waitForRun returned without an error for a run that never finished")
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("timeout error does not explain itself: %v", err)
	}
}

// Both spellings of a long flag must work: a developer typing --no-wait
// should not get "flag provided but not defined" while --no-wait=true works.
func TestNormalizeLongFlags(t *testing.T) {
	got := normalizeLongFlags([]string{"counter", "--no-wait", "--body={\"a\":1}", "--", "--kept"})
	want := []string{"counter", "-no-wait", "--body={\"a\":1}", "--", "--kept"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Flags last is how people write commands; the standard flag package cannot
// parse that, so run reorders before parsing.
func TestFlagsFirst(t *testing.T) {
	takesValue := func(arg string) (bool, bool) {
		name := strings.TrimLeft(arg, "-")
		if i := strings.Index(name, "="); i >= 0 {
			name = name[:i]
		}
		return name == "body" || name == "timeout", true
	}
	tests := []struct {
		in   []string
		want []string
	}{
		{[]string{"counter", "--no-wait"}, []string{"-no-wait", "counter"}},
		{[]string{"counter", "--body", "{}"}, []string{"-body", "{}", "counter"}},
		{[]string{"--no-wait", "counter"}, []string{"-no-wait", "counter"}},
		{[]string{"counter"}, []string{"counter"}},
		// After a bare --, nothing is a flag.
		{[]string{"counter", "--", "--no-wait"}, []string{"counter", "--", "--no-wait"}},
	}
	for _, tc := range tests {
		// The pipeline cmdRun uses: normalize the dashes, then reorder.
		got := flagsFirst(normalizeLongFlags(tc.in), takesValue)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("flagsFirst(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
