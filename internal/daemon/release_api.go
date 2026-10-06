package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/tkoizumi/otter/internal/api"
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
	digest = strings.TrimSpace(digest)
	if digest == "" {
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
