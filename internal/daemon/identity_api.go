package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
)

// ResolveJob implements api.Backend. It resolves a label, a path or an
// id to the job it names, so the CLI never has to guess and never has
// to read the registry itself.
func (d *Daemon) ResolveJob(ref string) (api.JobView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.JobView{}, err
	}
	return d.jobView(entry, true), nil
}

// JobGeneration implements api.Backend. It is the check that lets a
// state write refuse a run token minted before the identity changed.
func (d *Daemon) JobGeneration(id string) (int64, bool) {
	entry, ok := d.reg.get(id)
	if !ok {
		return 0, false
	}
	return entry.Instance.Generation, true
}

// RegisterJob implements api.Backend. It registers a valid source
// directory explicitly, clearing any deletion suppression. It never adopts a
// marker that names an existing identity: an unowned path gets a fresh one.
func (d *Daemon) RegisterJob(ctx context.Context, path string) (api.JobView, error) {
	manifestPath := path
	if filepath.Base(manifestPath) != config.ManifestFileName {
		manifestPath = filepath.Join(path, config.ManifestFileName)
	}
	m, err := config.LoadAndValidate(manifestPath)
	if err != nil {
		return api.JobView{}, fmt.Errorf("register %s: %v: %w", path, err, api.ErrInvalid)
	}
	inst, err := d.ident.Register(ctx, filepath.Dir(manifestPath), m.Name)
	if err != nil {
		return api.JobView{}, err
	}
	if _, err := d.Reload(ctx); err != nil {
		return api.JobView{}, err
	}
	entry, ok := d.reg.get(inst.ID.String())
	if !ok {
		return api.JobView{}, fmt.Errorf("registered %s but the registry did not load it: %w", inst.ID, api.ErrNotFound)
	}
	return d.jobView(entry, true), nil
}

// ResetJob implements api.Backend. It retires the current identity and
// mints a fresh one at the same path, preserving the old data for explicit
// inspection or deletion.
func (d *Daemon) ResetJob(ctx context.Context, ref string) (api.ResetView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.ResetView{}, err
	}
	inst := entry.Instance
	if inst.ID.IsZero() {
		return api.ResetView{}, fmt.Errorf("job %q has no registry identity: %w", ref, api.ErrInvalid)
	}
	label := entry.Job.Name
	if label == "" {
		label = inst.ID.String()
	}

	// Refuse while the job is executed by the registry on purpose:
	// resetting is an explicit operator action, not a side effect of a scan.
	fresh, err := d.ident.Reset(ctx, inst.ID, label)
	if err != nil {
		return api.ResetView{}, err
	}
	if _, err := d.Reload(ctx); err != nil {
		return api.ResetView{}, err
	}
	return api.ResetView{
		OldID: inst.ID.String(),
		NewID: fresh.ID.String(),
		Name:  fresh.Name,
		Path:  fresh.CanonicalPath,
	}, nil
}

// DeleteJob implements api.Backend. It purges everything the job owns. A
// workspace job's source directory is left in place and suppressed so discovery
// does not immediately re-register it; a released-only job has no source
// directory and no registry row, and every one of its releases is removed.
//
// TWO SHAPES OF JOB, and the difference is which authority knows the job:
//
//   - a workspace job has a row in the identity registry. It is deleted
//     through identity.Service, which writes the tombstone and suppresses the
//     path, exactly as it always has.
//   - a released-only job has NO identity row. A pooled runtime discovers it
//     from the release store: the release root holds a directory named by the
//     durable job id and an active symlink, and discovery derives an in-memory
//     instance from those (released.go). Deleting it through the identity
//     service therefore fails with "instance not found" -- there is no row to
//     delete -- so it is purged directly, keyed by the same durable id.
//
// WHAT IS REMOVED, for both shapes: state, runs and their logs, captured
// payloads, the webhook token, the pause, schedules, job configuration and
// every staged release including the active link. Everything except the release
// store is keyed by the job id in its own table, which is why a job with no
// identity row can still have its runs history and captures removed.
//
// WHAT IS NOT REMOVED: source files in the jobs directory (a workspace delete
// has never removed them), and -- for a released-only job -- an identity
// tombstone or a path suppression, because there is no registry row to carry
// them. That last one is reported in the result rather than hidden: staging
// and activating a fresh release for the same id will make the job appear
// again, and a caller must not read "deleted" as "the id can never return".
// There is also no identity operation journal for a released-only job, so a
// purge that fails partway is finished by running the delete again rather than
// resumed: releases are removed LAST, so the job is still discoverable until
// the purge has run to completion.
//
// It refuses while the job has a run that has not finished. A queued, running
// or retrying run still owns the job's state and its release snapshot, so
// removing the job underneath it would either strand the run or delete data it
// is about to use. The refusal is an error and nothing is removed: the operator
// cancels first, then deletes.
func (d *Daemon) DeleteJob(ctx context.Context, ref string) (api.DeletedView, error) {
	inst, releasedOnly, err := d.deleteTarget(ctx, ref)
	if err != nil {
		return api.DeletedView{}, err
	}
	if inst.ID.IsZero() {
		return api.DeletedView{}, fmt.Errorf("job %q has no registry identity: %w", ref, api.ErrInvalid)
	}

	// Checked before ANY store is touched, so a refusal is never a partial
	// delete. Both the CLI's `otter delete` and the agent's `delete` command
	// arrive here, so the safety cannot be bypassed by choosing a channel.
	if err := d.refuseDeleteWhileInFlight(ctx, ref, inst.ID.String()); err != nil {
		return api.DeletedView{}, err
	}

	view := api.DeletedView{
		Deleted: true,
		ID:      inst.ID.String(),
		Name:    inst.Name,
	}
	if releasedOnly {
		// No identity row, so identity.Service.Delete has nothing to act on;
		// running the same purge directly is what keeps the two shapes removing
		// the same durable artifacts. The path is deliberately left out of the
		// view: a derived instance's "source" is the release snapshot itself,
		// and it is about to be removed.
		if err := d.purgeInstance(ctx, inst); err != nil {
			return api.DeletedView{}, err
		}
		view.Note = "released-only job: every release was removed and the derived registration dropped; " +
			"runs history, state, captures, webhook token, pause, schedules and configuration keyed by this id " +
			"were purged. No identity tombstone or path suppression was written, because the job has no registry " +
			"row, so staging and activating a new release for this id would make the job appear again."
	} else {
		if err := d.ident.Delete(ctx, inst.ID, d.purgeInstance); err != nil {
			return api.DeletedView{}, err
		}
		view.Path = inst.CanonicalPath
		view.Note = "source files were left in place; the path is suppressed until an explicit register."
	}

	// A reload makes the removal visible to the listing and the scheduler now,
	// rather than at the next restart. For a released-only job the release
	// directory is already gone, so discovery does not put it back; the derived
	// instance is rebuilt from disk on every pass and has nothing left to build
	// from.
	if _, err := d.Reload(ctx); err != nil {
		return api.DeletedView{}, err
	}
	return view, nil
}

// deleteTarget resolves a delete reference and reports whether the job exists
// only in the release store.
//
// The identity store is the discriminator, not the reference grammar: a job
// either has a registry row or it does not. A row means identity.Service owns
// the delete and writes the tombstone; no row means the job was derived from
// the release store and there is nothing for identity.Service to delete.
//
// When the registry knows nothing at all, the release store is consulted a last
// time by job id. That covers a release store that has been deactivated (no
// active link, so discovery does not surface the job) but whose releases are
// still on disk: a delete must still be able to remove them, or a job would
// become undeletable the moment its active link went away.
func (d *Daemon) deleteTarget(ctx context.Context, ref string) (identity.Instance, bool, error) {
	inst, err := d.resolveLifecycleRef(ctx, ref)
	if err != nil {
		if !errors.Is(err, api.ErrNotFound) {
			return identity.Instance{}, false, err
		}
		if target, ok := d.releasedTarget(trimIDPrefix(ref)); ok {
			return target, true, nil
		}
		return identity.Instance{}, false, err
	}
	if inst.ID.IsZero() {
		return inst, false, nil
	}

	_, rowErr := d.ident.Store().Instance(ctx, inst.ID)
	switch {
	case rowErr == nil:
		return inst, false, nil
	case errors.Is(rowErr, identity.ErrNotFound):
		return inst, true, nil
	default:
		// A store fault is not "no row": misreading it as a released-only job
		// would purge without the tombstone. Fail the delete instead.
		return identity.Instance{}, false, rowErr
	}
}

// trimIDPrefix removes the explicit `id:` spelling so a release-store lookup
// can use the bare directory name.
func trimIDPrefix(ref string) string {
	ref = strings.TrimSpace(ref)
	if id, ok := strings.CutPrefix(ref, "id:"); ok {
		return id
	}
	return ref
}

// deleteInFlightStatuses are the run statuses that refuse a delete. All three
// are non-terminal: the run has not finished with the job's state or its
// release snapshot.
var deleteInFlightStatuses = []runs.Status{runs.StatusQueued, runs.StatusRunning, runs.StatusRetrying}

// refuseDeleteWhileInFlight fails when the job has any run that has not
// finished.
//
// Cancel first is the requirement, not a hint: the operator (or the control
// plane) knows whether an in-flight run is wanted, and the runtime must not
// guess by killing it as a side effect of a delete. Naming the status and the
// run id in the error is what makes the next step obvious.
func (d *Daemon) refuseDeleteWhileInFlight(ctx context.Context, ref, jobID string) error {
	for _, status := range deleteInFlightStatuses {
		active, err := d.runs.List(ctx, runs.Filter{JobID: jobID, Status: status, Limit: 1})
		if err != nil {
			return fmt.Errorf("delete %s: check for %s runs: %w", ref, status, err)
		}
		if len(active) > 0 {
			return fmt.Errorf(
				"job %q has a %s run (%s); cancel it before deleting the job: %w",
				ref, status, active[0].ID, api.ErrConflict)
		}
	}
	return nil
}

// MoveJob implements api.Backend. It preserves the identity across a
// same-filesystem rename performed by the daemon, which is the only supported
// way to move a job without losing its state.
func (d *Daemon) MoveJob(ctx context.Context, ref, destination string) (api.JobView, error) {
	entry, err := d.resolveRef(ref)
	if err != nil {
		return api.JobView{}, err
	}
	inst := entry.Instance
	if inst.ID.IsZero() {
		return api.JobView{}, fmt.Errorf("job %q has no registry identity: %w", ref, api.ErrInvalid)
	}
	moved, err := d.ident.Move(ctx, inst.ID, destination)
	if err != nil {
		return api.JobView{}, err
	}
	if _, err := d.Reload(ctx); err != nil {
		return api.JobView{}, err
	}
	loaded, ok := d.reg.get(moved.ID.String())
	if !ok {
		return api.JobView{}, fmt.Errorf("moved %s but the registry did not load it: %w", moved.ID, api.ErrNotFound)
	}
	return d.jobView(loaded, true), nil
}

// purgeInstance removes the durable artifacts one identity owns. It is scoped
// by identity, never by a filesystem glob, so nothing shared is touched:
// content-addressed Python environments and the embedded SDK survive because
// they are not owned by one job.
func (d *Daemon) purgeInstance(ctx context.Context, inst identity.Instance) error {
	id := inst.ID.String()

	if _, err := d.state.DeleteAll(ctx, id); err != nil {
		return fmt.Errorf("delete state for %s: %w", id, err)
	}
	if _, err := d.runs.DeleteByJob(ctx, id); err != nil {
		return fmt.Errorf("delete runs for %s: %w", id, err)
	}
	if d.inspection != nil {
		if _, err := d.inspection.DeleteForJob(ctx, id); err != nil {
			return fmt.Errorf("delete capture for %s: %w", id, err)
		}
	}
	if _, err := d.db.ExecContext(ctx, `DELETE FROM webhook_tokens WHERE job_id = ?`, id); err != nil {
		return fmt.Errorf("delete webhook token for %s: %w", id, err)
	}
	if err := d.paused.Delete(ctx, id); err != nil {
		return err
	}
	if err := d.schedules.DeleteForJob(ctx, id); err != nil {
		return err
	}
	d.purgeJobConfig(ctx, id)
	if err := (release.Manager{DataDir: d.cfg.DataDir}).DeleteAll(id); err != nil {
		return err
	}
	return nil
}
