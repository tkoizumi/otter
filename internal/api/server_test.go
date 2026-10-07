package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/logging"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/timeline"
)

// fakeBackend is an in-memory Backend that lets the real HTTP handler be
// exercised end to end through httptest.
type fakeBackend struct {
	// activeReleases is what GET /v1/runtime/releases/active reports.
	activeReleases []ReleaseView
	// installed records the bodies POST /v1/runtime/releases/install received.
	installed  []string
	installErr error
	mu         sync.Mutex

	version   string
	startedAt time.Time

	jobs       map[string]JobView
	webhookFor map[string]string
	runTokens  map[string]RunToken

	// Release activation (the agent's promote path).
	activated   []string
	activateErr error

	// Named, scoped API tokens (CL-21). The fake keys them by the presented
	// token, which is what ResolveAPIToken is handed.
	apiTokens   map[string]APIToken
	apiTokenLog []APITokenView
	tokenSeq    int

	runs      map[string]*runs.Run
	runOrder  []string
	nextRun   int
	logs      map[string][]runs.LogEntry
	nextLogID int64
	// overloadedJob, when set, makes SubmitRun refuse autonomous triggers for
	// that job with ErrOverloaded -- the max_queue_depth answer -- so the API's
	// status code for it is asserted rather than assumed.
	overloadedJob string
	// admissionRefusals is what the authenticated health view reports for jobs
	// whose max_queue_depth refused a trigger.
	admissionRefusals map[string]AdmissionRefusal
	// scheduleCounters is what it reports for schedules that folded or skipped
	// occurrences.
	scheduleCounters []HealthSchedule
	// maintenance is the runtime's maintenance state.
	maintenance MaintenanceView
	// gated makes SubmitRun refuse the way a gated runtime does, so the
	// refusal's status code is asserted rather than assumed.
	gated bool
	state map[string]map[string]json.RawMessage

	queueDepth int
	runCounts  map[string]int

	// Operational signals behind the authenticated health view.
	queueStats QueueStats
	freshness  map[string]time.Time
	storage    StorageStats

	// capture is the in-memory HTTP capture fake, defined in capture_test.go.
	capture *fakeCapture

	// timeline seam for the API contract tests, defined in timeline_test.go.
	timelinePage     *timeline.Page
	timelineErr      error
	timelineRequests []timeline.Request

	submitted []submittedRun
	cancelled []string

	pauseCalls []pauseCall
	pauseErr   error

	scheduleCalls []scheduleCall
	scheduleErr   error

	// schedByID is the in-memory schedule store behind the schedule-id
	// endpoints. It is keyed by schedule id, like the real store.
	schedByID map[string]ScheduleView
	schedSeq  int
	schedKeys map[string]string

	// jobConfigs is the in-memory configuration store, keyed by job reference.
	jobConfigs map[string]JobConfigView
	configSeq  int

	reloads   int
	reloadOut ReloadResult
	reloadErr error
}

type submittedRun struct {
	jobID   string
	payload TriggerPayload
}

// pauseCall records one pause or resume the handler forwarded to the backend.
type pauseCall struct {
	ref    string
	paused bool
}

// scheduleCall records one schedule change the handler forwarded to the backend.
type scheduleCall struct {
	ref  string
	cron string
}

var _ Backend = (*fakeBackend)(nil)

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		version:    "test-1.0.0",
		startedAt:  time.Now().Add(-90 * time.Second).UTC(),
		jobs:       map[string]JobView{},
		webhookFor: map[string]string{},
		runTokens:  map[string]RunToken{},
		runs:       map[string]*runs.Run{},
		logs:       map[string][]runs.LogEntry{},
		state:      map[string]map[string]json.RawMessage{},
		runCounts:  map[string]int{},
		freshness:  map[string]time.Time{},
		capture:    newFakeCapture(),
		schedByID:  map[string]ScheduleView{},
		schedKeys:  map[string]string{},
		jobConfigs: map[string]JobConfigView{},
	}
}

func (f *fakeBackend) addJob(id string, valid bool, webhookToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id] = JobView{
		ID:       id,
		Name:     id,
		Path:     "/jobs/" + id,
		Valid:    valid,
		Triggers: TriggerView{WebhookEnabled: webhookToken != "", WebhookToken: webhookToken},
	}
	if webhookToken != "" {
		f.webhookFor[id] = webhookToken
	}
}

func (f *fakeBackend) addRun(r *runs.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[r.ID] = r
	f.runOrder = append(f.runOrder, r.ID)
}

func (f *fakeBackend) addLogs(runID string, entries ...runs.LogEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range entries {
		if e.ID > f.nextLogID {
			f.nextLogID = e.ID
		}
		f.logs[runID] = append(f.logs[runID], e)
	}
}

func (f *fakeBackend) seedState(jobID, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state[jobID] == nil {
		f.state[jobID] = map[string]json.RawMessage{}
	}
	f.state[jobID][key] = json.RawMessage(value)
}

func (f *fakeBackend) submissionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submitted)
}

func (f *fakeBackend) lastSubmission() (submittedRun, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.submitted) == 0 {
		return submittedRun{}, false
	}
	return f.submitted[len(f.submitted)-1], true
}

func (f *fakeBackend) logsFor(runID string) []runs.LogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runs.LogEntry(nil), f.logs[runID]...)
}

// ------------------------------------------------------------- Backend impl

func (f *fakeBackend) Version() string { return f.version }

func (f *fakeBackend) StartedAt() time.Time { return f.startedAt }

func (f *fakeBackend) ListJobs(context.Context) []JobView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]JobView, 0, len(f.jobs))
	for _, v := range f.jobs {
		// A listing must never leak the webhook token.
		v.Triggers.WebhookToken = ""
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *fakeBackend) GetJob(id string) (JobView, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.jobs[id]
	return v, ok
}

func (f *fakeBackend) JobGeneration(id string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.jobs[id]
	if !ok {
		return 0, false
	}
	return v.Generation, true
}

func (f *fakeBackend) ResolveJob(ref string) (JobView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.jobs[ref]; ok {
		return v, nil
	}
	for _, v := range f.jobs {
		if v.Name == ref {
			return v, nil
		}
	}
	return JobView{}, fmt.Errorf("job %q: %w", ref, ErrNotFound)
}

func (f *fakeBackend) RegisterJob(_ context.Context, path string) (JobView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return JobView{ID: path, Name: path, Path: path, Valid: true}, nil
}

func (f *fakeBackend) ResetJob(_ context.Context, ref string) (ResetView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ResetView{OldID: ref, NewID: ref + "-new", Name: ref, Path: "/tmp/" + ref}, nil
}

func (f *fakeBackend) DeleteJob(_ context.Context, ref string) (DeletedView, error) {
	return DeletedView{Deleted: true, ID: ref, Name: ref}, nil
}

func (f *fakeBackend) MoveJob(_ context.Context, ref, destination string) (JobView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return JobView{ID: ref, Name: ref, Path: destination, Valid: true}, nil
}

// SetPaused records the call and mirrors it into the job's view, so a
// handler test can read back the trigger state the API reported.
func (f *fakeBackend) SetPaused(_ context.Context, ref string, paused bool) (PauseView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauseErr != nil {
		return PauseView{}, f.pauseErr
	}
	f.pauseCalls = append(f.pauseCalls, pauseCall{ref: ref, paused: paused})

	view, ok := f.jobs[ref]
	if !ok {
		return PauseView{}, ErrNotFound
	}
	changed := view.Triggers.Paused != paused
	view.Triggers.Paused = paused
	f.jobs[ref] = view

	out := PauseView{
		JobID:   ref,
		Name:    view.Name,
		Paused:  paused,
		Changed: changed,
	}
	if paused {
		at := time.Now().UTC()
		out.Since = &at
	}
	return out, nil
}

// SetSchedule records the cadence and mirrors it into the job's view, so a
// handler test can read back the schedule the API reported.
func (f *fakeBackend) SetSchedule(_ context.Context, ref, cron string) (ScheduleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scheduleErr != nil {
		return ScheduleView{}, f.scheduleErr
	}
	f.scheduleCalls = append(f.scheduleCalls, scheduleCall{ref: ref, cron: cron})

	view, ok := f.jobs[ref]
	if !ok {
		return ScheduleView{}, ErrNotFound
	}
	changed := view.Triggers.Cron != cron
	view.Triggers.Cron = cron
	f.jobs[ref] = view

	return ScheduleView{
		JobID:   ref,
		Name:    view.Name,
		Cron:    cron,
		Changed: changed,
	}, nil
}

// ListSchedules returns every fake schedule belonging to a job.
func (f *fakeBackend) ListSchedules(_ context.Context, ref string) ([]ScheduleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []ScheduleView{}
	for _, view := range f.schedByID {
		if view.JobID == ref {
			out = append(out, view)
		}
	}
	return out, nil
}

// CreateSchedule adds a fake schedule, honouring the idempotency key.
func (f *fakeBackend) CreateSchedule(_ context.Context, ref string, req ScheduleCreateRequest, idempotencyKey string) (ScheduleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scheduleErr != nil {
		return ScheduleView{}, f.scheduleErr
	}
	if idempotencyKey != "" {
		if id, ok := f.schedKeys[idempotencyKey]; ok {
			view := f.schedByID[id]
			view.Changed = false
			return view, nil
		}
	}
	f.schedSeq++
	id := "sched-" + itoa(f.schedSeq)
	view := ScheduleView{
		ID:           id,
		JobID:        ref,
		Cron:         req.Cron,
		Timezone:     req.Timezone,
		Payload:      req.Payload,
		MissedPolicy: req.MissedPolicy,
		Origin:       "api",
		Changed:      true,
	}
	f.schedByID[id] = view
	if idempotencyKey != "" {
		f.schedKeys[idempotencyKey] = id
	}
	return view, nil
}

func (f *fakeBackend) GetSchedule(_ context.Context, scheduleID string) (ScheduleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	view, ok := f.schedByID[scheduleID]
	if !ok {
		return ScheduleView{}, ErrNotFound
	}
	return view, nil
}

func (f *fakeBackend) UpdateSchedule(_ context.Context, scheduleID string, req ScheduleUpdateRequest) (ScheduleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	view, ok := f.schedByID[scheduleID]
	if !ok {
		return ScheduleView{}, ErrNotFound
	}
	if view.Origin == "manifest" {
		return ScheduleView{}, ErrConflict
	}
	changed := false
	if req.Cron != nil && view.Cron != *req.Cron {
		view.Cron = *req.Cron
		changed = true
	}
	if req.Timezone != nil {
		view.Timezone = *req.Timezone
		changed = true
	}
	if req.Payload != nil {
		view.Payload = *req.Payload
		changed = true
	}
	if req.MissedPolicy != nil {
		view.MissedPolicy = *req.MissedPolicy
		changed = true
	}
	view.Changed = changed
	f.schedByID[scheduleID] = view
	return view, nil
}

func (f *fakeBackend) DeleteSchedule(_ context.Context, scheduleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	view, ok := f.schedByID[scheduleID]
	if !ok {
		return ErrNotFound
	}
	if view.Origin == "manifest" {
		return ErrConflict
	}
	delete(f.schedByID, scheduleID)
	return nil
}

func (f *fakeBackend) SetSchedulePaused(_ context.Context, scheduleID string, paused bool) (ScheduleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	view, ok := f.schedByID[scheduleID]
	if !ok {
		return ScheduleView{}, ErrNotFound
	}
	changed := view.Paused != paused
	view.Paused = paused
	view.Changed = changed
	f.schedByID[scheduleID] = view
	return view, nil
}

// GetJobConfig mirrors the real backend: a job with no configuration yields an
// empty object and no version.
func (f *fakeBackend) GetJobConfig(_ context.Context, ref string) (JobConfigView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[ref]; !ok {
		return JobConfigView{}, ErrNotFound
	}
	view, ok := f.jobConfigs[ref]
	if !ok {
		return JobConfigView{JobID: ref, Values: json.RawMessage("{}")}, nil
	}
	return view, nil
}

// SetJobConfig writes a new version and moves the pointer, reporting changed
// only when the values differ.
func (f *fakeBackend) SetJobConfig(_ context.Context, ref string, values json.RawMessage, by string) (JobConfigView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[ref]; !ok {
		return JobConfigView{}, ErrNotFound
	}
	previous, ok := f.jobConfigs[ref]
	changed := !ok || string(previous.Values) != string(values)
	if len(values) == 0 {
		values = json.RawMessage("{}")
	}
	f.configSeq++
	view := JobConfigView{
		JobID:         ref,
		ConfigVersion: "cfg-" + itoa(f.configSeq),
		Values:        values,
		UpdatedBy:     by,
		Changed:       changed,
	}
	f.jobConfigs[ref] = view
	return view, nil
}

// itoa avoids importing strconv just for a fake id.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func (f *fakeBackend) Reload(_ context.Context) (ReloadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	if f.reloadErr != nil {
		return ReloadResult{}, f.reloadErr
	}
	return f.reloadOut, nil
}

func (f *fakeBackend) SubmitRun(_ context.Context, jobID string, payload TriggerPayload) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// The daemon refuses an autonomous trigger for a paused job and
	// admits a manual run regardless. The fake has to model that, because the
	// API's status code -- 503 rather than 409 -- is chosen from it.
	if view, ok := f.jobs[jobID]; ok && view.Triggers.Paused {
		if payload.Type != "" && payload.Type != TriggerManual {
			return "", fmt.Errorf("job %q is paused: %w", jobID, ErrPaused)
		}
	}

	// max_queue_depth is modelled as it reaches the API: a refusal the handler
	// has to turn into a status code. It governs autonomous triggers only -- a
	// manual run is exempt -- which is the daemon's rule, modelled here so the
	// manual-bypass assertion at the HTTP layer tests the real contract rather
	// than this fake.
	if f.overloadedJob == jobID && payload.Type != TriggerManual {
		return "", fmt.Errorf("job %q is at its max_queue_depth: %w", jobID, ErrOverloaded)
	}

	// The maintenance gate closes every path, including a manual run: unlike a
	// pause or a queue bound, there is nothing it does not apply to.
	if f.gated {
		return "", fmt.Errorf("runtime is under maintenance: %w", ErrGated)
	}

	f.submitted = append(f.submitted, submittedRun{jobID: jobID, payload: payload})
	f.nextRun++
	runID := fmt.Sprintf("submitted-%d", f.nextRun)
	f.runs[runID] = &runs.Run{
		ID:          runID,
		JobID:       jobID,
		TriggerType: payload.Type,
		Status:      runs.StatusQueued,
		Attempt:     1,
		CreatedAt:   time.Now().UTC(),
	}
	f.runOrder = append(f.runOrder, runID)
	return runID, nil
}

func (f *fakeBackend) CancelRun(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.runs[runID]; !ok {
		return ErrNotFound
	}
	f.cancelled = append(f.cancelled, runID)
	return nil
}

func (f *fakeBackend) GetRunDetail(_ context.Context, runID string) (*RunView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[runID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	// MaxAttempts stands in for the manifest ceiling the daemon resolves; a
	// non-zero value keeps the JSON round-trip of the field under test.
	return &RunView{Run: &cp, RootRunID: cp.ID, LatestStatus: cp.Status, MaxAttempts: 3, Attempts: []*runs.Run{&cp}}, nil
}

func (f *fakeBackend) ListRuns(_ context.Context, filter runs.Filter) ([]*runs.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*runs.Run{}
	for _, id := range f.runOrder {
		r := f.runs[id]
		if filter.JobID != "" && r.JobID != filter.JobID {
			continue
		}
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeBackend) RunLogs(_ context.Context, runID string, afterID int64, limit int) ([]runs.LogEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runs.LogEntry
	for _, e := range f.logs[runID] {
		if e.ID <= afterID {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeBackend) AppendRunLog(_ context.Context, runID, stream, message string, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextLogID++
	f.logs[runID] = append(f.logs[runID], runs.LogEntry{
		ID:        f.nextLogID,
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Stream:    stream,
		Message:   message,
	})
	return nil
}

func (f *fakeBackend) GetState(_ context.Context, jobID, key string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.state[jobID][key]
	if !ok {
		return nil, ErrNotFound
	}
	return append(json.RawMessage(nil), v...), nil
}

func (f *fakeBackend) SetState(_ context.Context, jobID, key string, value json.RawMessage) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state[jobID] == nil {
		f.state[jobID] = map[string]json.RawMessage{}
	}
	f.state[jobID][key] = append(json.RawMessage(nil), value...)
	return time.Now().UTC(), nil
}

func (f *fakeBackend) DeleteState(_ context.Context, jobID, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.state[jobID][key]; !ok {
		return false, nil
	}
	delete(f.state[jobID], key)
	return true, nil
}

func (f *fakeBackend) AllState(_ context.Context, jobID string) (map[string]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]json.RawMessage{}
	for k, v := range f.state[jobID] {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out, nil
}

func (f *fakeBackend) QueueDepth(context.Context) (int, error) { return f.queueDepth, nil }

func (f *fakeBackend) QueueStats(context.Context) (QueueStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queueStats, nil
}

// admissionRefusals is what the fake reports for the health surface. A test
// that wants a refusal visible sets it.
func (f *fakeBackend) AdmissionRefusals(context.Context) (map[string]AdmissionRefusal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admissionRefusals, nil
}

// maintenance is the fake's runtime maintenance state. Tests that exercise the
// gate set it; the default is serving with nothing running.
func (f *fakeBackend) Maintenance(context.Context) (MaintenanceView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maintenance, nil
}

func (f *fakeBackend) EnterMaintenance(_ context.Context, reason string) (MaintenanceView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maintenance = MaintenanceView{
		Mode: "draining", AcceptingWork: false, Explicit: true, Reason: reason,
		Running: f.maintenance.Running,
	}
	return f.maintenance, nil
}

func (f *fakeBackend) ExitMaintenance(context.Context) (MaintenanceView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maintenance = MaintenanceView{Mode: "serving", AcceptingWork: true, Explicit: true}
	return f.maintenance, nil
}

// ActivateRelease mirrors the daemon's contract closely enough to test the HTTP
// surface: it refuses while serving, because that ordering requirement is the
// property the endpoint exists to enforce.
// InstallRelease records the upload so a test can assert what was installed.
func (f *fakeBackend) InstallRelease(_ context.Context, r io.Reader) (ReleaseView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, err := io.ReadAll(r)
	if err != nil {
		return ReleaseView{}, err
	}
	f.installed = append(f.installed, string(body))
	if f.installErr != nil {
		return ReleaseView{}, f.installErr
	}
	return ReleaseView{Job: "sync", Digest: "sha256:installed", Digest_: "sha256:installed"}, nil
}

// ActiveReleases reports whatever the test seeded, so the served release can be
// asserted without a real release root.
func (f *fakeBackend) ActiveReleases(_ context.Context) ([]ReleaseView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activeReleases == nil {
		return []ReleaseView{}, nil
	}
	out := make([]ReleaseView, len(f.activeReleases))
	copy(out, f.activeReleases)
	return out, nil
}

func (f *fakeBackend) ActivateRelease(_ context.Context, digest string) (ReleaseView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if digest == "" {
		return ReleaseView{}, ErrInvalid
	}
	if f.maintenance.Mode != "maintenance" && f.maintenance.Mode != "draining" && f.maintenance.Mode != "startup" {
		return ReleaseView{}, ErrInvalid
	}
	if f.activateErr != nil {
		return ReleaseView{}, f.activateErr
	}
	f.activated = append(f.activated, digest)
	return ReleaseView{Job: "sync", Digest: digest}, nil
}

// scheduleCounters is what the fake reports for the per-schedule missed
// occurrence accounting.
func (f *fakeBackend) ScheduleCounters(context.Context) ([]HealthSchedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scheduleCounters, nil
}

func (f *fakeBackend) LastSuccessByJob(context.Context) (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]time.Time{}
	for id, at := range f.freshness {
		out[id] = at
	}
	return out, nil
}

func (f *fakeBackend) StorageStats(context.Context) (StorageStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storage, nil
}

func (f *fakeBackend) RunCounts(context.Context) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.runCounts {
		out[k] = v
	}
	return out, nil
}

func (f *fakeBackend) ResolveRunToken(token string) (RunToken, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scope, ok := f.runTokens[token]
	return scope, ok
}

func (f *fakeBackend) WebhookTokenFor(jobID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token, ok := f.webhookFor[jobID]
	return token, ok && token != ""
}

func (f *fakeBackend) CreateAPIToken(_ context.Context, name string, scope Scope) (APITokenCreated, error) {
	if strings.TrimSpace(name) == "" {
		return APITokenCreated{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if !scope.Valid() {
		return APITokenCreated{}, fmt.Errorf("%w: scope must be read or control", ErrInvalid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.apiTokens == nil {
		f.apiTokens = map[string]APIToken{}
	}
	f.tokenSeq++
	id := fmt.Sprintf("tok-%d", f.tokenSeq)
	token := "otter_test_" + id
	view := APITokenView{ID: id, Name: name, Scope: scope, CreatedAt: time.Now().UTC()}
	f.apiTokens[token] = APIToken{ID: id, Name: name, Scope: scope}
	f.apiTokenLog = append(f.apiTokenLog, view)
	return APITokenCreated{APITokenView: view, Token: token}, nil
}

func (f *fakeBackend) ListAPITokens(context.Context) ([]APITokenView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]APITokenView, len(f.apiTokenLog))
	copy(out, f.apiTokenLog)
	return out, nil
}

func (f *fakeBackend) RevokeAPIToken(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	known := false
	for token, authority := range f.apiTokens {
		if authority.ID == id {
			delete(f.apiTokens, token)
			known = true
		}
	}
	for i := range f.apiTokenLog {
		if f.apiTokenLog[i].ID == id {
			known = true
			now := time.Now().UTC()
			f.apiTokenLog[i].RevokedAt = &now
		}
	}
	return known, nil
}

func (f *fakeBackend) ResolveAPIToken(token string) (APIToken, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	authority, ok := f.apiTokens[token]
	return authority, ok
}

// ------------------------------------------------------------------ helpers

func testLogger() *logging.Logger {
	return logging.New(io.Discard, logging.FormatJSON, logging.LevelError)
}

func newTestServer(t *testing.T, cfg ServerConfig, b Backend) *httptest.Server {
	t.Helper()
	return httptest.NewServer(NewServer(cfg, b, testLogger()).Handler())
}

type httpResult struct {
	status int
	body   []byte
	header http.Header
}

func do(t *testing.T, method, url string, body []byte, hdr map[string]string) httpResult {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return httpResult{status: resp.StatusCode, body: data, header: resp.Header}
}

func (r httpResult) errorEnvelope(t *testing.T) ErrorResponse {
	t.Helper()
	var env ErrorResponse
	if err := json.Unmarshal(r.body, &env); err != nil {
		t.Fatalf("response body is not the JSON error envelope: %q: %v", r.body, err)
	}
	if env.Error.Code == "" {
		t.Fatalf("error envelope has no code: %q", r.body)
	}
	return env
}

func (r httpResult) decode(t *testing.T, out any) {
	t.Helper()
	if err := json.Unmarshal(r.body, out); err != nil {
		t.Fatalf("decode response %q: %v", r.body, err)
	}
}

func wantStatus(t *testing.T, r httpResult, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status = %d, want %d (body: %s)", r.status, want, r.body)
	}
}

// --------------------------------------------------------------------- tests

func TestAuthenticationWithoutTokenOnLoopback(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	for _, path := range []string{"/health", "/v1/jobs", "/v1/jobs/int-A", "/v1/runs"} {
		t.Run(path, func(t *testing.T) {
			r := do(t, http.MethodGet, srv.URL+path, nil, nil)
			wantStatus(t, r, http.StatusOK)
		})
	}
}

func TestAuthenticationWithAPIToken(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{APIToken: "s3cret-token"}, b)
	defer srv.Close()

	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"missing header", nil, http.StatusUnauthorized},
		{"wrong token", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"wrong scheme", map[string]string{"Authorization": "Basic s3cret-token"}, http.StatusUnauthorized},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, http.StatusUnauthorized},
		{"correct token", map[string]string{"Authorization": "Bearer s3cret-token"}, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, http.MethodGet, srv.URL+"/v1/jobs", nil, tc.hdr)
			wantStatus(t, r, tc.want)
			if tc.want == http.StatusUnauthorized {
				if env := r.errorEnvelope(t); env.Error.Code != CodeUnauthorized {
					t.Fatalf("error code = %q, want %q", env.Error.Code, CodeUnauthorized)
				}
			}
		})
	}
}

func TestRunTokenScopes(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.addJob("int-B", true, "")
	b.runTokens["run-token"] = RunToken{RunID: "run-A", JobID: "int-A"}
	now := time.Now().UTC()
	b.addRun(&runs.Run{ID: "run-A", JobID: "int-A", Status: runs.StatusQueued, Attempt: 1, CreatedAt: now})
	b.addRun(&runs.Run{ID: "run-B", JobID: "int-B", Status: runs.StatusQueued, Attempt: 1, CreatedAt: now})
	b.seedState("int-A", "present", "1")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	auth := map[string]string{"Authorization": "Bearer run-token"}

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		want   int
	}{
		{"read own run", http.MethodGet, "/v1/runs/run-A", nil, http.StatusOK},
		{"read own logs", http.MethodGet, "/v1/runs/run-A/logs", nil, http.StatusOK},
		{"append own log", http.MethodPost, "/v1/runs/run-A/logs", []byte(`{"message":"hi"}`), http.StatusCreated},
		{"read own state", http.MethodGet, "/v1/jobs/int-A/state", nil, http.StatusOK},
		{"read own state key", http.MethodGet, "/v1/jobs/int-A/state/present", nil, http.StatusOK},
		{"write own state key", http.MethodPut, "/v1/jobs/int-A/state/fresh", []byte(`{"x":1}`), http.StatusOK},
		{"delete own state key", http.MethodDelete, "/v1/jobs/int-A/state/present", nil, http.StatusOK},
		{"read other run", http.MethodGet, "/v1/runs/run-B", nil, http.StatusForbidden},
		{"read other run logs", http.MethodGet, "/v1/runs/run-B/logs", nil, http.StatusForbidden},
		{"append to other run", http.MethodPost, "/v1/runs/run-B/logs", []byte(`{"message":"hi"}`), http.StatusForbidden},
		{"read other job state", http.MethodGet, "/v1/jobs/int-B/state", nil, http.StatusForbidden},
		{"write other job state", http.MethodPut, "/v1/jobs/int-B/state/k", []byte(`1`), http.StatusForbidden},
		{"admin list jobs", http.MethodGet, "/v1/jobs", nil, http.StatusForbidden},
		{"admin submit run", http.MethodPost, "/v1/jobs/int-A/runs", nil, http.StatusForbidden},
		{"admin list runs", http.MethodGet, "/v1/runs", nil, http.StatusForbidden},
		{"admin cancel run", http.MethodPost, "/v1/runs/run-A/cancel", nil, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, tc.method, srv.URL+tc.path, tc.body, auth)
			wantStatus(t, r, tc.want)
			if tc.want == http.StatusForbidden {
				if env := r.errorEnvelope(t); env.Error.Code != CodeForbidden {
					t.Fatalf("error code = %q, want %q", env.Error.Code, CodeForbidden)
				}
			}
		})
	}
}

// A run token carries the identity generation it was authorized against. When
// a reset, move, retirement or deletion bumps that generation, a token minted
// before the change can no longer write state -- the token itself stays valid,
// which is exactly why the check has to happen at the mutation.
func TestStateWriteRefusesAStaleGeneration(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.jobs["int-A"] = JobView{ID: "int-A", Name: "counter", Valid: true, Generation: 3}
	b.runTokens["stale"] = RunToken{RunID: "run-A", JobID: "int-A", Generation: 2}
	b.seedState("int-A", "count", "1")

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	r := do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/state/count", []byte(`2`),
		map[string]string{"Authorization": "Bearer stale"})
	wantStatus(t, r, http.StatusConflict)
	if env := r.errorEnvelope(t); env.Error.Code != CodeConflict {
		t.Fatalf("error code = %q, want %q", env.Error.Code, CodeConflict)
	}

	// A token at the current generation is unaffected.
	b.runTokens["fresh"] = RunToken{RunID: "run-A", JobID: "int-A", Generation: 3}
	r = do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/state/count", []byte(`2`),
		map[string]string{"Authorization": "Bearer fresh"})
	wantStatus(t, r, http.StatusOK)
}

func TestWebhookAuthentication(t *testing.T) {
	b := newFakeBackend()
	b.addJob("hooked", true, "webhook-token")
	b.addJob("disabled", true, "")

	// A webhook caller must not need the admin bearer token.
	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	cases := []struct {
		name string
		path string
		hdr  map[string]string
		want int
	}{
		{"missing token", "/v1/hooks/hooked", nil, http.StatusUnauthorized},
		{"wrong token", "/v1/hooks/hooked", map[string]string{"X-Otter-Token": "nope"}, http.StatusUnauthorized},
		{"header token", "/v1/hooks/hooked", map[string]string{"X-Otter-Token": "webhook-token"}, http.StatusAccepted},
		{"query token", "/v1/hooks/hooked?token=webhook-token", nil, http.StatusAccepted},
		{"unknown job", "/v1/hooks/ghost", map[string]string{"X-Otter-Token": "webhook-token"}, http.StatusNotFound},
		{"webhook disabled", "/v1/hooks/disabled", map[string]string{"X-Otter-Token": "webhook-token"}, http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := b.submissionCount()
			r := do(t, http.MethodPost, srv.URL+tc.path, []byte(`{"event":"push"}`), tc.hdr)
			wantStatus(t, r, tc.want)

			switch tc.want {
			case http.StatusAccepted:
				var out SubmitRunResponse
				r.decode(t, &out)
				if out.RunID == "" || out.Status != string(runs.StatusQueued) {
					t.Fatalf("unexpected submit response: %+v", out)
				}
				last, ok := b.lastSubmission()
				if !ok || b.submissionCount() != before+1 {
					t.Fatalf("webhook did not reach SubmitRun")
				}
				if last.jobID != "hooked" || last.payload.Type != TriggerWebhook {
					t.Fatalf("unexpected submission: %+v", last)
				}
				if string(last.payload.Body) != `{"event":"push"}` {
					t.Fatalf("webhook body = %s, want the original JSON body", last.payload.Body)
				}
				if _, leaked := last.payload.Headers["X-Otter-Token"]; leaked {
					t.Fatalf("webhook token leaked into recorded headers")
				}
			case http.StatusNotFound:
				if b.submissionCount() != before {
					t.Fatalf("rejected webhook must not queue a run")
				}
			}
		})
	}
}

func TestHealthEndpoint(t *testing.T) {
	b := newFakeBackend()
	b.version = "v9.9.9"
	b.startedAt = time.Now().Add(-2 * time.Minute).UTC()
	b.addJob("ok", true, "")
	b.addJob("bad", false, "")
	b.queueDepth = 3
	b.runCounts = map[string]int{"queued": 1, "running": 2}

	// The operational signals. Their instants are fixed so the ages below are
	// checkable, not merely present.
	oldest := time.Now().Add(-10 * time.Minute).UTC()
	nextRetry := time.Now().Add(4 * time.Minute).UTC()
	lastSuccess := time.Now().Add(-3 * time.Minute).UTC()
	b.queueStats = QueueStats{
		ByJob:           map[string]int{"ok": 2, "bad": 1},
		OldestWaitingAt: &oldest,
		Retrying:        1,
		NextRetryAt:     &nextRetry,
	}
	b.freshness = map[string]time.Time{"ok": lastSuccess}
	b.storage = StorageStats{DBBytes: 4096, DiskFreeBytes: 1 << 30, DiskTotalBytes: 2 << 30}

	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	r := do(t, http.MethodGet, srv.URL+"/health", nil, nil)
	wantStatus(t, r, http.StatusOK)

	var raw map[string]json.RawMessage
	r.decode(t, &raw)
	for _, key := range []string{"status", "version", "uptime_seconds", "jobs", "queue_depth", "runs", "queue", "freshness", "storage"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("health response is missing %q: %s", key, r.body)
		}
	}
	var counts map[string]json.RawMessage
	if err := json.Unmarshal(raw["jobs"], &counts); err != nil {
		t.Fatalf("jobs is not an object: %v", err)
	}
	for _, key := range []string{"total", "valid", "invalid"} {
		if _, ok := counts[key]; !ok {
			t.Fatalf("jobs is missing %q: %s", key, raw["jobs"])
		}
	}

	var health HealthResponse
	r.decode(t, &health)
	if health.Status != "ok" {
		t.Fatalf("status = %q, want ok", health.Status)
	}
	if health.Version != "v9.9.9" {
		t.Fatalf("version = %q", health.Version)
	}
	if health.UptimeSeconds <= 0 {
		t.Fatalf("uptime_seconds = %v, want > 0", health.UptimeSeconds)
	}
	if health.Jobs == nil {
		t.Fatal("an authenticated /health response must include jobs")
	}
	if *health.Jobs != (HealthCounts{Total: 2, Valid: 1, Invalid: 1}) {
		t.Fatalf("jobs = %+v", *health.Jobs)
	}
	if health.QueueDepth == nil || *health.QueueDepth != 3 {
		t.Fatalf("queue_depth = %v, want 3", health.QueueDepth)
	}
	if health.Runs["running"] != 2 || health.Runs["queued"] != 1 {
		t.Fatalf("runs = %+v", health.Runs)
	}
	if health.Queue == nil {
		t.Fatal("an authenticated /health response must include the queue signals")
	}
	if health.Queue.OldestWaitingAt == nil || !health.Queue.OldestWaitingAt.Equal(oldest) {
		t.Errorf("oldest_waiting_at = %v, want %s", health.Queue.OldestWaitingAt, oldest)
	}
	if health.Queue.OldestWaitingSeconds == nil {
		t.Fatal("oldest_waiting_seconds is missing")
	}
	if age := *health.Queue.OldestWaitingSeconds; age < 599 || age > 601 {
		t.Errorf("oldest_waiting_seconds = %v, want about 600", age)
	}
	if health.Queue.ByJob["ok"] != 2 || health.Queue.ByJob["bad"] != 1 {
		t.Errorf("by_job = %v, want two for ok and one for bad", health.Queue.ByJob)
	}
	if health.Queue.Retrying != 1 {
		t.Errorf("retrying = %d, want 1", health.Queue.Retrying)
	}
	if health.Queue.NextRetryAt == nil || !health.Queue.NextRetryAt.Equal(nextRetry) {
		t.Errorf("next_retry_at = %v, want %s", health.Queue.NextRetryAt, nextRetry)
	}
	// Every job is named, and the ones without a success say so by omission.
	if len(health.Freshness) != 2 {
		t.Fatalf("freshness = %+v, want one entry per job", health.Freshness)
	}
	if health.Freshness[0].JobID != "bad" || health.Freshness[1].JobID != "ok" {
		t.Errorf("freshness order = %v, want it sorted by job id", health.Freshness)
	}
	if health.Freshness[0].LastSuccessAt != nil || health.Freshness[0].AgeSeconds != nil {
		t.Errorf("a job that never succeeded claims a success: %+v", health.Freshness[0])
	}
	if health.Freshness[1].LastSuccessAt == nil || !health.Freshness[1].LastSuccessAt.Equal(lastSuccess) {
		t.Errorf("ok last_success_at = %v, want %s", health.Freshness[1].LastSuccessAt, lastSuccess)
	}
	if health.Storage == nil || health.Storage.DBBytes != 4096 {
		t.Fatalf("storage = %+v, want db_bytes 4096", health.Storage)
	}
	if health.Storage.DiskTotalBytes != 2<<30 {
		t.Errorf("disk_total_bytes = %d, want %d", health.Storage.DiskTotalBytes, int64(2<<30))
	}
}

// TestHealthHidesCountersFromUnauthenticatedCallers covers the liveness probe:
// it must still answer 200, but must not disclose operational detail when a
// token is configured and none (or a wrong one) was presented.
//
// The field-count check is the point: the WS4 signals added to /health must not
// put one new key on the wire for an unauthenticated caller.
func TestHealthHidesCountersFromUnauthenticatedCallers(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.queueStats = QueueStats{Retrying: 1}
	b.storage = StorageStats{DBBytes: 4096}
	srv := newTestServer(t, ServerConfig{APIToken: "s3cret"}, b)
	defer srv.Close()

	healthURL := srv.URL + "/health"

	t.Run("no token", func(t *testing.T) {
		r := do(t, http.MethodGet, healthURL, nil, nil)
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 so liveness probes keep working", r.status)
		}
		var raw map[string]json.RawMessage
		r.decode(t, &raw)
		// schema_version is part of the liveness payload by design: it versions
		// the shape, discloses nothing about the workload, and a client needs it
		// before it authenticates.
		if len(raw) != 4 {
			t.Fatalf("unauthenticated /health has %d fields, want exactly schema_version, status, version and uptime_seconds: %s", len(raw), r.body)
		}
		for _, key := range []string{"schema_version", "status", "version", "uptime_seconds"} {
			if _, ok := raw[key]; !ok {
				t.Fatalf("unauthenticated /health is missing %q: %s", key, r.body)
			}
		}

		var health HealthResponse
		r.decode(t, &health)
		if health.Status != "ok" || health.Version == "" {
			t.Fatalf("liveness payload = %+v", health)
		}
		if health.Jobs != nil || health.QueueDepth != nil || len(health.Runs) != 0 ||
			health.Queue != nil || len(health.Freshness) != 0 || health.Storage != nil {
			t.Fatalf("unauthenticated /health leaked operational detail: %s", r.body)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		r := do(t, http.MethodGet, healthURL, nil, map[string]string{"Authorization": "Bearer nope"})
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.status)
		}
		var health HealthResponse
		r.decode(t, &health)
		if health.Jobs != nil {
			t.Fatalf("a wrong token must not reveal counters: %s", r.body)
		}
	})

	t.Run("valid token", func(t *testing.T) {
		r := do(t, http.MethodGet, healthURL, nil, map[string]string{"Authorization": "Bearer s3cret"})
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.status)
		}
		var health HealthResponse
		r.decode(t, &health)
		if health.Jobs == nil || health.QueueDepth == nil {
			t.Fatalf("an authenticated caller should see counters: %s", r.body)
		}
	})
}

func TestSubmitRunEndpoint(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	t.Run("empty body succeeds", func(t *testing.T) {
		before := b.submissionCount()
		r := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", nil, nil)
		wantStatus(t, r, http.StatusAccepted)
		var out SubmitRunResponse
		r.decode(t, &out)
		if out.RunID == "" || out.Status != string(runs.StatusQueued) {
			t.Fatalf("unexpected response: %+v", out)
		}
		last, ok := b.lastSubmission()
		if !ok || b.submissionCount() != before+1 {
			t.Fatalf("SubmitRun was not called")
		}
		if last.jobID != "int-A" || last.payload.Type != TriggerManual {
			t.Fatalf("unexpected submission: %+v", last)
		}
		if len(last.payload.Body) != 0 {
			t.Fatalf("empty body should not become a trigger body, got %q", last.payload.Body)
		}
	})

	t.Run("invalid json rejected", func(t *testing.T) {
		before := b.submissionCount()
		r := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", []byte(`{"not json`), nil)
		wantStatus(t, r, http.StatusBadRequest)
		if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
			t.Fatalf("error code = %q", env.Error.Code)
		}
		if b.submissionCount() != before {
			t.Fatalf("invalid body must not queue a run")
		}
	})

	t.Run("valid json passed through", func(t *testing.T) {
		body := []byte(`{"a":1,"b":[true,null]}`)
		r := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", body, nil)
		wantStatus(t, r, http.StatusAccepted)
		last, ok := b.lastSubmission()
		if !ok {
			t.Fatalf("SubmitRun was not called")
		}
		if string(last.payload.Body) != string(body) {
			t.Fatalf("trigger body = %s, want %s", last.payload.Body, body)
		}
	})
}

func TestListRunsValidation(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.addRun(&runs.Run{ID: "run-1", JobID: "int-A", Status: runs.StatusQueued, Attempt: 1, CreatedAt: time.Now().UTC()})
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	cases := []struct {
		query string
		want  int
	}{
		{"?status=bogus", http.StatusBadRequest},
		{"?limit=0", http.StatusBadRequest},
		{"?limit=abc", http.StatusBadRequest},
		{"?limit=-3", http.StatusBadRequest},
		{"?offset=abc", http.StatusBadRequest},
		{"?offset=-1", http.StatusBadRequest},
		{"?status=queued", http.StatusOK},
		{"?limit=5&offset=0", http.StatusOK},
		{"", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			r := do(t, http.MethodGet, srv.URL+"/v1/runs"+tc.query, nil, nil)
			wantStatus(t, r, tc.want)
			if tc.want == http.StatusBadRequest {
				if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
					t.Fatalf("error code = %q", env.Error.Code)
				}
			}
		})
	}
}

func TestJobEndpointsAndTokenScope(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "wh-secret-token")
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	t.Run("single includes webhook token", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/jobs/int-A", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var view JobView
		r.decode(t, &view)
		if view.ID != "int-A" {
			t.Fatalf("id = %q", view.ID)
		}
		if view.Triggers.WebhookToken != "wh-secret-token" {
			t.Fatalf("webhook_token = %q", view.Triggers.WebhookToken)
		}
		if !bytes.Contains(r.body, []byte(`"webhook_token"`)) {
			t.Fatalf("single job response has no webhook_token field: %s", r.body)
		}
	})

	t.Run("list never leaks webhook token", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/jobs", nil, nil)
		wantStatus(t, r, http.StatusOK)
		if bytes.Contains(r.body, []byte("wh-secret-token")) {
			t.Fatalf("list response leaked the webhook token: %s", r.body)
		}
		var out struct {
			Jobs []JobView `json:"jobs"`
		}
		r.decode(t, &out)
		if len(out.Jobs) != 1 {
			t.Fatalf("jobs = %d, want 1", len(out.Jobs))
		}
		for _, v := range out.Jobs {
			if v.Triggers.WebhookToken != "" {
				t.Fatalf("listed job carries a webhook token")
			}
		}
	})

	t.Run("unknown job is 404", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/jobs/ghost", nil, nil)
		wantStatus(t, r, http.StatusNotFound)
		if env := r.errorEnvelope(t); env.Error.Code != CodeNotFound {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})
}

func TestStateEndpoints(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.seedState("int-A", "num", "123")
	b.seedState("int-A", "obj", `{"a":1}`)
	b.seedState("int-A", "gone", `"bye"`)
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	rawCases := []struct {
		key  string
		want string
	}{
		{"num", "123"},
		{"obj", `{"a":1}`},
	}
	for _, tc := range rawCases {
		t.Run("raw "+tc.key, func(t *testing.T) {
			r := do(t, http.MethodGet, srv.URL+"/v1/jobs/int-A/state/"+tc.key, nil, nil)
			wantStatus(t, r, http.StatusOK)
			if got := strings.TrimRight(string(r.body), "\n"); got != tc.want {
				t.Fatalf("body = %q, want raw value %q (no envelope)", got, tc.want)
			}
			if ct := r.header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type = %q", ct)
			}
		})
	}

	t.Run("unknown key is 404", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/jobs/int-A/state/missing", nil, nil)
		wantStatus(t, r, http.StatusNotFound)
		if env := r.errorEnvelope(t); env.Error.Code != CodeNotFound {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})

	t.Run("all state", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/jobs/int-A/state", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var out StateResponse
		r.decode(t, &out)
		if len(out.State) != 3 {
			t.Fatalf("state = %v", out.State)
		}
		if string(out.State["num"]) != "123" {
			t.Fatalf("num = %s", out.State["num"])
		}
	})

	t.Run("put rejects non-json", func(t *testing.T) {
		r := do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/state/k", []byte("not json"), nil)
		wantStatus(t, r, http.StatusBadRequest)
		if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})

	t.Run("put echoes value", func(t *testing.T) {
		r := do(t, http.MethodPut, srv.URL+"/v1/jobs/int-A/state/k", []byte(`{"x":true}`), nil)
		wantStatus(t, r, http.StatusOK)
		var out SetStateResponse
		r.decode(t, &out)
		if out.JobID != "int-A" || out.Key != "k" {
			t.Fatalf("unexpected response: %+v", out)
		}
		if string(out.Value) != `{"x":true}` {
			t.Fatalf("value = %s", out.Value)
		}
		// The write must be visible to a following read.
		got := do(t, http.MethodGet, srv.URL+"/v1/jobs/int-A/state/k", nil, nil)
		wantStatus(t, got, http.StatusOK)
		if strings.TrimSpace(string(got.body)) != `{"x":true}` {
			t.Fatalf("read back = %s", got.body)
		}
	})

	t.Run("delete then 404", func(t *testing.T) {
		r := do(t, http.MethodDelete, srv.URL+"/v1/jobs/int-A/state/gone", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var out DeleteStateResponse
		r.decode(t, &out)
		if !out.Deleted || out.Key != "gone" {
			t.Fatalf("unexpected response: %+v", out)
		}

		again := do(t, http.MethodDelete, srv.URL+"/v1/jobs/int-A/state/gone", nil, nil)
		wantStatus(t, again, http.StatusNotFound)
		if env := again.errorEnvelope(t); env.Error.Code != CodeNotFound {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})
}

func TestRunLogEndpoints(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	now := time.Now().UTC()
	b.addRun(&runs.Run{ID: "run-1", JobID: "int-A", Status: runs.StatusRunning, Attempt: 1, CreatedAt: now})
	b.addLogs("run-1", runs.LogEntry{ID: 1, RunID: "run-1", Timestamp: now, Stream: runs.StreamOtter, Message: "first"})
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	t.Run("list logs", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-1/logs", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var out struct {
			Logs []runs.LogEntry `json:"logs"`
		}
		r.decode(t, &out)
		if len(out.Logs) != 1 || out.Logs[0].Message != "first" || out.Logs[0].Stream != runs.StreamOtter {
			t.Fatalf("logs = %+v", out.Logs)
		}
	})

	t.Run("after_id validation", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-1/logs?after_id=abc", nil, nil)
		wantStatus(t, r, http.StatusBadRequest)
		if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})

	t.Run("after_id filters", func(t *testing.T) {
		r := do(t, http.MethodGet, srv.URL+"/v1/runs/run-1/logs?after_id=1", nil, nil)
		wantStatus(t, r, http.StatusOK)
		var out struct {
			Logs []runs.LogEntry `json:"logs"`
		}
		r.decode(t, &out)
		if len(out.Logs) != 0 {
			t.Fatalf("logs = %+v, want none after id 1", out.Logs)
		}
	})

	t.Run("append log", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs",
			[]byte(`{"stream":"stdout","message":"hello"}`), nil)
		wantStatus(t, r, http.StatusCreated)

		logs := b.logsFor("run-1")
		if len(logs) != 2 {
			t.Fatalf("recorded logs = %+v", logs)
		}
		if logs[1].Stream != runs.StreamStdout || logs[1].Message != "hello" {
			t.Fatalf("recorded entry = %+v", logs[1])
		}
	})

	t.Run("append log defaults stream", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs", []byte(`{"message":"defaulted"}`), nil)
		wantStatus(t, r, http.StatusCreated)
		logs := b.logsFor("run-1")
		if logs[len(logs)-1].Stream != runs.StreamOtter {
			t.Fatalf("stream = %q, want %q", logs[len(logs)-1].Stream, runs.StreamOtter)
		}
	})

	t.Run("rejects bad stream", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs",
			[]byte(`{"stream":"bogus","message":"hi"}`), nil)
		wantStatus(t, r, http.StatusBadRequest)
		if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
			t.Fatalf("error code = %q", env.Error.Code)
		}
	})

	t.Run("rejects empty message", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs",
			[]byte(`{"stream":"stdout","message":"   "}`), nil)
		wantStatus(t, r, http.StatusBadRequest)
	})

	t.Run("rejects malformed body", func(t *testing.T) {
		r := do(t, http.MethodPost, srv.URL+"/v1/runs/run-1/logs", []byte(`[1,2,3]`), nil)
		wantStatus(t, r, http.StatusBadRequest)
	})
}

func TestUnknownRouteReturnsJSONEnvelope(t *testing.T) {
	b := newFakeBackend()
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	for _, req := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/does-not-exist"},
		{http.MethodPost, "/nope"},
	} {
		t.Run(req.method+" "+req.path, func(t *testing.T) {
			r := do(t, req.method, srv.URL+req.path, nil, nil)
			wantStatus(t, r, http.StatusNotFound)
			if ct := r.header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type = %q, want JSON", ct)
			}
			if env := r.errorEnvelope(t); env.Error.Code != CodeNotFound {
				t.Fatalf("error code = %q", env.Error.Code)
			}
		})
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	srv := newTestServer(t, ServerConfig{}, b)
	defer srv.Close()

	big := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	r := do(t, http.MethodPost, srv.URL+"/v1/jobs/int-A/runs", big, nil)
	wantStatus(t, r, http.StatusBadRequest)
	if env := r.errorEnvelope(t); env.Error.Code != CodeInvalid {
		t.Fatalf("error code = %q", env.Error.Code)
	}
}

func TestReloadEndpointIsAdminOnlyAndReturnsTheResult(t *testing.T) {
	b := newFakeBackend()
	b.addJob("int-A", true, "")
	b.runTokens["run-token"] = RunToken{RunID: "run-A", JobID: "int-A"}
	b.reloadOut = ReloadResult{
		Added:         []string{"int-B"},
		Total:         2,
		Valid:         2,
		RunsCancelled: 1,
	}

	srv := newTestServer(t, ServerConfig{APIToken: "admin-secret"}, b)
	defer srv.Close()

	admin := map[string]string{"Authorization": "Bearer admin-secret"}

	// A per-run token is authenticated but not an admin. Reload changes what
	// the whole runtime can address, so it is behind the admin token.
	forbidden := do(t, http.MethodPost, srv.URL+"/v1/reload", nil,
		map[string]string{"Authorization": "Bearer run-token"})
	wantStatus(t, forbidden, http.StatusForbidden)
	if env := forbidden.errorEnvelope(t); env.Error.Code != CodeForbidden {
		t.Errorf("error code = %q, want %q", env.Error.Code, CodeForbidden)
	}

	ok := do(t, http.MethodPost, srv.URL+"/v1/reload", nil, admin)
	wantStatus(t, ok, http.StatusOK)

	var got ReloadResult
	ok.decode(t, &got)
	if len(got.Added) != 1 || got.Added[0] != "int-B" {
		t.Errorf("added = %v, want [int-B]", got.Added)
	}
	if got.RunsCancelled != 1 {
		t.Errorf("runs_cancelled = %d, want 1", got.RunsCancelled)
	}
	if b.reloads != 1 {
		t.Errorf("backend reloads = %d, want 1", b.reloads)
	}

	// A reload already in progress is a conflict, not a server fault.
	b.mu.Lock()
	b.reloadErr = ErrConflict
	b.mu.Unlock()

	conflict := do(t, http.MethodPost, srv.URL+"/v1/reload", nil, admin)
	wantStatus(t, conflict, http.StatusConflict)
	if env := conflict.errorEnvelope(t); env.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", env.Error.Code, CodeConflict)
	}
}

// GET /v1/runtime/releases/active reports what each job is serving.
//
// It exists so the control plane can check evidence rather than accept an
// agent's claim, so the shape has to let a caller see WHICH job is on WHICH
// digest.
func TestActiveReleasesReportsTheServedDigest(t *testing.T) {
	fb := newFakeBackend()
	fb.activeReleases = []ReleaseView{
		{Job: "sync", Digest: "sha256:aaa", Digest_: "sha256:aaa"},
		{Job: "other", Digest: "sha256:bbb", Digest_: "sha256:bbb"},
	}
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, fb)

	res := do(t, http.MethodGet, srv.URL+"/v1/runtime/releases/active", nil,
		map[string]string{"Authorization": "Bearer " + adminToken})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.status, res.body)
	}
	var out struct {
		Active []ReleaseView `json:"active"`
	}
	if err := json.Unmarshal([]byte(res.body), &out); err != nil {
		t.Fatalf("unexpected shape: %v (%s)", err, res.body)
	}
	if len(out.Active) != 2 {
		t.Fatalf("got %d active releases, want 2: %s", len(out.Active), res.body)
	}
	// Both jobs, not one: a single digest cannot express a per-job answer, and the
	// control plane has to find the deployed digest among them.
	seen := map[string]string{}
	for _, v := range out.Active {
		seen[v.Job] = v.Digest
	}
	if seen["sync"] != "sha256:aaa" || seen["other"] != "sha256:bbb" {
		t.Errorf("wrong digests reported: %v", seen)
	}
}

// A runtime with nothing active reports an EMPTY LIST: "nothing is serving" is a
// normal state, and a caller iterating the result should not have to guard a null
// that means the same thing.
func TestActiveReleasesWithNothingActiveIsAnEmptyList(t *testing.T) {
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, newFakeBackend())
	res := do(t, http.MethodGet, srv.URL+"/v1/runtime/releases/active", nil,
		map[string]string{"Authorization": "Bearer " + adminToken})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.status, res.body)
	}
	if !strings.Contains(string(res.body), `"active":[]`) {
		t.Errorf("expected an empty array, got %s", res.body)
	}
}

// It names the code a tenant is running, which is operator information.
func TestActiveReleasesNeedsAdmin(t *testing.T) {
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, newFakeBackend())
	res := do(t, http.MethodGet, srv.URL+"/v1/runtime/releases/active", nil, nil)
	if res.status != http.StatusUnauthorized && res.status != http.StatusForbidden {
		t.Errorf("an unauthenticated read of the served release got %d, want 401/403", res.status)
	}
}

// The install endpoint accepts a package body and hands the BYTES to the backend,
// which is what makes verification the runtime's job rather than Cloud's.
func TestInstallReleasePassesThePackageThrough(t *testing.T) {
	fb := newFakeBackend()
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, fb)

	res := do(t, http.MethodPost, srv.URL+"/v1/runtime/releases/install",
		[]byte("package-bytes"), map[string]string{"Authorization": "Bearer " + adminToken})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.status, res.body)
	}
	if len(fb.installed) != 1 || fb.installed[0] != "package-bytes" {
		t.Errorf("the package body did not reach the backend verbatim: %q", fb.installed)
	}
}

// A package that is not what it claims is a 4xx, not a 500: it is the uploader's
// problem, and a 500 would send them looking at our logs.
func TestInstallReleaseReportsARefusalAsBadRequest(t *testing.T) {
	fb := newFakeBackend()
	fb.installErr = fmt.Errorf("%w: the package claims %s but its content hashes to %s",
		ErrInvalid, "sha256:aaa", "sha256:bbb")
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, fb)

	res := do(t, http.MethodPost, srv.URL+"/v1/runtime/releases/install",
		[]byte("tampered"), map[string]string{"Authorization": "Bearer " + adminToken})
	if res.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.status, res.body)
	}
	// Both digests must survive to the caller, or the CLI cannot tell a corrupted
	// transfer from a wrong manifest.
	body := string(res.body)
	if !strings.Contains(body, "sha256:aaa") || !strings.Contains(body, "sha256:bbb") {
		t.Errorf("the refusal must name both digests: %s", body)
	}
}

// Installing changes which releases exist, so an unauthenticated caller must not
// reach it.
func TestInstallReleaseNeedsAdmin(t *testing.T) {
	srv := newTestServer(t, ServerConfig{APIToken: adminToken}, newFakeBackend())
	res := do(t, http.MethodPost, srv.URL+"/v1/runtime/releases/install", []byte("x"), nil)
	if res.status != http.StatusUnauthorized && res.status != http.StatusForbidden {
		t.Errorf("an unauthenticated install got %d, want 401/403", res.status)
	}
}
