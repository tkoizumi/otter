package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
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

// DeleteJob implements api.Backend. It purges everything the identity
// owns and leaves the source directory in place, suppressed so discovery does
// not immediately re-register it.
func (d *Daemon) DeleteJob(ctx context.Context, ref string) (api.DeletedView, error) {
	inst, err := d.resolveLifecycleRef(ctx, ref)
	if err != nil {
		return api.DeletedView{}, err
	}
	if inst.ID.IsZero() {
		return api.DeletedView{}, fmt.Errorf("job %q has no registry identity: %w", ref, api.ErrInvalid)
	}
	if err := d.ident.Delete(ctx, inst.ID, d.purgeInstance); err != nil {
		return api.DeletedView{}, err
	}
	if _, err := d.Reload(ctx); err != nil {
		return api.DeletedView{}, err
	}
	return api.DeletedView{
		Deleted: true,
		ID:      inst.ID.String(),
		Name:    inst.Name,
		Path:    inst.CanonicalPath,
	}, nil
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
	if err := (release.Manager{DataDir: d.cfg.DataDir}).DeleteAll(id); err != nil {
		return err
	}
	return nil
}
