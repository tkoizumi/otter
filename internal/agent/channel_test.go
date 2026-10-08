package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
