package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/pyenv"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
	"github.com/tkoizumi/otter/internal/state"
	"github.com/tkoizumi/otter/sdk"
)

// Integration views -----------------------------------------------------------

// ListIntegrations implements api.Backend. Webhook tokens are omitted here so
// that listing integrations never spills credentials.
func (d *Daemon) ListIntegrations() []api.IntegrationView {
	entries := d.reg.all()
	out := make([]api.IntegrationView, 0, len(entries))
	for _, entry := range entries {
		out = append(out, d.integrationView(entry, false))
	}
	return out
}

// GetIntegration implements api.Backend. It accepts a reference -- an id, a
// unique label, or a path -- and resolves it before building the view.
func (d *Daemon) GetIntegration(ref string) (api.IntegrationView, bool) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.IntegrationView{}, false
	}
	return d.integrationView(entry, true), true
}

func (d *Daemon) integrationView(entry *registered, includeWebhookToken bool) api.IntegrationView {
	it := entry.Integration

	view := api.IntegrationView{
		ID:         it.ID,
		Name:       it.Name,
		Path:       it.Dir,
		Valid:      it.Valid,
		Error:      it.Error,
		Generation: entry.Instance.Generation,
		Status:     string(entry.Instance.Status),
		Retry:      api.RetryView{Backoff: "none"},
		Triggers:   api.TriggerView{},
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
			Cron:           m.Cron(),
			WebhookEnabled: m.WebhookEnabled(),
		}
		if m.WebhookEnabled() {
			// The hook URL carries the durable identity, so renaming the
			// label does not break an external caller's webhook.
			view.Triggers.WebhookURL = "/v1/hooks/" + it.ID
		}
	}

	if d.sched != nil {
		if next, ok := d.sched.Next(it.ID); ok {
			at := next
			view.NextRunAt = &at
		}
	}

	if includeWebhookToken && entry.WebhookToken != "" {
		view.Triggers.WebhookToken = entry.WebhookToken
	}

	return view
}

// Runs -----------------------------------------------------------------------

// SubmitRun implements api.Backend. The run record and its queue entry are
// written in one transaction so a run can never exist without being queued.
func (d *Daemon) SubmitRun(ctx context.Context, ref string, payload api.TriggerPayload) (string, error) {
	if d.draining.Load() {
		return "", fmt.Errorf("otter is shutting down and is not accepting new runs: %w", api.ErrConflict)
	}

	entry, err := d.resolveRef(ref)
	if err != nil {
		return "", err
	}
	label := entry.Integration.Name
	if label == "" {
		label = entry.Integration.ID
	}
	// An integration that is present but invalid is still listed and still
	// named in the error; only the identity check below can make it unknown.
	if !entry.Integration.Valid || entry.Manifest == nil {
		return "", fmt.Errorf("integration %q cannot run: %s: %w",
			label, entry.Integration.Error, api.ErrInvalid)
	}
	inst := entry.Instance
	if inst.ID.IsZero() {
		return "", fmt.Errorf("integration %q has no registry identity: %w", label, api.ErrNotFound)
	}
	if !inst.Status.AcceptsWork() {
		return "", fmt.Errorf("integration %q is %s and cannot accept new runs: %w", label, inst.Status, api.ErrConflict)
	}
	integrationID := inst.ID.String()

	triggerType := payload.Type
	if triggerType == "" {
		triggerType = api.TriggerManual
	}

	metadata, err := encodeTriggerMetadata(payload, triggerType)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	// Every integration runs from an immutable release, so binding happens here,
	// at submission: activating a newer release cannot move a queued or retried
	// attempt onto different source code.
	//
	// The execution settings come from the BOUND RELEASE's manifest, never from
	// the live one. The live manifest describes code that may already be
	// different -- editing python.mode must not move a released run onto the
	// live tree -- while the snapshot is what actually executes.
	released, digest, ok, err := release.ActiveSourceDir(d.cfg.DataDir, integrationID)
	if err != nil {
		return "", err
	}
	if !ok {
		// A conflict with the integration's current state, not a server fault:
		// the request is well formed and the integration exists, but nothing has
		// been made live for it to run.
		return "", fmt.Errorf("integration %s has no active release; run otter release %s before submitting runs: %w",
			label, label, api.ErrConflict)
	}
	bound, err := config.LoadAndValidate(filepath.Join(released, config.ManifestFileName))
	if err != nil {
		return "", fmt.Errorf("integration %s: active release %s is invalid: %v; re-run otter release %s: %w",
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
		spec, err := manager.ResolveCurrent(context.Background(), sourceDir, integrationID)
		if err != nil {
			return "", fmt.Errorf("resolve managed Python for %s: %w", integrationID, err)
		}
		if _, err := manager.GetReady(spec); err != nil {
			return "", err
		}
		pythonVersion, environmentDigest, pythonPolicy = spec.Python, spec.Digest, spec.Policy
	}
	run := &runs.Run{
		ID:                    uuid.NewString(),
		IntegrationID:         integrationID,
		IntegrationName:       entry.Integration.Name,
		IntegrationGeneration: inst.Generation,
		TriggerType:           triggerType,
		Status:                runs.StatusQueued,
		Attempt:               1,
		CreatedAt:             now,
		Metadata:              metadata,
		PythonMode:            pythonMode,
		PythonVersion:         pythonVersion,
		EnvironmentDigest:     environmentDigest,
		PythonPolicy:          pythonPolicy,
		ReleaseDigest:         releaseDigest,
		ReleaseSourceDir:      releaseSourceDir,
		SDKVersion:            sdk.Version,
	}

	err = d.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := d.runs.CreateTx(ctx, tx, run); err != nil {
			return err
		}
		return d.queue.EnqueueTx(ctx, tx, run.ID, integrationID, now)
	})
	if err != nil {
		return "", fmt.Errorf("queue run for %s: %w", integrationID, err)
	}

	d.log.Info("run_queued",
		"integration", entry.Integration.Name,
		"id", integrationID,
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
			d.log.Info("run_cancelled", "integration", run.IntegrationID, "run_id", runID, "phase", "queued")
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
	d.log.Info("run_cancel_requested", "integration", run.IntegrationID, "run_id", runID)
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
	}, nil
}

// ListRuns implements api.Backend.
func (d *Daemon) ListRuns(ctx context.Context, f runs.Filter) ([]*runs.Run, error) {
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
	})
}

// State ----------------------------------------------------------------------

func (d *Daemon) requireIntegration(id string) error {
	if _, ok := d.reg.get(id); !ok {
		return fmt.Errorf("integration %q: %w", id, api.ErrNotFound)
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
// recreated integration must not read a previous one's values.
func (d *Daemon) stateScope(ref string) (string, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return "", err
	}
	if entry.Instance.ID.IsZero() {
		return "", fmt.Errorf("integration %q: %w", ref, api.ErrNotFound)
	}
	if !entry.Instance.Status.AcceptsWork() {
		return "", fmt.Errorf("integration %q is %s: %w", ref, entry.Instance.Status, api.ErrConflict)
	}
	return entry.Instance.ID.String(), nil
}

// GetState implements api.Backend.
func (d *Daemon) GetState(ctx context.Context, ref, key string) (json.RawMessage, error) {
	integrationID, err := d.stateScope(ref)
	if err != nil {
		return nil, err
	}
	if err := d.requireIntegration(integrationID); err != nil {
		return nil, err
	}
	value, err := d.state.Get(ctx, integrationID, key)
	return value, normalizeNotFound(err)
}

// SetState implements api.Backend.
func (d *Daemon) SetState(ctx context.Context, ref, key string, value json.RawMessage) (time.Time, error) {
	integrationID, err := d.stateScope(ref)
	if err != nil {
		return time.Time{}, err
	}
	if err := d.requireIntegration(integrationID); err != nil {
		return time.Time{}, err
	}
	updatedAt, err := d.state.Set(ctx, integrationID, key, value)
	return updatedAt, normalizeNotFound(err)
}

// DeleteState implements api.Backend.
func (d *Daemon) DeleteState(ctx context.Context, ref, key string) (bool, error) {
	integrationID, err := d.stateScope(ref)
	if err != nil {
		return false, err
	}
	if err := d.requireIntegration(integrationID); err != nil {
		return false, err
	}
	deleted, err := d.state.Delete(ctx, integrationID, key)
	return deleted, normalizeNotFound(err)
}

// AllState implements api.Backend.
func (d *Daemon) AllState(ctx context.Context, ref string) (map[string]json.RawMessage, error) {
	integrationID, err := d.stateScope(ref)
	if err != nil {
		return nil, err
	}
	if err := d.requireIntegration(integrationID); err != nil {
		return nil, err
	}
	all, err := d.state.All(ctx, integrationID)
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

// Tokens ---------------------------------------------------------------------

// ResolveRunToken implements api.Backend.
func (d *Daemon) ResolveRunToken(token string) (api.RunToken, bool) {
	return d.runTokens.Lookup(token)
}

// WebhookTokenFor implements api.Backend. It returns false both for an unknown
// integration and for one with the webhook trigger disabled. The reference is
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
