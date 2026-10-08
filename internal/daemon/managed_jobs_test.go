package daemon

import (
	"context"
	"slices"
	"testing"
)

// ManagedReleaseJobs is the discriminator an agent must not have to guess at:
// the jobs this runtime holds ONLY in its release store, which is what a control
// plane owns on a pooled tenant. A job whose source lives in the jobs directory
// is the workspace's and must never appear here, because an agent reconciles a
// deletion from this list.
func TestManagedReleaseJobsReportsReleasedOnlyJobs(t *testing.T) {
	d, jobID, _, _ := releasedOnlyFixture(t)

	managed, err := d.ManagedReleaseJobs(context.Background())
	if err != nil {
		t.Fatalf("managed release jobs: %v", err)
	}
	if !slices.Contains(managed, jobID) {
		t.Fatalf("the released-only job %s is not reported as managed: %v", jobID, managed)
	}
}

// The property the directive names explicitly: a job whose source is in the
// jobs directory is NOT Cloud-managed, even when a release exists for it. An
// agent that removed it would delete work the control plane never owned.
func TestManagedReleaseJobsExcludesAWorkspaceJob(t *testing.T) {
	jobsDir := t.TempDir()
	source := writeJob(t, jobsDir, "sync", syncManifest, `print("from the workspace")`)
	dataDir := t.TempDir()
	d := newDaemonWith(t, jobsDir, dataDir, nil, nil, false)
	defer closeUnstarted(t, d)
	ctx := context.Background()

	views := d.ListJobs(ctx)
	if len(views) != 1 || views[0].ID == "" {
		t.Fatalf("the workspace fixture did not register a job: %+v", views)
	}
	// A release for the SAME id, so the release store is not what makes the
	// difference -- the identity row is.
	installCloudRelease(t, dataDir, views[0].ID, source)
	if _, err := d.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	managed, err := d.ManagedReleaseJobs(ctx)
	if err != nil {
		t.Fatalf("managed release jobs: %v", err)
	}
	if slices.Contains(managed, views[0].ID) {
		t.Fatalf("a workspace job was reported as Cloud-managed: %v", managed)
	}
}

// A release staged but never activated has no active link, so discovery does not
// surface it -- and it would be clutter nobody cleans. It is still the control
// plane's, so it is reported and can be reconciled away.
func TestManagedReleaseJobsIncludesAStagedOnlyJob(t *testing.T) {
	stage := t.TempDir()
	source := writeJob(t, stage, "sync", syncManifest, `print("from the release")`)

	dataDir := t.TempDir()
	const jobID = "0a5c3d21-9f24-4a5e-8f1e-6b7c2d9e4a10"
	stageCloudRelease(t, dataDir, jobID, source) // staged, deliberately not active

	jobsDir := t.TempDir()
	d := newDaemonWith(t, jobsDir, dataDir, nil, nil, false)
	defer closeUnstarted(t, d)

	managed, err := d.ManagedReleaseJobs(context.Background())
	if err != nil {
		t.Fatalf("managed release jobs: %v", err)
	}
	if !slices.Contains(managed, jobID) {
		t.Fatalf("a staged-only release was not reported as managed: %v", managed)
	}
}

// A runtime that has never been given a release manages nothing, and says so
// with an empty list rather than an error.
func TestManagedReleaseJobsOnAFreshRuntimeIsEmpty(t *testing.T) {
	d := newDaemonWith(t, t.TempDir(), t.TempDir(), nil, nil, false)
	defer closeUnstarted(t, d)

	managed, err := d.ManagedReleaseJobs(context.Background())
	if err != nil {
		t.Fatalf("managed release jobs: %v", err)
	}
	if len(managed) != 0 {
		t.Fatalf("a fresh runtime reported managed jobs: %v", managed)
	}
}
