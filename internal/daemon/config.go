package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/jobconfig"
	"github.com/tkoizumi/otter/internal/runs"
)

// pinnedConfig resolves the configuration a run pinned at submission, as the
// JSON object the child will read through `ctx.config`.
//
// A run with no configuration, or one whose version no longer resolves,
// executes with an empty object rather than failing: configuration is an input
// to work that was already accepted, and refusing to run because a value went
// missing would turn a purge into a stranded queue. The miss is logged.
func (d *Daemon) pinnedConfig(run *runs.Run) string {
	if d.configs == nil || run.ConfigVersion == "" {
		return ""
	}
	version, ok, err := d.configs.Get(context.Background(), run.ConfigVersion)
	if err != nil {
		d.log.Warn("run_config_read_failed",
			"run_id", run.ID, "config_version", run.ConfigVersion, "error", err.Error())
		return ""
	}
	if !ok {
		d.log.Warn("run_config_missing", "run_id", run.ID, "config_version", run.ConfigVersion)
		return ""
	}
	return string(version.Values)
}

// GetJobConfig implements api.Backend.
func (d *Daemon) GetJobConfig(ctx context.Context, ref string) (api.JobConfigView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.JobConfigView{}, err
	}
	if entry.Instance.ID.IsZero() {
		return api.JobConfigView{}, fmt.Errorf("job %q has no registry identity: %w", ref, api.ErrNotFound)
	}
	view := api.JobConfigView{
		JobID: entry.Instance.ID.String(),
		Name:  jobName(entry),
		// An empty object, not null: "no configuration" and "configuration
		// with no keys" read the same to a job and should read the same here.
		Values: json.RawMessage("{}"),
	}
	current, ok, err := d.configs.Current(ctx, view.JobID)
	if err != nil {
		return api.JobConfigView{}, err
	}
	if ok {
		view.ConfigVersion = current.ID
		view.Values = current.Values
		at := current.CreatedAt
		view.UpdatedAt = &at
		view.UpdatedBy = current.CreatedBy
	}
	return view, nil
}

// SetJobConfig implements api.Backend. It writes a new immutable version and
// points the job at it; a no-op write reports changed=false. Runs already
// accepted keep the version they pinned.
func (d *Daemon) SetJobConfig(ctx context.Context, ref string, values json.RawMessage, by string) (api.JobConfigView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.JobConfigView{}, err
	}
	label := jobName(entry)
	inst := entry.Instance
	if inst.ID.IsZero() {
		return api.JobConfigView{}, fmt.Errorf("job %q has no registry identity: %w", label, api.ErrNotFound)
	}
	if !inst.Status.AcceptsWork() {
		return api.JobConfigView{}, fmt.Errorf("job %q is %s and cannot be configured: %w",
			label, inst.Status, api.ErrConflict)
	}

	version, changed, err := d.configs.Set(ctx, inst.ID.String(), values, by)
	if err != nil {
		return api.JobConfigView{}, configError(err)
	}
	if changed {
		d.log.Info("job_config_set", "job", label, "id", inst.ID.String(), "config_version", version.ID, "by", by)
	}
	at := version.CreatedAt
	return api.JobConfigView{
		JobID:         version.JobID,
		Name:          label,
		ConfigVersion: version.ID,
		Values:        version.Values,
		UpdatedAt:     &at,
		UpdatedBy:     version.CreatedBy,
		Changed:       changed,
	}, nil
}

// purgeJobConfig forgets a job's configuration and every version of it when its
// identity is deleted. A failure is logged, not returned: the identity is
// already gone.
func (d *Daemon) purgeJobConfig(ctx context.Context, jobID string) {
	if d.configs == nil {
		return
	}
	if err := d.configs.DeleteForJob(ctx, jobID); err != nil {
		d.log.Error("job_config_purge_failed", err, "job", jobID)
	}
}

// configError maps the jobconfig sentinels onto the API's.
func configError(err error) error {
	switch {
	case errors.Is(err, jobconfig.ErrNotFound):
		return fmt.Errorf("%v: %w", err, api.ErrNotFound)
	case errors.Is(err, jobconfig.ErrInvalid):
		return fmt.Errorf("%v: %w", err, api.ErrInvalid)
	default:
		return err
	}
}

// jobName is the manifest label for display, falling back to the identity.
func jobName(entry *registered) string {
	if entry.Job.Name != "" {
		return entry.Job.Name
	}
	return entry.Job.ID
}
