package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/config"
	"github.com/tkoizumi/otter/internal/identity"
	"github.com/tkoizumi/otter/internal/release"
	"github.com/tkoizumi/otter/internal/runs"
)

// syncManifest is the job from the measured failure: a release named `sync`
// whose source never lands in the runtime's jobs directory.
const syncManifest = `
version: 1
name: sync
entrypoint: main.py
timeout: 30
`

// stageCloudRelease stages a release for jobID from a source tree OUTSIDE the
// runtime's jobs directory, which is the state `otter deploy --cloud` leaves a
// pooled runtime in: the release lands under the data directory while the
// workspace stays empty. Activation is left to the caller so a test can drive
// it through the runtime's own API.
func stageCloudRelease(t *testing.T, dataDir, jobID, sourceDir string) release.Metadata {
	t.Helper()

	// The release base is the source's parent, so the snapshot places the job
	// at <release>/<name>/otter.yaml. The exact placement is irrelevant to the
	// test; what matters is that the runtime learns it from the release store.
	layout, err := release.Plan(filepath.Dir(sourceDir), sourceDir, nil)
	if err != nil {
		t.Fatalf("plan release: %v", err)
	}
	manager := release.Manager{DataDir: dataDir}
	meta, err := manager.StageWithLayout(jobID, sourceDir, layout, "")
	if err != nil {
		t.Fatalf("stage release: %v", err)
	}
	return meta
}

// installCloudRelease is stageCloudRelease followed by activation.
func installCloudRelease(t *testing.T, dataDir, jobID, sourceDir string) release.Metadata {
	t.Helper()
	meta := stageCloudRelease(t, dataDir, jobID, sourceDir)
	if err := (release.Manager{DataDir: dataDir}).Activate(jobID, meta.Digest); err != nil {
		t.Fatalf("activate release: %v", err)
	}
	return meta
}

// The defect, reproduced: a runtime whose jobs directory is EMPTY but whose
// release store holds an activated release. Before release-store discovery the
// job was in neither GET /v1/jobs nor the registry, so
// `POST /v1/jobs/sync/runs` answered 404 "job \"sync\": not found" even though
// the release was installed and serving.
func TestAReleasedJobWithNoWorkspaceSourceIsRunnable(t *testing.T) {
	stage := t.TempDir()
	source := writeJob(t, stage, "sync", syncManifest, `print("from the release")`)

	dataDir := t.TempDir()
	const jobID = "bc19af64-a5b7-4dba-bfc7-21f3114e7476"
	meta := installCloudRelease(t, dataDir, jobID, source)

	// The customer deployed from their laptop, so nothing was written here.
	jobsDir := t.TempDir()
	d := newDaemonWith(t, jobsDir, dataDir, nil, nil, false)
	ctx := context.Background()

	manager := release.Manager{DataDir: dataDir}
	releaseRoot, err := manager.Root()
	if err != nil {
		t.Fatal(err)
	}
	wantSource, _, ok, err := release.ActiveSourceDir(dataDir, jobID)
	if err != nil || !ok {
		t.Fatalf("the fixture has no active release: ok=%v err=%v", ok, err)
	}

	// Resolvable by durable id...
	byID, ok := d.GetJob(jobID)
	if !ok {
		t.Fatalf("the released job does not resolve by id %s", jobID)
	}
	if byID.Name != "sync" {
		t.Errorf("name = %q, want sync", byID.Name)
	}

	// ...and by the manifest name.
	byName, ok := d.GetJob("sync")
	if !ok {
		t.Fatal("the released job does not resolve by name")
	}
	if byName.ID != jobID {
		t.Errorf("id = %q, want %q", byName.ID, jobID)
	}

	// It appears in the listing, once, with both identifiers.
	listed := 0
	for _, view := range d.ListJobs(ctx) {
		if view.ID == jobID {
			listed++
		}
	}
	if listed != 1 {
		t.Fatalf("the released job is listed %d times, want 1", listed)
	}

	// The path a run would use is INSIDE the installed release tree, never the
	// empty jobs directory. Asserted exactly so a future change cannot silently
	// point a released job back at an empty workspace.
	if byName.Path != wantSource {
		t.Errorf("resolved path = %q, want the active release tree %q", byName.Path, wantSource)
	}
	if !strings.HasPrefix(byName.Path, releaseRoot+string(os.PathSeparator)) {
		t.Errorf("resolved path %q is not inside the release root %q", byName.Path, releaseRoot)
	}

	// Submittable for a run, by id and by name.
	runByID, err := d.SubmitRun(ctx, jobID, api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit by id: %v", err)
	}
	runByName, err := d.SubmitRun(ctx, "sync", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit by name: %v", err)
	}

	detail, err := d.GetRunDetail(ctx, runByID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if detail.Run.ReleaseSourceDir != wantSource {
		t.Errorf("the run is bound to %q, want %q", detail.Run.ReleaseSourceDir, wantSource)
	}
	if detail.Run.ReleaseDigest != meta.Digest {
		t.Errorf("the run is bound to digest %q, want %q", detail.Run.ReleaseDigest, meta.Digest)
	}

	// Executing it runs the release tree's code.
	startDaemon(t, d)
	for _, runID := range []string{runByID, runByName} {
		view := awaitTerminal(t, d, runID)
		if view.Run.Status != runs.StatusSucceeded {
			t.Fatalf("run %s status = %s (%s), want succeeded",
				runID, view.Run.Status, view.Run.ErrorString())
		}
		stdout := strings.Join(logMessages(t, d, runID, runs.StreamStdout), "\n")
		if !strings.Contains(stdout, "from the release") {
			t.Errorf("run %s did not execute the installed release:\n%s", runID, stdout)
		}
	}
}

// The regression guard: discovering the release store must not change a job
// whose source IS in the jobs directory. Its identity, its path and its
// schedules stay exactly as they were, and the release store does not list a
// second copy of it.
func TestWorkspaceJobKeepsItsOwnIdentityPathAndSchedules(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", `
version: 1
name: plain
entrypoint: main.py
timeout: 30
trigger:
  cron: "@every 6h"
`, `print("from the workspace")`)

	// newDaemon stages and activates a release keyed by the identity the
	// workspace scan minted, exactly as `otter release` does.
	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()

	id := runtimeID(t, d, "plain")
	view, ok := d.GetJob("plain")
	if !ok {
		t.Fatal("the workspace job does not resolve")
	}

	canonicalRoot, err := identity.Canonical(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(canonicalRoot, "plain"); view.Path != want {
		t.Errorf("path = %q, want the workspace directory %q", view.Path, want)
	}

	releaseRoot, err := release.Manager{DataDir: d.cfg.DataDir}.Root()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(view.Path, releaseRoot+string(os.PathSeparator)) {
		t.Errorf("a workspace job's path moved into the release tree: %q", view.Path)
	}

	listed := 0
	for _, v := range d.ListJobs(ctx) {
		if v.ID == id {
			listed++
		}
	}
	if listed != 1 {
		t.Errorf("the workspace job is listed %d times, want 1", listed)
	}

	startDaemon(t, d)
	// The scheduler is only armed once the daemon runs, so the trigger is
	// checked after it starts.
	if _, ok := jobNextRun(d, id); !ok {
		t.Error("the workspace job lost its schedule")
	}
	runID, err := d.SubmitRun(ctx, "plain", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	run := awaitTerminal(t, d, runID)
	if run.Run.Status != runs.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", run.Run.Status, run.Run.ErrorString())
	}
	stdout := strings.Join(logMessages(t, d, runID, runs.StreamStdout), "\n")
	if !strings.Contains(stdout, "from the workspace") {
		t.Errorf("the workspace job did not run its released snapshot:\n%s", stdout)
	}
}

// Requirement: registration is driven by the release store on disk, so a
// restart re-derives it. Nothing about the released job is kept in the process
// or written into the identity registry, so there is nothing a restart can
// lose.
func TestAReleasedJobIsRediscoveredAfterARestart(t *testing.T) {
	stage := t.TempDir()
	source := writeJob(t, stage, "sync", syncManifest, noopPython)

	dataDir := t.TempDir()
	const jobID = "bc19af64-a5b7-4dba-bfc7-21f3114e7476"
	installCloudRelease(t, dataDir, jobID, source)
	jobsDir := t.TempDir()

	first := newDaemonWith(t, jobsDir, dataDir, nil, nil, false)
	if _, ok := first.GetJob("sync"); !ok {
		t.Fatal("the first process did not discover the released job")
	}
	closeUnstarted(t, first)

	second := newDaemonWith(t, jobsDir, dataDir, nil, nil, false)
	defer closeUnstarted(t, second)

	view, ok := second.GetJob("sync")
	if !ok {
		t.Fatal("the restarted process lost the released job")
	}
	if view.ID != jobID {
		t.Errorf("id after restart = %q, want %q", view.ID, jobID)
	}
	if _, ok := second.GetJob(jobID); !ok {
		t.Errorf("the restarted process does not resolve id %s", jobID)
	}
}

// The production path, end to end in one process: a gated pooled runtime, an
// install, then activation through the runtime's own API. The job must become
// runnable at activation, not at the next restart, because the agent exits
// maintenance and reports the deploy right after this call.
func TestActivatingAReleaseRegistersTheJobWithoutARestart(t *testing.T) {
	stage := t.TempDir()
	source := writeJob(t, stage, "sync", syncManifest, `print("from the release")`)

	dataDir := t.TempDir()
	const jobID = "bc19af64-a5b7-4dba-bfc7-21f3114e7476"
	meta := stageCloudRelease(t, dataDir, jobID, source)

	jobsDir := t.TempDir()
	d := newDaemonWith(t, jobsDir, dataDir, nil, func(cfg *config.DaemonConfig) {
		cfg.StartInMaintenance = true
	}, false)
	ctx := context.Background()

	// Installed but not activated is not yet a job this runtime serves.
	if _, ok := d.GetJob("sync"); ok {
		t.Fatal("a staged-but-inactive release registered a job")
	}

	if _, err := d.ActivateRelease(ctx, meta.Digest); err != nil {
		t.Fatalf("activate release: %v", err)
	}
	view, ok := d.GetJob("sync")
	if !ok {
		t.Fatal("activation did not register the job until a restart")
	}
	if view.ID != jobID {
		t.Errorf("id = %q, want %q", view.ID, jobID)
	}

	if _, err := d.ExitMaintenance(ctx); err != nil {
		t.Fatalf("exit maintenance: %v", err)
	}
	startDaemon(t, d)
	runID, err := d.SubmitRun(ctx, "sync", api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit run: %v", err)
	}
	run := awaitTerminal(t, d, runID)
	if run.Run.Status != runs.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", run.Run.Status, run.Run.ErrorString())
	}
}
