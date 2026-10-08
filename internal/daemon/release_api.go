package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
)

// ActivateRelease makes a verified release active.
//
// This exists so the runtime agent can promote a release through the same API an
// operator uses, rather than reaching past it into the data directory. That is
// the property the agent design depends on: the agent must not be able to do
// anything an operator could not, and every action must be auditable in one
// place.
//
// Activation by digest rather than by job name, because a pull-based deploy is
// told "run release abc123" and does not necessarily know which job that release
// belongs to. The job is resolved from the release metadata, which is what makes
// the digest the identifier.
func (d *Daemon) ActivateRelease(ctx context.Context, digest string) (api.ReleaseView, error) {
	// The control plane sends the canonical `sha256:<hex>` form while the store is
	// keyed by bare hex, so normalise at the boundary. Comparing spellings instead
	// of identities was the "no job has release sha256:..." failure.
	digest, err := release.NormalizeDigest(digest)
	if err != nil {
		return api.ReleaseView{}, fmt.Errorf("activate release: %w", api.ErrInvalid)
	}

	job, err := d.jobForDigest(digest)
	if err != nil {
		return api.ReleaseView{}, err
	}

	// Activation is a write to the release layout, so it happens while the
	// runtime is gated. A caller that skipped EnterMaintenance would be swapping
	// the active release under running work.
	if !d.maint.Gated() {
		return api.ReleaseView{}, fmt.Errorf("activate release: runtime is serving; enter maintenance first: %w", api.ErrInvalid)
	}

	m := release.Manager{DataDir: d.cfg.DataDir}
	meta, err := m.Metadata(job, digest)
	if err != nil {
		return api.ReleaseView{}, fmt.Errorf("activate release: %s: %w", digest, api.ErrConflict)
	}
	if err := m.Activate(job, digest); err != nil {
		return api.ReleaseView{}, fmt.Errorf("activate release: %s: %w", digest, err)
	}
	d.log.Info("release_activated", "job", job, "digest", digest)

	// Activation is the moment a released job becomes runnable, so the registry
	// is rebuilt now rather than waiting for a restart. This is a rescan
	// TRIGGER, not the registration itself: discovery reads the release store,
	// so a start or a reload converges on the same set from disk. Measured
	// before this: a release was installed and activated, yet the job stayed
	// unregistered and `POST /v1/jobs/sync/runs` answered 404 while the release
	// was serving.
	//
	// A failed rescan does not undo the activation: the release IS active, and
	// reporting failure would have the agent retry a swap that already landed.
	// The next discovery pass converges, so the failure is logged, not returned.
	if _, err := d.Reload(ctx); err != nil {
		d.log.Error("release_activated_reload_failed", err, "job", job, "digest", digest)
	}

	return api.ReleaseView{
		Job:     job,
		Digest:  digest,
		Digest_: meta.Digest,
	}, nil
}

// jobForDigest finds which job owns a release digest.
//
// A digest is content-addressed, so it is unique across jobs in practice; this
// scans the release root rather than requiring the caller to know the job. A
// digest that matches nothing is an error rather than a guess, because
// activating the wrong job's release would deploy one tenant's code to another.
func (d *Daemon) jobForDigest(digest string) (string, error) {
	m := release.Manager{DataDir: d.cfg.DataDir}
	root, err := m.Root()
	if err != nil {
		return "", fmt.Errorf("activate release: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("activate release: %w", err)
	}
	matches := []string{}
	for _, e := range entries {
		job := e.Name()
		if !e.IsDir() || job == "active" {
			continue
		}
		if _, err := m.Metadata(job, digest); err == nil {
			matches = append(matches, job)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("activate release: no job has release %s: %w", digest, api.ErrNotFound)
	case 1:
		return matches[0], nil
	default:
		// Ambiguous. Refusing is the only safe answer: picking one would deploy
		// one tenant's code to another.
		return "", fmt.Errorf("activate release: release %s is present under %d jobs (%s); refusing to guess: %w",
			digest, len(matches), strings.Join(matches, ", "), api.ErrConflict)
	}
}

// ActiveReleases reports what each job is currently serving.
//
// Read by the control plane's evidence rule, which must not accept an agent's
// CLAIM that it applied a release. Before this the runtime could only be told
// what to run, never asked what it was running, so the only available evidence
// was the agent's own word -- which is precisely what the unknown-outcome rule
// exists to refuse.
//
// One entry per job, because a runtime manages several and the active release is
// per job. Taken from each job's `active` symlink rather than from the last
// activation this process performed: the answer must survive a restart, and a
// release activated before this process started is still the one serving.
func (d *Daemon) ActiveReleases(ctx context.Context) ([]api.ReleaseView, error) {
	_ = ctx
	m := release.Manager{DataDir: d.cfg.DataDir}
	root, err := m.Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		// No release root yet is an empty answer, not a failure: a runtime that
		// has never been given a release is a normal state, and reporting it as an
		// error would make the control plane treat a fresh runtime as broken.
		if os.IsNotExist(err) {
			return []api.ReleaseView{}, nil
		}
		return nil, err
	}
	out := []api.ReleaseView{}
	for _, e := range entries {
		job := e.Name()
		if !e.IsDir() || job == "active" {
			continue
		}
		meta, ok, err := m.Active(job)
		if err != nil {
			// A job whose active symlink cannot be resolved is reported as absent
			// rather than failing the whole call: one broken job must not hide
			// every other job's release, or the control plane would see "nothing
			// active" and conclude a deploy had not landed.
			d.log.Warn("active_release_unreadable", "job", job, "error", err.Error())
			continue
		}
		if !ok {
			continue
		}
		out = append(out, api.ReleaseView{
			Job:     job,
			Digest:  meta.Digest,
			Digest_: meta.Digest,
		})
	}
	return out, nil
}

// ManagedReleaseJobs reports the jobs this runtime holds ONLY in its release
// store: those with no source in the jobs directory.
//
// This is the discriminator an agent needs and must not guess at. A pooled
// tenant runs with an empty jobs directory, so every job it knows about came
// from a Cloud deploy and lives only as a release directory; a workspace job has
// an identity row and a source, and reconciling it away because a control plane
// did not name it would delete work the control plane never owned.
//
// The identity store is the same authority `deleteTarget` uses, deliberately:
// two places deciding "is this job Cloud-managed" differently is how one of them
// becomes wrong.
func (d *Daemon) ManagedReleaseJobs(ctx context.Context) ([]string, error) {
	m := release.Manager{DataDir: d.cfg.DataDir}
	root, err := m.Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		// No release root yet is an empty answer, not a failure: a runtime that
		// has never been given a release manages nothing.
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		job := e.Name()
		if !e.IsDir() || job == release.ActiveDirName {
			continue
		}
		if _, err := identity.Parse(job); err != nil {
			continue
		}
		// A row means the job's SOURCE exists in the jobs directory, so it is the
		// workspace's and must not be reconciled away.
		if _, rowErr := d.ident.Store().Instance(ctx, identity.ID(job)); rowErr == nil {
			continue
		} else if !errors.Is(rowErr, identity.ErrNotFound) {
			// A store fault is not "no row": treating it as one would hand an
			// operator's job to the control plane to delete.
			return nil, rowErr
		}
		out = append(out, job)
	}
	sort.Strings(out)
	return out, nil
}
