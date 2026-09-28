package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/api"
	"github.com/tkoizumi/otter/internal/runs"
)

// TestListRunsResolvesAReferenceToTheDurableIdentity is the daemon half of the
// `otter runs <integration>` fix.
//
// A run records the durable identity, which is a UUID, while an operator types
// the manifest label. Before this, the API treated the filter as a raw key, so
// `otter runs counter` and `GET /v1/runs?integration_id=counter` both returned
// nothing. The label, the id, the `id:` form and a path must all name the same
// integration, and rows written before identities existed -- keyed by the
// label -- must stay in the answer.
func TestListRunsResolvesAReferenceToTheDurableIdentity(t *testing.T) {
	root := t.TempDir()
	writeIntegration(t, root, "counter", `
version: 1
name: counter
entrypoint: main.py
`, `print("hi")`)

	d := newDaemonUnreleased(t, root, "", nil, nil)
	id := runtimeID(t, d, "counter")
	if id == "counter" {
		t.Fatalf("the test needs a durable identity distinct from the label, got %q", id)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	for _, run := range []*runs.Run{
		{ID: "modern", IntegrationID: id, IntegrationName: "counter", TriggerType: runs.TriggerManual, Status: runs.StatusSucceeded, Attempt: 1, CreatedAt: now},
		// A row from before migration 0003: no name, keyed by the label.
		{ID: "legacy", IntegrationID: "counter", TriggerType: runs.TriggerManual, Status: runs.StatusSucceeded, Attempt: 1, CreatedAt: now.Add(-time.Minute)},
		{ID: "other", IntegrationID: "something-else", IntegrationName: "something-else", TriggerType: runs.TriggerManual, Status: runs.StatusSucceeded, Attempt: 1, CreatedAt: now},
	} {
		if err := d.runs.Create(ctx, run); err != nil {
			t.Fatalf("create %s: %v", run.ID, err)
		}
	}

	for _, ref := range []string{"counter", id, "id:" + id, filepath.Join(root, "counter")} {
		list, err := d.ListRuns(ctx, runs.Filter{IntegrationID: ref})
		if err != nil {
			t.Fatalf("ListRuns(%q): %v", ref, err)
		}
		if len(list) != 2 {
			t.Errorf("ListRuns(%q) returned %d runs, want the modern and legacy rows", ref, len(list))
		}
		for _, run := range list {
			if run.ID == "other" {
				t.Errorf("ListRuns(%q) leaked another integration's run", ref)
			}
		}
	}

	// No filter is still every run.
	all, err := d.ListRuns(ctx, runs.Filter{})
	if err != nil {
		t.Fatalf("ListRuns(all): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered list returned %d runs, want 3", len(all))
	}

	// A reference that names nothing is an answer, not an empty page.
	if _, err := d.ListRuns(ctx, runs.Filter{IntegrationID: "nope"}); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("ListRuns(unknown) = %v, want api.ErrNotFound", err)
	}
}
