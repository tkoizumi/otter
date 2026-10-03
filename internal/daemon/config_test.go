package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tkoizumi/otter/internal/api"
)

// Configuration is pinned at submission, so a change afterwards does not move
// work that was already accepted, and a retry keeps the values its parent was
// accepted with -- the same property releases have.
func TestSubmitPinsJobConfigAndRetryKeepsIt(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "plain")

	view, err := d.SetJobConfig(ctx, "id:"+id, json.RawMessage(`{"dataset":42}`), "test")
	if err != nil {
		t.Fatalf("set config: %v", err)
	}
	if !view.Changed || view.ConfigVersion == "" {
		t.Fatalf("set view = %+v, want a new version", view)
	}
	if string(view.Values) != `{"dataset":42}` {
		t.Fatalf("stored values = %s", view.Values)
	}

	runID, err := d.SubmitRun(ctx, "id:"+id, api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	run, err := d.runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.ConfigVersion != view.ConfigVersion {
		t.Fatalf("run config_version = %q, want %q", run.ConfigVersion, view.ConfigVersion)
	}
	if got := d.pinnedConfig(run); got != `{"dataset":42}` {
		t.Fatalf("pinned config = %q, want the accepted values", got)
	}

	// A later change writes a new version and leaves the accepted run alone.
	second, err := d.SetJobConfig(ctx, "id:"+id, json.RawMessage(`{"dataset":43}`), "test")
	if err != nil {
		t.Fatalf("second set: %v", err)
	}
	if second.ConfigVersion == view.ConfigVersion {
		t.Fatal("a change reused the previous version")
	}
	if got := d.pinnedConfig(run); got != `{"dataset":42}` {
		t.Fatalf("an accepted run moved to %q", got)
	}

	// A retry is the same accepted work, so it keeps the same version.
	entry, ok := d.reg.get(id)
	if !ok {
		t.Fatal("job is not registered")
	}
	plan := d.planRetry(run, entry.Manifest)
	if plan.run.ConfigVersion != view.ConfigVersion {
		t.Fatalf("retry config_version = %q, want %q", plan.run.ConfigVersion, view.ConfigVersion)
	}
}

// A job with no configuration yields an empty object, and pinning records
// nothing, so the child reads `{}` rather than failing.
func TestSubmitWithoutConfiguration(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "plain")

	view, err := d.GetJobConfig(ctx, "id:"+id)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if view.ConfigVersion != "" || string(view.Values) != "{}" {
		t.Fatalf("unconfigured view = %+v, want an empty object and no version", view)
	}

	runID, err := d.SubmitRun(ctx, "id:"+id, api.TriggerPayload{Type: api.TriggerManual})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	run, err := d.runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.ConfigVersion != "" {
		t.Fatalf("run config_version = %q, want empty", run.ConfigVersion)
	}
	if got := d.pinnedConfig(run); got != "" {
		t.Fatalf("pinned config = %q, want empty", got)
	}
}

// Deleting an identity purges its configuration and every version of it.
func TestDeleteJobPurgesConfiguration(t *testing.T) {
	root := t.TempDir()
	writeJob(t, root, "plain", noCronJob, noopPython)

	d := newDaemon(t, root, "", nil, nil)
	ctx := context.Background()
	id := runtimeID(t, d, "plain")

	if _, err := d.SetJobConfig(ctx, "id:"+id, json.RawMessage(`{"a":1}`), "test"); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if _, err := d.DeleteJob(ctx, "id:"+id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := d.configs.Current(ctx, id); err != nil || ok {
		t.Fatalf("configuration survived the delete: ok=%v err=%v", ok, err)
	}
}
