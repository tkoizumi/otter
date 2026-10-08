package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
)

// releasedTarget resolves a job that exists only in the release store: a
// directory under the release root named by the durable job id.
//
// It is the delete-side counterpart of observeReleases, and it deliberately
// does NOT require an ACTIVE release. observeReleases skips a job with no active
// link because it is not runnable, but a delete must still be able to remove
// staged releases for a job id, or a job whose active link went away would be
// undeletable and its snapshots would sit in the store forever.
//
// The directory name is the identity, exactly as it is for discovery: it is what
// runs, state and releases are keyed by, and it has to be a usable identity
// before any of that can be true.
func (d *Daemon) releasedTarget(job string) (identity.Instance, bool) {
	id, err := identity.Parse(job)
	if err != nil {
		return identity.Instance{}, false
	}
	manager := release.Manager{DataDir: d.cfg.DataDir}
	root, err := manager.Root()
	if err != nil {
		return identity.Instance{}, false
	}
	info, err := os.Stat(filepath.Join(root, job))
	if err != nil || !info.IsDir() {
		return identity.Instance{}, false
	}

	// The label and the snapshot directory are filled in when the release is
	// active, because that is when they are knowable; a job with staged-only
	// releases is still a legitimate delete target and reports its id.
	name := job
	canonical := ""
	if sourceDir, _, ok, err := release.ActiveSourceDir(d.cfg.DataDir, job); err == nil && ok {
		canonical = sourceDir
		if m, err := config.LoadAndValidate(filepath.Join(sourceDir, config.ManifestFileName)); err == nil && m.Name != "" {
			name = m.Name
		}
	}
	return releasedInstance(id, name, canonical), true
}

// releasedJob is a job this runtime can run because a release for it is on
// disk, even though no source for it sits in the jobs directory.
type releasedJob struct {
	job      *config.Job
	instance identity.Instance
}

// observeReleases reports the jobs the release store can serve, so discovery
// can register them alongside the jobs directory.
//
// The failure this exists to fix: a pooled tenant runtime runs with
// `--jobs /workspace`, and /workspace is EMPTY because the customer runs
// `otter deploy --cloud` from their laptop, so the job's source never lands
// there. The deploy itself succeeded -- `release_installed` and
// `release_activated` were both logged -- but the job was then not runnable:
// `jobs_discovered total:0`, `GET /v1/jobs` was empty, and
// `POST /v1/jobs/sync/runs` answered 404 `job "sync": not found`. Discovery
// walked the jobs directory and nothing else, so the release store -- the only
// place the job existed -- was never consulted.
//
// The release root is the authority for which job owns a release: it is what
// jobForDigest already scans, and each directory is named by the durable job id
// (Metadata.Job). This reads it the same way and resolves the code through
// release.ActiveSourceDir, the same function a submission uses, so the path
// discovery reports is the path a run will execute.
func (d *Daemon) observeReleases() []releasedJob {
	manager := release.Manager{DataDir: d.cfg.DataDir}
	root, err := manager.Root()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		// A runtime that has never been given a release has no release store;
		// that is the normal state for a plain workspace, not a failure. Any
		// other read fault means releases that ARE on disk are invisible, so
		// it is worth a line in the daemon log.
		if !errors.Is(err, fs.ErrNotExist) {
			d.log.Warn("release_scan_failed", "root", root, "error", err.Error())
		}
		return nil
	}

	out := make([]releasedJob, 0, len(entries))
	for _, entry := range entries {
		job := entry.Name()
		if !entry.IsDir() || job == release.ActiveDirName {
			continue
		}

		// The directory name is the durable identity, so it has to be usable
		// as one before state, runs and releases are keyed off it. A name that
		// cannot be an identity is reported and skipped rather than coerced.
		id, err := identity.Parse(job)
		if err != nil {
			d.log.Warn("release_scan_skipped", "job", job, "error", err.Error())
			continue
		}

		sourceDir, _, ok, err := release.ActiveSourceDir(d.cfg.DataDir, job)
		if err != nil {
			d.log.Warn("release_scan_skipped", "job", job, "error", err.Error())
			continue
		}
		if !ok {
			// Releases are staged but none is active: nothing is being served
			// yet, exactly like a workspace job that has never been released.
			continue
		}

		manifestPath := filepath.Join(sourceDir, config.ManifestFileName)
		m, err := config.LoadAndValidate(manifestPath)
		if err != nil {
			// Present but broken is reported rather than silently absent: the
			// difference decides whether an operator edits a manifest or goes
			// looking for a missing release.
			out = append(out, releasedJob{
				job: &config.Job{
					ID:           job,
					Name:         job,
					Dir:          sourceDir,
					ManifestPath: manifestPath,
					Error:        "active release is invalid: " + err.Error(),
				},
				instance: releasedInstance(id, job, sourceDir),
			})
			continue
		}

		out = append(out, releasedJob{
			job: &config.Job{
				ID:           job,
				Name:         m.Name,
				Dir:          sourceDir,
				ManifestPath: manifestPath,
				Manifest:     m,
				Valid:        true,
			},
			instance: releasedInstance(id, m.Name, sourceDir),
		})
	}
	return out
}

// releasedInstance is the in-memory identity a released job runs under.
//
// It is DERIVED on every pass, never persisted, and both halves of that are
// deliberate. The release directory already names the durable identity, so
// writing a registry row (and, worse, a marker into an immutable snapshot)
// would create a second authority for a fact the release root already records.
// Re-deriving it is also what makes the registration survive a restart: there
// is no process memory to lose, and the release store on disk is re-read by
// every start and reload.
func releasedInstance(id identity.ID, name, sourceDir string) identity.Instance {
	return identity.Instance{
		ID:            id,
		Name:          name,
		CanonicalPath: sourceDir,
		Status:        identity.StatusActive,
		// The first generation of an identity this runtime never minted.
		// Submission records this generation on the run and the worker fences
		// against the registry's, and both come from this function, so a
		// released run cannot be refused by a mismatch it never had.
		Generation: 1,
		CreatedAt:  time.Now().UTC(),
	}
}
