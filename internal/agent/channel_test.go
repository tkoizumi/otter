package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeChannel records what the agent reported and hands back queued work.
type fakeChannel struct {
	work     *ControlWork
	commands []CommandResult
	reads    []ReadResult
}

func (f *fakeChannel) Control(context.Context) (*ControlWork, error) { return f.work, nil }
func (f *fakeChannel) CommandResult(_ context.Context, r CommandResult) error {
	f.commands = append(f.commands, r)
	return nil
}
func (f *fakeChannel) ReadResult(_ context.Context, r ReadResult) error {
	f.reads = append(f.reads, r)
	return nil
}

// fakeCommandRuntime records the calls the control loop made.
type fakeCommandRuntime struct {
	runID      string
	runErr     error
	submitted  []struct{ job, key string }
	cancelled  []string
	paused     []string
	resumed    []string
	deleted    []string
	deleteErr  error
	deleteNote string
	readBody   json.RawMessage
	readErr    error
}

func (f *fakeCommandRuntime) SubmitRun(_ context.Context, job, key string) (string, error) {
	f.submitted = append(f.submitted, struct{ job, key string }{job, key})
	return f.runID, f.runErr
}
func (f *fakeCommandRuntime) CancelRun(_ context.Context, runID string) error {
	f.cancelled = append(f.cancelled, runID)
	return nil
}
func (f *fakeCommandRuntime) PauseJob(_ context.Context, job string) error {
	f.paused = append(f.paused, job)
	return nil
}
func (f *fakeCommandRuntime) ResumeJob(_ context.Context, job string) error {
	f.resumed = append(f.resumed, job)
	return nil
}
func (f *fakeCommandRuntime) DeleteJob(_ context.Context, job string) (string, error) {
	f.deleted = append(f.deleted, job)
	return f.deleteNote, f.deleteErr
}
func (f *fakeCommandRuntime) Read(context.Context, string, string, string, int, string) (json.RawMessage, error) {
	return f.readBody, f.readErr
}

// A run command carries its caller-derived idempotency key to the runtime and is
// acknowledged with the run id -- accepted, not succeeded.
func TestControlLoopExecutesARunCommandWithItsKey(t *testing.T) {
	channel := &fakeChannel{work: &ControlWork{Commands: []Command{
		{ID: "cmd-1", Action: "run", Job: "sync", IdempotencyKey: "key-1"},
	}}}
	runtime := &fakeCommandRuntime{runID: "run-9"}
	loop := &ControlLoop{Control: channel, Runtime: runtime, RuntimeID: "rt-a"}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(runtime.submitted) != 1 || runtime.submitted[0].job != "sync" || runtime.submitted[0].key != "key-1" {
		t.Fatalf("SubmitRun calls = %+v, want one sync/key-1", runtime.submitted)
	}
	if len(channel.commands) != 1 {
		t.Fatalf("command results = %+v, want one", channel.commands)
	}
	got := channel.commands[0]
	if got.Status != CommandAccepted || got.RunID != "run-9" || got.CommandID != "cmd-1" || got.RuntimeID != "rt-a" {
		t.Fatalf("command result = %+v, want accepted run-9", got)
	}
}

// An action outside the allowlist is REJECTED and never reaches the runtime.
func TestControlLoopRejectsAnUnknownAction(t *testing.T) {
	channel := &fakeChannel{work: &ControlWork{Commands: []Command{
		{ID: "cmd-2", Action: "escalate", Job: "sync"},
	}}}
	runtime := &fakeCommandRuntime{}
	loop := &ControlLoop{Control: channel, Runtime: runtime, RuntimeID: "rt-a"}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(runtime.submitted) != 0 || len(runtime.cancelled) != 0 || len(runtime.paused) != 0 {
		t.Fatalf("an unsupported action reached the runtime: %+v", runtime)
	}
	if len(channel.commands) != 1 || channel.commands[0].Status != CommandRejected {
		t.Fatalf("command results = %+v, want a rejection", channel.commands)
	}
}

// The delete action reaches the runtime and its outcome is reported: accepted,
// with the runtime's own account of what was removed carried as Note. It is the
// first irreversible action on this channel, so a bare "accepted" would not be
// enough -- the control plane has to be able to record the scope.
func TestControlLoopExecutesADeleteCommandAndReportsItsNote(t *testing.T) {
	channel := &fakeChannel{work: &ControlWork{Commands: []Command{
		{ID: "cmd-del", Action: "delete", Job: "sync"},
	}}}
	runtime := &fakeCommandRuntime{deleteNote: "released-only job: every release was removed"}
	loop := &ControlLoop{Control: channel, Runtime: runtime, RuntimeID: "rt-a"}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(runtime.deleted) != 1 || runtime.deleted[0] != "sync" {
		t.Fatalf("DeleteJob calls = %+v, want one sync", runtime.deleted)
	}
	if len(channel.commands) != 1 {
		t.Fatalf("command results = %+v, want one", channel.commands)
	}
	got := channel.commands[0]
	if got.Status != CommandAccepted || got.CommandID != "cmd-del" || got.RuntimeID != "rt-a" {
		t.Fatalf("command result = %+v, want accepted cmd-del", got)
	}
	if got.Note != runtime.deleteNote {
		t.Errorf("note = %q, want the runtime's own %q", got.Note, runtime.deleteNote)
	}
	if got.Reason != "" {
		t.Errorf("an accepted command carries no failure reason, got %q", got.Reason)
	}
}

// A delete the runtime REFUSES -- an in-flight run, for instance -- is reported
// as failed with the runtime's reason. Acceptance is never inferred from the
// fact that the action is on the allowlist.
func TestControlLoopReportsARefusedDeleteWithTheRuntimeReason(t *testing.T) {
	channel := &fakeChannel{work: &ControlWork{Commands: []Command{
		{ID: "cmd-del", Action: "delete", Job: "sync"},
	}}}
	runtime := &fakeCommandRuntime{
		deleteErr: context.DeadlineExceeded,
	}
	loop := &ControlLoop{Control: channel, Runtime: runtime, RuntimeID: "rt-a"}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(channel.commands) != 1 {
		t.Fatalf("command results = %+v, want one", channel.commands)
	}
	got := channel.commands[0]
	if got.Status != CommandFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Reason == "" || got.Note != "" {
		t.Errorf("a refused delete must carry a reason and no note: %+v", got)
	}
}

// A read returns the runtime's own body, unmodified, with status ok.
func TestControlLoopReportsAReadBody(t *testing.T) {
	body := json.RawMessage(`{"runs":[{"id":"run-1"}]}`)
	channel := &fakeChannel{work: &ControlWork{Reads: []Read{
		{ID: "read-1", Op: "runs.list", Limit: 10},
	}}}
	runtime := &fakeCommandRuntime{readBody: body}
	loop := &ControlLoop{Control: channel, Runtime: runtime, RuntimeID: "rt-a"}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(channel.reads) != 1 {
		t.Fatalf("read results = %+v, want one", channel.reads)
	}
	got := channel.reads[0]
	if got.Status != ReadOK || got.ReadID != "read-1" || string(got.Body) != string(body) {
		t.Fatalf("read result = %+v, want ok with the runtime body", got)
	}
}

// A read that fails is reported as an error, not as an empty ok.
func TestControlLoopReportsAReadError(t *testing.T) {
	channel := &fakeChannel{work: &ControlWork{Reads: []Read{{ID: "read-2", Op: "run.logs", RunID: "run-1"}}}}
	runtime := &fakeCommandRuntime{readErr: context.DeadlineExceeded}
	loop := &ControlLoop{Control: channel, Runtime: runtime, RuntimeID: "rt-a"}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(channel.reads) != 1 || channel.reads[0].Status != ReadError {
		t.Fatalf("read results = %+v, want an error", channel.reads)
	}
}

// The read allowlist is enforced on the agent as well as on Cloud.
func TestReadPathAllowlist(t *testing.T) {
	if _, err := readPath("jobs.list", "", "", 5, ""); err != nil {
		t.Errorf("jobs.list should be allowed: %v", err)
	}
	if _, err := readPath("run.logs", "", "run-1", 0, ""); err != nil {
		t.Errorf("run.logs should be allowed: %v", err)
	}
	if _, err := readPath("run.logs", "", "", 0, ""); err == nil {
		t.Error("run.logs without a run id should be refused")
	}
	if _, err := readPath("capture.payload", "", "run-1", 0, ""); err == nil {
		t.Error("an op outside the allowlist should be refused")
	}
}

// The real runtime client sends the key as the Idempotency-Key header and reads
// the run id back.
func TestRuntimeHTTPSubmitRunCarriesTheIdempotencyKey(t *testing.T) {
	var sawPath, sawKey, sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawKey, sawAuth = r.URL.Path, r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization")
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"run_id": "run-42"})
	}))
	defer srv.Close()

	rt := &RuntimeHTTP{BaseURL: srv.URL, Token: "runtime-token"}
	id, err := rt.SubmitRun(context.Background(), "sync", "key-42")
	if err != nil {
		t.Fatalf("SubmitRun: %v", err)
	}
	if id != "run-42" {
		t.Fatalf("run id = %q, want run-42", id)
	}
	if sawPath != "/v1/jobs/sync/runs" {
		t.Errorf("path = %q, want /v1/jobs/sync/runs", sawPath)
	}
	if sawKey != "key-42" {
		t.Errorf("Idempotency-Key = %q, want key-42", sawKey)
	}
	if sawAuth != "Bearer runtime-token" {
		t.Errorf("Authorization = %q", sawAuth)
	}
}

// A read returns the runtime's JSON, and the client refuses an op it cannot map.
func TestRuntimeHTTPReadReturnsRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run-1/logs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"lines":["hello"]}`))
	}))
	defer srv.Close()

	rt := &RuntimeHTTP{BaseURL: srv.URL, Token: "t"}
	raw, err := rt.Read(context.Background(), "run.logs", "", "run-1", 0, "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(raw) != `{"lines":["hello"]}` {
		t.Errorf("body = %s", raw)
	}
	if _, err := rt.Read(context.Background(), "tokens.list", "", "", 0, ""); err == nil {
		t.Error("a read outside the allowlist must be refused before any request")
	}
}

// The real runtime client deletes through the operator's own route -- DELETE
// /v1/jobs/{id} -- and returns the runtime's note as the command's outcome.
func TestRuntimeHTTPDeleteJobUsesTheOperatorRouteAndReturnsTheNote(t *testing.T) {
	var sawMethod, sawPath, sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod, sawPath, sawAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":true,"id":"job-7","name":"sync","note":"every release was removed"}`))
	}))
	defer srv.Close()

	rt := &RuntimeHTTP{BaseURL: srv.URL, Token: "runtime-token"}
	note, err := rt.DeleteJob(context.Background(), "job-7")
	if err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if sawMethod != http.MethodDelete || sawPath != "/v1/jobs/job-7" {
		t.Errorf("request = %s %s, want DELETE /v1/jobs/job-7", sawMethod, sawPath)
	}
	if sawAuth != "Bearer runtime-token" {
		t.Errorf("Authorization = %q", sawAuth)
	}
	if note != "every release was removed" {
		t.Errorf("note = %q, want the runtime's note", note)
	}
}

// A 2xx that does not confirm a delete is not a successful delete. Reporting it
// as accepted would let a control plane close an operation that never happened.
func TestRuntimeHTTPDeleteJobRefusesAnUnconfirmedSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":false,"id":"job-7"}`))
	}))
	defer srv.Close()

	rt := &RuntimeHTTP{BaseURL: srv.URL, Token: "t"}
	if _, err := rt.DeleteJob(context.Background(), "job-7"); err == nil {
		t.Fatal("DeleteJob accepted a response that did not confirm the delete")
	}
	// A delete with no job is refused before any request is made.
	if _, err := rt.DeleteJob(context.Background(), "  "); err == nil {
		t.Error("DeleteJob must refuse an empty job reference")
	}
}

// scriptedChannel answers a fixed script and records WHEN each call happened,
// so a test can assert the SPACING between calls -- which is the whole question
// of whether the loop sleeps after a response that carried work.
type scriptedChannel struct {
	mu    sync.Mutex
	calls int
	at    []time.Time
	// work returns the answer for call n (1-based). Never nil.
	work func(n int) *ControlWork
	// after runs after the answer for call n has been recorded, if set.
	after func(n int)
}

func (c *scriptedChannel) Control(context.Context) (*ControlWork, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.at = append(c.at, time.Now())
	c.mu.Unlock()
	if c.after != nil {
		c.after(n)
	}
	if c.work == nil {
		return &ControlWork{}, nil
	}
	return c.work(n), nil
}

func (c *scriptedChannel) CommandResult(context.Context, CommandResult) error { return nil }
func (c *scriptedChannel) ReadResult(context.Context, ReadResult) error       { return nil }

func (c *scriptedChannel) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// gaps is the time between consecutive calls, in order.
func (c *scriptedChannel) gaps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, 0, len(c.at))
	for i := 1; i < len(c.at); i++ {
		out = append(out, c.at[i].Sub(c.at[i-1]))
	}
	return out
}

// A response that CARRIED work must be followed by another request immediately.
// A dashboard page queues several reads as a burst and one poll leases up to
// MAX_PER_POLL of them; sleeping the fallback interval between them is what made
// a page cost several intervals.
func TestControlLoopReasksImmediatelyAfterWork(t *testing.T) {
	channel := &scriptedChannel{
		work: func(n int) *ControlWork {
			if n == 1 {
				return &ControlWork{Commands: []Command{{ID: "c1", Action: "run", Job: "sync"}}}
			}
			return &ControlWork{}
		},
	}
	loop := &ControlLoop{
		Control: channel,
		Runtime: &fakeCommandRuntime{runID: "run-1"},
		// An hour, so if the loop sleeps at all after work it cannot reach a
		// second call before the test's patience runs out.
		Interval: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The second call is empty, and cancel after it so Run returns.
	channel.after = func(n int) {
		if n == 2 {
			cancel()
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = loop.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not re-request within 5s: it slept after a response that carried work")
	}

	gaps := channel.gaps()
	if len(gaps) < 1 {
		t.Fatalf("control calls = %d, want at least two", channel.callCount())
	}
	if gaps[0] > time.Second {
		t.Fatalf("gap after a work-carrying response = %s, want an immediate re-request", gaps[0])
	}
}

// The 2-second interval is the FALLBACK for a control plane that answers
// immediately (a 204, or an older deployment with no hold). It must still be
// honoured, or such a plane would be polled in a hot loop.
func TestControlLoopKeepsTheFallbackIntervalWhenTheAnswerIsEmpty(t *testing.T) {
	channel := &scriptedChannel{
		work: func(int) *ControlWork { return &ControlWork{} },
	}
	fallback := 60 * time.Millisecond
	loop := &ControlLoop{Control: channel, Runtime: &fakeCommandRuntime{}, Interval: fallback}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	channel.after = func(n int) {
		if n == 3 {
			cancel()
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = loop.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not run")
	}

	gaps := channel.gaps()
	if len(gaps) < 2 {
		t.Fatalf("control calls = %d, want at least three", channel.callCount())
	}
	for i, gap := range gaps {
		// Timers never fire early, so a correct sleep gives at least `fallback`.
		// Half of it is a generous floor that still fails a hot loop outright.
		if gap < fallback/2 {
			t.Errorf("gap %d between empty answers = %s, want about the fallback interval %s", i, gap, fallback)
		}
	}
}

// The default fallback is two seconds, and an explicit interval wins.
func TestControlLoopFallbackIntervalIsTwoSeconds(t *testing.T) {
	if got := (&ControlLoop{}).interval(); got != 2*time.Second {
		t.Fatalf("fallback interval = %s, want 2s for a control plane that answers immediately", got)
	}
	if got := (&ControlLoop{Interval: 5 * time.Second}).interval(); got != 5*time.Second {
		t.Fatalf("explicit interval = %s, want 5s", got)
	}
}

// The HTTP client timeout must comfortably exceed the hold, or every held poll
// becomes a timeout error. The hold ceiling names the bound Cloud's 25s hold
// must fit inside.
func TestControlPlaneClientTimeoutExceedsTheHold(t *testing.T) {
	timeout := (&HTTPControlPlane{}).client().Timeout
	if timeout <= controlHoldCeiling {
		t.Fatalf("client timeout = %s, must comfortably exceed the hold ceiling %s", timeout, controlHoldCeiling)
	}
}

// The loop drains EVERYTHING one response carried -- all commands and all reads
// -- before it decides whether to ask again. A response is a batch, not one item.
func TestControlLoopDrainsEveryItemInOneResponse(t *testing.T) {
	channel := &fakeChannel{work: &ControlWork{
		Commands: []Command{
			{ID: "c1", Action: "pause", Job: "job-a"},
			{ID: "c2", Action: "resume", Job: "job-b"},
		},
		Reads: []Read{
			{ID: "r1", Op: "jobs.list", Limit: 5},
			{ID: "r2", Op: "runs.list", Limit: 3},
		},
	}}
	runtime := &fakeCommandRuntime{readBody: json.RawMessage(`{"ok":true}`)}
	loop := &ControlLoop{Control: channel, Runtime: runtime, RuntimeID: "rt-a"}

	if err := loop.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(runtime.paused) != 1 || runtime.paused[0] != "job-a" {
		t.Fatalf("paused = %v, want job-a exactly once", runtime.paused)
	}
	if len(runtime.resumed) != 1 || runtime.resumed[0] != "job-b" {
		t.Fatalf("resumed = %v, want job-b exactly once", runtime.resumed)
	}
	if len(channel.commands) != 2 {
		t.Fatalf("command results = %d, want one per command", len(channel.commands))
	}
	if len(channel.reads) != 2 {
		t.Fatalf("read results = %d, want one per read", len(channel.reads))
	}
}
