package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
)

// seedDeleteRun writes one run for a job without going through the queue, so a
// test can put the job in a known in-flight state (queued, running, retrying) or
// a terminal one and then ask for a delete.
func seedDeleteRun(t *testing.T, d *Daemon, jobID, runID string, status runs.Status) {
	t.Helper()
	if err := d.runs.Create(context.Background(), &runs.Run{
		ID:          runID,
		JobID:       jobID,
		JobName:     "sync",
		TriggerType: runs.TriggerManual,
		Status:      status,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed %s run: %v", status, err)
	}
}

// releasedOnlyFixture stages and activates a release for a job outside the
// runtime's jobs directory, then builds a daemon whose jobs directory is EMPTY:
// the pooled-runtime state `otter deploy --cloud` leaves behind.
func releasedOnlyFixture(t *testing.T) (*Daemon, string, release.Metadata, string) {
	t.Helper()

	stage := t.TempDir()
	source := writeJob(t, stage, "sync", syncManifest, `print("from the release")`)

	dataDir := t.TempDir()
	const jobID = "bc19af64-a5b7-4dba-bfc7-21f3114e7476"
	meta := installCloudRelease(t, dataDir, jobID, source)

	jobsDir := t.TempDir()
	d := newDaemonWith(t, jobsDir, dataDir, nil, nil, false)
	return d, jobID, meta, dataDir
}

// The gap this closes: a job that exists ONLY as a release (no identity row, an
// empty jobs directory) can be deleted. The delete must remove every release
// including the active link, and must leave the job out of the listing and out
// of a fresh discovery pass.
func TestDeleteAReleasedOnlyJobRemovesItsReleasesAndRegistration(t *testing.T) {
	d, jobID, meta, dataDir := releasedOnlyFixture(t)
	ctx := context.Background()

	manager := release.Manager{DataDir: dataDir}
	root, err := manager.Root()
	if err != nil {
		t.Fatal(err)
	}
	activePath, err := manager.ActivePath(jobID)
	if err != nil {
		t.Fatal(err)
	}

	// The fixture is what the test claims: discovered, released, no identity row.
	if _, ok := d.GetJob(jobID); !ok {
		t.Fatalf("the fixture job %s was not discovered from the release store", jobID)
	}
	if _, err := d.ident.Store().Instance(ctx, identity.MustParse(jobID)); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the fixture should have no identity row, got %v", err)
	}

	// Deleted the way an operator would name it -- by the manifest label -- so
	// the label branch of reference resolution reaches the released-only path.
	view, err := d.DeleteJob(ctx, "sync")
	if err != nil {
		t.Fatalf("delete a released-only job: %v", err)
	}
	if !view.Deleted || view.ID != jobID {
		t.Fatalf("deleted view = %+v, want deleted %s", view, jobID)
	}
	if view.Note == "" {
		t.Error("the delete reported no note; the released-only scope must be stated")
	}
	if strings.Contains(view.Note, "source files were left in place") {
		t.Errorf("the released-only note claims workspace semantics: %q", view.Note)
	}

	// Gone from the runtime view...
	if _, ok := d.GetJob(jobID); ok {
		t.Error("the job is still resolvable after its delete")
	}
	for _, v := range d.ListJobs(ctx) {
		if v.ID == jobID {
			t.Fatalf("the job is still listed after its delete: %+v", v)
		}
	}

	// ...and from a FRESH discovery pass, which rebuilds the derived instance
	// from disk. The derivation is in-memory per pass, so this proves the
	// release store has nothing left to derive it from.
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload after delete: %v", err)
	}
	if _, ok := d.GetJob(jobID); ok {
		t.Error("a fresh discovery pass resurrected the deleted job")
	}
	for _, v := range d.ListJobs(ctx) {
		if v.ID == jobID {
			t.Fatalf("a fresh discovery pass re-listed the deleted job: %+v", v)
		}
	}

	// THE STORE, not the response: the job directory is gone, the active link
	// is gone, and the digest no longer resolves.
	if _, err := os.Stat(filepath.Join(root, jobID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("release directory for %s still exists (err=%v)", jobID, err)
	}
	if _, err := os.Lstat(activePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the active link for %s still exists (err=%v)", jobID, err)
	}
	if _, ok, err := manager.Active(jobID); err != nil || ok {
		t.Errorf("the store still reports an active release: ok=%v err=%v", ok, err)
	}
	if _, err := manager.Metadata(jobID, meta.Digest); err == nil {
		t.Errorf("release %s still resolves in the store after the delete", meta.Digest)
	}

	// And a restart over the same data directory does not bring it back.
	closeUnstarted(t, d)
	restarted := newDaemonWith(t, t.TempDir(), dataDir, nil, nil, false)
	defer closeUnstarted(t, restarted)
	if _, ok := restarted.GetJob(jobID); ok {
		t.Error("a restarted runtime rediscovered the deleted job from the release store")
	}

	// A second delete reports not found rather than corrupting anything.
	if _, err := restarted.DeleteJob(ctx, "id:"+jobID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("second delete error = %v, want not found", err)
	}
}

// The regression guard: a job whose source IS in the jobs directory and which
// has an identity row still deletes through the identity path. It keeps its
// source, gets its tombstone, suppresses its path, and still has its releases
// purged.
func TestDeleteAWorkspaceJobStillUsesTheIdentityPath(t *testing.T) {
	root := t.TempDir()
	dir := writeJob(t, root, "counter", manifestFor("counter"), noopPython)

	// newDaemon stages and activates a release keyed by the minted identity, so
	// this job has both an identity row and a release.
	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "counter")

	releaseRoot, err := release.Manager{DataDir: d.cfg.DataDir}.Root()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(releaseRoot, id)); err != nil {
		t.Fatalf("the fixture has no releases for %s: %v", id, err)
	}

	view, err := d.DeleteJob(ctx, "id:"+id)
	if err != nil {
		t.Fatalf("delete a workspace job: %v", err)
	}
	if !view.Deleted || view.ID != id {
		t.Fatalf("deleted view = %+v", view)
	}
	if view.Path != mustCanonicalPath(t, dir) {
		t.Errorf("path = %q, want the source directory %q", view.Path, mustCanonicalPath(t, dir))
	}

	// The identity path ran: a deleted tombstone and a suppressed path.
	inst, err := d.ident.Store().Instance(ctx, identity.MustParse(id))
	if err != nil || inst.Status != identity.StatusDeleted {
		t.Fatalf("instance = %+v (%v), want deleted", inst, err)
	}
	rec, found, err := d.ident.Store().PathRecord(ctx, mustCanonicalPath(t, dir))
	if err != nil || !found || !rec.Suppressed {
		t.Fatalf("path record = %+v found=%v (%v), want suppressed", rec, found, err)
	}
	// Source is left alone...
	if _, err := os.Stat(filepath.Join(dir, "otter.yaml")); err != nil {
		t.Fatalf("delete removed source files: %v", err)
	}
	// ...and the releases are still purged, exactly as the other shape does.
	if _, err := os.Stat(filepath.Join(releaseRoot, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("releases for a deleted workspace job still exist (err=%v)", err)
	}
}

// An unknown job is a clean not-found: no panic, no half-delete, and the store
// is untouched.
func TestDeleteAnUnknownJobErrorsCleanly(t *testing.T) {
	d, jobID, _, dataDir := releasedOnlyFixture(t)
	ctx := context.Background()

	for _, ref := range []string{"id:no-such-job", "no-such-job"} {
		t.Run(ref, func(t *testing.T) {
			if _, err := d.DeleteJob(ctx, ref); !errors.Is(err, api.ErrNotFound) {
				t.Fatalf("delete %q error = %v, want not found", ref, err)
			}
		})
	}

	// The real job, whose id also names a release-store directory, survives.
	if _, ok := d.GetJob(jobID); !ok {
		t.Error("an unknown delete removed the released job")
	}
	root, err := release.Manager{DataDir: dataDir}.Root()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, jobID)); err != nil {
		t.Errorf("an unknown delete touched the release store: %v", err)
	}
}

// The first irreversible action on the agent channel is guarded: a job with a
// run that has not finished is REFUSED, with the reason named, and nothing at
// all is removed.
func TestDeleteRefusesAJobWithAnInFlightRunAndRemovesNothing(t *testing.T) {
	for _, status := range []runs.Status{runs.StatusQueued, runs.StatusRunning, runs.StatusRetrying} {
		t.Run(string(status), func(t *testing.T) {
			d, jobID, meta, dataDir := releasedOnlyFixture(t)
			ctx := context.Background()
			seedDeleteRun(t, d, jobID, "run-inflight", status)

			manager := release.Manager{DataDir: dataDir}
			root, err := manager.Root()
			if err != nil {
				t.Fatal(err)
			}
			activePath, err := manager.ActivePath(jobID)
			if err != nil {
				t.Fatal(err)
			}

			_, err = d.DeleteJob(ctx, "id:"+jobID)
			if !errors.Is(err, api.ErrConflict) {
				t.Fatalf("delete with a %s run error = %v, want conflict", status, err)
			}
			if !strings.Contains(err.Error(), string(status)) || !strings.Contains(err.Error(), "cancel") {
				t.Errorf("the refusal must name the status and the fix: %v", err)
			}

			// NOTHING was removed: the job, the release store and the run all
			// survive, because the check runs before the first store write.
			if _, ok := d.GetJob(jobID); !ok {
				t.Error("the refused delete dropped the job from the listing")
			}
			if _, err := os.Stat(filepath.Join(root, jobID)); err != nil {
				t.Errorf("the refused delete removed releases: %v", err)
			}
			if _, err := os.Lstat(activePath); err != nil {
				t.Errorf("the refused delete removed the active link: %v", err)
			}
			if _, err := manager.Metadata(jobID, meta.Digest); err != nil {
				t.Errorf("the refused delete removed release %s: %v", meta.Digest, err)
			}
			if _, err := d.runs.Get(ctx, "run-inflight"); err != nil {
				t.Errorf("the refused delete removed the run: %v", err)
			}
		})
	}
}

// A FINISHED run does not block a delete, and the purge takes the run history
// with it. That is the other half of the in-flight rule: the guard is about
// work still in progress, not about history existing.
func TestDeleteAllowsAFinishedRunAndPurgesItsHistory(t *testing.T) {
	d, jobID, _, _ := releasedOnlyFixture(t)
	ctx := context.Background()
	seedDeleteRun(t, d, jobID, "run-done", runs.StatusSucceeded)

	if _, err := d.DeleteJob(ctx, "id:"+jobID); err != nil {
		t.Fatalf("delete a job whose only run finished: %v", err)
	}
	if _, err := d.runs.Get(ctx, "run-done"); !errors.Is(err, runs.ErrNotFound) {
		t.Errorf("run history survived the purge: %v", err)
	}
}
