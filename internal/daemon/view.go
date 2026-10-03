package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/inspection"
	"github.com/tkoizumi/otter/internal/pyenv"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/state"
	"github.com/tkoizumi/otter/sdk"
)

// Job views -----------------------------------------------------------

// ListJobs implements api.Backend. Webhook tokens are omitted here so
// that listing jobs never spills credentials.
//
// Each view carries the job's last successful completion. It is read once for
// the whole listing rather than per job, which is what lets `otter jobs
// --schedule` render freshness without a run listing per job. A read that fails
// degrades to "no freshness" rather than failing the listing.
func (d *Daemon) ListJobs(ctx context.Context) []api.JobView {
	fresh := d.lastSuccessByJob(ctx)
	entries := d.reg.all()
	out := make([]api.JobView, 0, len(entries))
	for _, entry := range entries {
		view := d.jobView(entry, false)
		if at, ok := fresh[view.ID]; ok {
			instant := at
			view.LastSuccessAt = &instant
		}
		out = append(out, view)
	}
	return out
}

// lastSuccessByJob is the freshness half of ListJobs. A nil store (a partially
// built daemon in a test) or a failed read yields no freshness, never a panic.
func (d *Daemon) lastSuccessByJob(ctx context.Context) map[string]time.Time {
	if d.runs == nil {
		return nil
	}
	fresh, err := d.runs.LastSuccessByJob(ctx)
	if err != nil {
		d.log.Warn("job_freshness", "error", err.Error())
		return nil
	}
	return fresh
}

// GetJob implements api.Backend. It accepts a reference -- an id, a
// unique label, or a path -- and resolves it before building the view.
func (d *Daemon) GetJob(ref string) (api.JobView, bool) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.JobView{}, false
	}
	return d.jobView(entry, true), true
}

func (d *Daemon) jobView(entry *registered, includeWebhookToken bool) api.JobView {
	it := entry.Job

	view := api.JobView{
		ID:         it.ID,
		Name:       it.Name,
		Path:       it.Dir,
		Valid:      it.Valid,
		Error:      it.Error,
		Generation: entry.Instance.Generation,
		Status:     string(entry.Instance.Status),
		Retry:      api.RetryView{Backoff: "none"},
		Triggers:   api.TriggerView{},
		Capture:    d.CapturePolicyFor(entry.Manifest).String(),
	}
	if view.Name == "" {
		view.Name = it.ID
	}

	if m := entry.Manifest; m != nil {
		view.Description = m.Description
		view.Entrypoint = m.Entrypoint
		view.PythonExecutable = m.Python.Executable
		view.PythonMode = m.Python.Mode
		view.PythonPath = m.Python.Path
		view.TimeoutSeconds = m.Timeout.Seconds()
		view.Concurrency = m.Concurrency
		view.Env = m.Env
		view.Secrets = m.Secrets
		view.Retry = api.RetryView{
			Attempts:     m.Retry.Attempts,
			MaxAttempts:  m.MaxAttempts(),
			Backoff:      m.Retry.Backoff,
			InitialDelay: m.Retry.InitialDelay.String(),
			MaxDelay:     m.Retry.MaxDelay.String(),
		}
		view.Triggers = api.TriggerView{
			Cron:           d.jobCron(it.ID, m),
			WebhookEnabled: m.WebhookEnabled(),
		}
		if m.WebhookEnabled() {
			// The hook URL carries the durable identity, so renaming the
			// label does not break an external caller's webhook.
			view.Triggers.WebhookURL = "/v1/hooks/" + it.ID
		}
	}

	// The next run is the soonest of every schedule the job holds, so a job
	// with several cadences reports the one that will actually fire first.
	if d.sched != nil && d.schedules != nil {
		for _, rec := range d.schedules.ForJob(it.ID) {
			next, ok := d.sched.Next(rec.ID)
			if !ok {
				continue
			}
			if view.NextRunAt == nil || next.Before(*view.NextRunAt) {
				at := next
				view.NextRunAt = &at
			}
		}
	}

	// A paused job is unarmed, so it has no next run. Saying so
	// explicitly is what keeps "paused" from reading as "not yet due" or
	// "silently not firing".
	if d.paused != nil {
		if state := d.paused.Get(it.ID); state.Paused {
			view.Triggers.Paused = true
			if !state.Since.IsZero() {
				at := state.Since
				view.Triggers.PausedAt = &at
			}
		}
	}

	if includeWebhookToken && entry.WebhookToken != "" {
		view.Triggers.WebhookToken = entry.WebhookToken
	}

	return view
}

// CapturePolicyFor is the policy a new run of a job would use, ignoring
// any per-run override. It is what `otter inspect` reports, so an operator can
// see that payloads are being stored before a failure rather than after.
func (d *Daemon) CapturePolicyFor(manifest *config.Manifest) inspection.Policy {
	policy, err := d.resolveCapturePolicy("", manifest)
	if err != nil {
		return d.cfg.CaptureDefaultPolicy()
	}
	return policy
}

// resolveCapturePolicy applies the capture precedence: an explicit per-run
// override, then the job's declared policy, then the deployment
// default.
//
// An empty override or declaration means "no opinion", not "off", so a run only
// stops being captured when something explicitly says so. Every value that
// reaches here is validated, which is what keeps a typo from silently widening
// or disabling capture.
func (d *Daemon) resolveCapturePolicy(override inspection.Policy, manifest *config.Manifest) (inspection.Policy, error) {
	if override != "" {
		if !override.Valid() {
			return "", fmt.Errorf("invalid capture policy %q: use one of off, metadata, full", override)
		}
		return override, nil
	}
	if manifest != nil {
		if policy, ok := manifest.CapturePolicy(); ok {
			return policy, nil
		}
	}
	return d.cfg.CaptureDefaultPolicy(), nil
}

// Runs -----------------------------------------------------------------------

// SubmitRun implements api.Backend. The run record and its queue entry are
// written in one transaction so a run can never exist without being queued.
// SubmitRun implements api.Backend with the default submission options: a
// manual run still records metadata capture, like every other trigger.
func (d *Daemon) SubmitRun(ctx context.Context, ref string, payload api.TriggerPayload) (string, error) {
	return d.submitRun(ctx, ref, payload, api.SubmitRunOptions{}, nil)
}

// SubmitRunWithOptions implements api.Backend.
func (d *Daemon) SubmitRunWithOptions(ctx context.Context, ref string, payload api.TriggerPayload, opts api.SubmitRunOptions) (string, error) {
	return d.submitRun(ctx, ref, payload, opts, nil)
}

// fireRequest ties a run to the schedule occurrence that produced it. When it is
// present the occurrence ledger row is written in the same transaction as the
// run, so exactly one run exists per occurrence and a duplicate wake-up loses to
// the ledger's primary key.
type fireRequest struct {
	ScheduleID string
	Occurrence time.Time
}

// errAlreadyFired aborts a submit whose occurrence is already in the ledger.
var errAlreadyFired = errors.New("occurrence already fired")

// submitRun is the one place a trigger becomes a run. A cron fire passes a
// fireRequest; every other trigger passes nil.
func (d *Daemon) submitRun(ctx context.Context, ref string, payload api.TriggerPayload, opts api.SubmitRunOptions, fire *fireRequest) (string, error) {
	if d.draining.Load() {
		return "", fmt.Errorf("otter is shutting down and is not accepting new runs: %w", api.ErrConflict)
	}

	entry, err := d.resolveRef(ref)
	if err != nil {
		return "", err
	}
	label := entry.Job.Name
	if label == "" {
		label = entry.Job.ID
	}
	// A job that is present but invalid is still listed and still
	// named in the error; only the identity check below can make it unknown.
	if !entry.Job.Valid || entry.Manifest == nil {
		return "", fmt.Errorf("job %q cannot run: %s: %w",
			label, entry.Job.Error, api.ErrInvalid)
	}
	inst := entry.Instance
	if inst.ID.IsZero() {
		return "", fmt.Errorf("job %q has no registry identity: %w", label, api.ErrNotFound)
	}
	if !inst.Status.AcceptsWork() {
		return "", fmt.Errorf("job %q is %s and cannot accept new runs: %w", label, inst.Status, api.ErrConflict)
	}
	jobID := inst.ID.String()

	triggerType := payload.Type
	if triggerType == "" {
		triggerType = api.TriggerManual
	}

	// A pause suspends autonomous admission only. Cron and webhook triggers are
	// refused -- here, at the one place every trigger becomes a run, so no path
	// can slip past -- while a manual run is allowed through: an operator
	// asking for a run is not the thing that was paused.
	//
	// A retry never reaches this point. It is created by the worker from the
	// attempt it retries, so pausing stops new work without stranding a chain
	// that was already admitted.
	if triggerType != api.TriggerManual && d.paused.Paused(jobID) {
		return "", fmt.Errorf("job %q is paused; resume it with otter resume %q: %w",
			label, label, api.ErrPaused)
	}

	// The capture policy is resolved once, here, and then recorded on the run:
	// the child is told the result and cannot widen it, and a retry inherits
	// exactly what the run was submitted with.
	//
	// The job's own declaration is read from the LIVE manifest rather
	// than the bound release. Capture is a diagnostic switch, not code: turning
	// it off for a noisy or sensitive job must take effect on the next
	// reload, not wait for a new release.
	capturePolicy, err := d.resolveCapturePolicy(opts.Capture, entry.Manifest)
	if err != nil {
		return "", fmt.Errorf("%v: %w", err, api.ErrInvalid)
	}

	metadata, err := encodeTriggerMetadata(payload, triggerType)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	// Every job runs from an immutable release, so binding happens here,
	// at submission: activating a newer release cannot move a queued or retried
	// attempt onto different source code.
	//
	// The execution settings come from the BOUND RELEASE's manifest, never from
	// the live one. The live manifest describes code that may already be
	// different -- editing python.mode must not move a released run onto the
	// live tree -- while the snapshot is what actually executes.
	released, digest, ok, err := release.ActiveSourceDir(d.cfg.DataDir, jobID)
	if err != nil {
		return "", err
	}
	if !ok {
		// A conflict with the job's current state, not a server fault:
		// the request is well formed and the job exists, but nothing has
		// been made live for it to run.
		return "", fmt.Errorf("job %s has no active release; run otter release %s before submitting runs: %w",
			label, label, api.ErrConflict)
	}
	bound, err := config.LoadAndValidate(filepath.Join(released, config.ManifestFileName))
	if err != nil {
		return "", fmt.Errorf("job %s: active release %s is invalid: %v; re-run otter release %s: %w",
			label, shortDigest(digest), err, label, api.ErrConflict)
	}
	pythonMode := bound.Python.Mode
	if pythonMode == "" {
		pythonMode = "external"
	}
	releaseDigest, releaseSourceDir := digest, released
	sourceDir := released

	// A managed run binds to an environment now, at submission, so that a
	// later dependency change cannot silently move a queued run onto a
	// different interpreter. The daemon resolves the current identity here --
	// the one place on the run path where uv may be consulted -- and records
	// it, so execution never needs to resolve anything again.
	pythonVersion, environmentDigest, pythonPolicy := "", "", ""
	if pythonMode == "managed" {
		manager := pyenv.Manager{DataDir: d.cfg.DataDir}
		// Resolved against the snapshot, not the live tree: the snapshot is
		// what executes, so it is what the environment must match.
		spec, err := manager.ResolveCurrent(context.Background(), sourceDir, jobID)
		if err != nil {
			return "", fmt.Errorf("resolve managed Python for %s: %w", jobID, err)
		}
		if _, err := manager.GetReady(spec); err != nil {
			return "", err
		}
		pythonVersion, environmentDigest, pythonPolicy = spec.Python, spec.Digest, spec.Policy
	}
	run := &runs.Run{
		ID:                uuid.NewString(),
		JobID:             jobID,
		JobName:           entry.Job.Name,
		JobGeneration:     inst.Generation,
		TriggerType:       triggerType,
		Status:            runs.StatusQueued,
		Attempt:           1,
		CreatedAt:         now,
		Metadata:          metadata,
		PythonMode:        pythonMode,
		PythonVersion:     pythonVersion,
		EnvironmentDigest: environmentDigest,
		PythonPolicy:      pythonPolicy,
		ReleaseDigest:     releaseDigest,
		ReleaseSourceDir:  releaseSourceDir,
		SDKVersion:        sdk.Version,
		CapturePolicy:     capturePolicy.String(),
	}
	if fire != nil {
		run.ScheduleID = fire.ScheduleID
	}

	err = d.db.Tx(ctx, func(tx *sql.Tx) error {
		// The bound release must still exist. Retention renames a doomed
		// release aside inside its own immediate transaction, and this
		// transaction is also immediate, so the two cannot interleave: a run
		// committed before the prune is in the pin set, and a run committed
		// after it sees the directory gone here and is refused rather than
		// queued against a snapshot that no longer exists. This is the daemon
		// half of OT-010; the CLI prune is the other half.
		if _, statErr := os.Stat(run.ReleaseSourceDir); statErr != nil {
			return fmt.Errorf("the release bound to this run (%s) is no longer on disk; "+
				"it was pruned while the run was being submitted: %w",
				shortDigest(run.ReleaseDigest), api.ErrConflict)
		}
		// The ledger row goes first. When the occurrence is already recorded
		// the whole transaction aborts, so the duplicate produces neither a
		// run nor a queue entry.
		if fire != nil {
			recorded, err := d.schedules.RecordFireTx(ctx, tx, fire.ScheduleID, fire.Occurrence, run.ID)
			if err != nil {
				return err
			}
			if !recorded {
				return errAlreadyFired
			}
		}
		if err := d.runs.CreateTx(ctx, tx, run); err != nil {
			return err
		}
		return d.queue.EnqueueTx(ctx, tx, run.ID, jobID, now)
	})
	if err != nil {
		if errors.Is(err, errAlreadyFired) {
			return "", errAlreadyFired
		}
		return "", fmt.Errorf("queue run for %s: %w", jobID, err)
	}
	if fire != nil {
		d.schedules.NoteFired(fire.ScheduleID, fire.Occurrence)
	}

	// The summary is written before workers are woken, so a child can never
	// submit capture for a run whose recording does not exist yet.
	d.beginCapture(run.ID, jobID, capturePolicy)

	d.log.Info("run_queued",
		"job", entry.Job.Name,
		"id", jobID,
		"run_id", run.ID,
		"trigger", triggerType)
	d.appendOtterLog(run.ID, "run queued (trigger "+triggerType+")")
	d.notifyWorkers()

	return run.ID, nil
}

func encodeTriggerMetadata(payload api.TriggerPayload, triggerType string) (json.RawMessage, error) {
	meta := map[string]any{"type": triggerType}
	if len(payload.Body) > 0 {
		meta["body"] = json.RawMessage(payload.Body)
	}
	if len(payload.Headers) > 0 {
		meta["headers"] = payload.Headers
	}
	if payload.ScheduledAt != nil {
		meta["scheduled_at"] = payload.ScheduledAt.UTC().Format(time.RFC3339Nano)
	}

	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode trigger metadata: %w", err)
	}
	return encoded, nil
}

// CancelRun implements api.Backend.
func (d *Daemon) CancelRun(ctx context.Context, runID string) error {
	run, err := d.runs.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return fmt.Errorf("run %s is already %s: %w", runID, run.Status, api.ErrConflict)
	}

	// Still waiting: drop it from the queue and finish it immediately.
	if run.Status == runs.StatusQueued || run.Status == runs.StatusRetrying {
		removed, err := d.queue.Remove(ctx, runID)
		if err != nil {
			return err
		}
		if removed {
			if err := d.runs.Finish(ctx, runID, runs.Finish{
				Status:     runs.StatusCancelled,
				Error:      "cancelled by operator before execution",
				FinishedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
			d.appendOtterLog(runID, "run cancelled before execution")
			d.log.Info("run_cancelled", "job", run.JobID, "run_id", runID, "phase", "queued")
			return nil
		}
	}

	// Executing: signal the process. The worker records the final status.
	d.runCtlMu.Lock()
	ctl, ok := d.runCtl[runID]
	d.runCtlMu.Unlock()
	if !ok {
		return fmt.Errorf("run %s is not currently executing: %w", runID, api.ErrConflict)
	}

	ctl.setReason(reasonUser)
	ctl.cancel()
	d.log.Info("run_cancel_requested", "job", run.JobID, "run_id", runID)
	return nil
}

// GetRunDetail implements api.Backend.
func (d *Daemon) GetRunDetail(ctx context.Context, runID string) (*api.RunView, error) {
	root, attempts, err := d.runs.Chain(ctx, runID)
	if err != nil {
		return nil, normalizeNotFound(err)
	}

	target := root
	latest := root.Status
	for _, attempt := range attempts {
		latest = attempt.Status
		if attempt.ID == runID {
			target = attempt
		}
	}

	return &api.RunView{
		Run:          target,
		RootRunID:    root.ID,
		LatestStatus: latest,
		Attempts:     attempts,
		MaxAttempts:  d.maxAttemptsFor(target.JobID),
	}, nil
}

// maxAttemptsFor reports the retry ceiling the job's manifest currently
// allows, or 0 when the job is no longer registered. Run history
// outlives the registration (and a release can change the policy), so a missing
// manifest degrades to "unknown" rather than inventing a ceiling for a chain
// that has already run.
func (d *Daemon) maxAttemptsFor(jobID string) int {
	entry, ok := d.reg.get(jobID)
	if !ok || entry.Manifest == nil {
		return 0
	}
	return entry.Manifest.MaxAttempts()
}

// ListRuns implements api.Backend.
//
// A filter that names a job is a reference, not a raw key: the
// operator types a label (`otter runs counter`), the CLI's positional form, and
// the durable identity a run records is a UUID. Resolving here is what makes
// both the CLI and `GET /v1/runs?job_id=counter` mean the one
// job instead of silently matching nothing. Legacy rows, keyed by the
// label before identities existed, are matched alongside the identity so a
// migrated workspace does not lose its history.
func (d *Daemon) ListRuns(ctx context.Context, f runs.Filter) ([]*runs.Run, error) {
	if f.JobID != "" {
		entry, err := d.resolveRef(f.JobID)
		if err != nil {
			return nil, err
		}
		ids := []string{entry.Job.ID}
		if name := entry.Job.Name; name != "" && name != entry.Job.ID {
			ids = append(ids, name)
		}
		f.JobIDs = ids
		f.JobID = ""
	}
	return d.runs.List(ctx, f)
}

// RunLogs implements api.Backend.
func (d *Daemon) RunLogs(ctx context.Context, runID string, afterID int64, limit int) ([]runs.LogEntry, error) {
	if _, err := d.runs.Get(ctx, runID); err != nil {
		return nil, normalizeNotFound(err)
	}
	return d.logs.List(ctx, runID, afterID, limit)
}

// AppendRunLog implements api.Backend. Structured fields are rendered as a
// trailing JSON object so they survive in the plain-text log stream.
func (d *Daemon) AppendRunLog(ctx context.Context, runID, stream, message string, fields map[string]any) error {
	if _, err := d.runs.Get(ctx, runID); err != nil {
		return normalizeNotFound(err)
	}

	text := message
	if len(fields) > 0 {
		if encoded, err := json.Marshal(fields); err == nil {
			text = message + " " + string(encoded)
		}
	}

	return d.logs.Append(ctx, runs.LogEntry{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Stream:    stream,
		Message:   text,
		Origin:    runs.OriginChild,
	})
}

// State ----------------------------------------------------------------------

func (d *Daemon) requireJob(id string) error {
	if _, ok := d.reg.get(id); !ok {
		return fmt.Errorf("job %q: %w", id, api.ErrNotFound)
	}
	return nil
}

// normalizeNotFound keeps the api.Backend error contract uniform: every
// "missing thing" surfaces as api.ErrNotFound regardless of which store
// produced it, so the HTTP layer and the CLI only need one sentinel.
func normalizeNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, api.ErrNotFound) {
		return err
	}
	if errors.Is(err, runs.ErrNotFound) || errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("%s: %w", err.Error(), api.ErrNotFound)
	}
	return err
}

// stateScope resolves a reference to the durable identity that namespaces
// state. State is always stored under the identity, never the label: a
// recreated job must not read a previous one's values.
func (d *Daemon) stateScope(ref string) (string, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return "", err
	}
	if entry.Instance.ID.IsZero() {
		return "", fmt.Errorf("job %q: %w", ref, api.ErrNotFound)
	}
	if !entry.Instance.Status.AcceptsWork() {
		return "", fmt.Errorf("job %q is %s: %w", ref, entry.Instance.Status, api.ErrConflict)
	}
	return entry.Instance.ID.String(), nil
}

// GetState implements api.Backend.
func (d *Daemon) GetState(ctx context.Context, ref, key string) (json.RawMessage, error) {
	jobID, err := d.stateScope(ref)
	if err != nil {
		return nil, err
	}
	if err := d.requireJob(jobID); err != nil {
		return nil, err
	}
	value, err := d.state.Get(ctx, jobID, key)
	return value, normalizeNotFound(err)
}

// SetState implements api.Backend.
func (d *Daemon) SetState(ctx context.Context, ref, key string, value json.RawMessage) (time.Time, error) {
	jobID, err := d.stateScope(ref)
	if err != nil {
		return time.Time{}, err
	}
	if err := d.requireJob(jobID); err != nil {
		return time.Time{}, err
	}
	updatedAt, err := d.state.Set(ctx, jobID, key, value)
	return updatedAt, normalizeNotFound(err)
}

// DeleteState implements api.Backend.
func (d *Daemon) DeleteState(ctx context.Context, ref, key string) (bool, error) {
	jobID, err := d.stateScope(ref)
	if err != nil {
		return false, err
	}
	if err := d.requireJob(jobID); err != nil {
		return false, err
	}
	deleted, err := d.state.Delete(ctx, jobID, key)
	return deleted, normalizeNotFound(err)
}

// AllState implements api.Backend.
func (d *Daemon) AllState(ctx context.Context, ref string) (map[string]json.RawMessage, error) {
	jobID, err := d.stateScope(ref)
	if err != nil {
		return nil, err
	}
	if err := d.requireJob(jobID); err != nil {
		return nil, err
	}
	all, err := d.state.All(ctx, jobID)
	return all, normalizeNotFound(err)
}

// Introspection ---------------------------------------------------------------

// QueueDepth implements api.Backend.
func (d *Daemon) QueueDepth(ctx context.Context) (int, error) {
	return d.queue.Depth(ctx)
}

// RunCounts implements api.Backend.
func (d *Daemon) RunCounts(ctx context.Context) (map[string]int, error) {
	counts, err := d.runs.CountByStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(counts))
	for status, n := range counts {
		out[string(status)] = n
	}
	return out, nil
}

// QueueStats implements api.Backend. It answers what a depth count cannot: how
// long the oldest claimable run has waited, which job is backing up, and when
// the next retry is due.
func (d *Daemon) QueueStats(ctx context.Context) (api.QueueStats, error) {
	var out api.QueueStats
	if d.queue != nil {
		byJob, err := d.queue.DepthByJob(ctx)
		if err != nil {
			return out, err
		}
		out.ByJob = byJob

		oldest, ok, err := d.queue.OldestWaiting(ctx, time.Now().UTC())
		if err != nil {
			return out, err
		}
		if ok {
			out.OldestWaitingAt = &oldest
		}

		next, ok, err := d.queue.NextRetryAt(ctx, string(runs.StatusRetrying))
		if err != nil {
			return out, err
		}
		if ok {
			out.NextRetryAt = &next
		}
	}
	if d.runs != nil {
		counts, err := d.runs.CountByStatus(ctx)
		if err != nil {
			return out, err
		}
		out.Retrying = counts[runs.StatusRetrying]
	}
	return out, nil
}

// LastSuccessByJob implements api.Backend. A job absent from the map has never
// succeeded.
func (d *Daemon) LastSuccessByJob(ctx context.Context) (map[string]time.Time, error) {
	if d.runs == nil {
		return map[string]time.Time{}, nil
	}
	return d.runs.LastSuccessByJob(ctx)
}

// StorageStats implements api.Backend. The database size is page_count *
// page_size, so it excludes the WAL file. The disk numbers cover the filesystem
// holding the data directory; a platform that cannot report them leaves those
// fields zero rather than failing the whole health response.
func (d *Daemon) StorageStats(ctx context.Context) (api.StorageStats, error) {
	var out api.StorageStats
	if d.db != nil {
		pages, err := d.pragmaInt(ctx, "page_count")
		if err != nil {
			return out, err
		}
		pageSize, err := d.pragmaInt(ctx, "page_size")
		if err != nil {
			return out, err
		}
		out.DBBytes = pages * pageSize
	}
	if d.cfg.DataDir != "" {
		if free, total, err := diskSpace(d.cfg.DataDir); err == nil {
			out.DiskFreeBytes = free
			out.DiskTotalBytes = total
		}
	}
	return out, nil
}

// pragmaInt reads a single-integer PRAGMA. A PRAGMA name cannot be
// parameterized, so every caller passes a constant, never input.
func (d *Daemon) pragmaInt(ctx context.Context, name string) (int64, error) {
	var v int64
	if err := d.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		return 0, fmt.Errorf("daemon: read pragma %s: %w", name, err)
	}
	return v, nil
}

// Tokens ---------------------------------------------------------------------

// ResolveRunToken implements api.Backend.
func (d *Daemon) ResolveRunToken(token string) (api.RunToken, bool) {
	return d.runTokens.Lookup(token)
}

// WebhookTokenFor implements api.Backend. It returns false both for an unknown
// job and for one with the webhook trigger disabled. The reference is
// resolved like any other, so a hook URL that carries either the durable
// identity or a still-unambiguous label keeps working.
func (d *Daemon) WebhookTokenFor(ref string) (string, bool) {
	entry, err := d.resolveRef(ref)
	if err != nil || entry.Manifest == nil || !entry.Manifest.WebhookEnabled() {
		return "", false
	}
	if entry.WebhookToken == "" {
		return "", false
	}
	return entry.WebhookToken, true
}
